"""In-process entrypoint for one user program. Runs inside the sandbox.

argv: bootstrap.py <program.py> [user args...]

The program is compiled here rather than by `python program.py` so that the
AST can be rewritten first (see accel.accelerate) and so the run trace — what
compiled, how long the user's code actually spent on CPU — can be written out
without the user program having to cooperate. The trace goes to the file named
by MOJOJOJO_TRACE, never to stdout, so stdout stays exactly what the program
printed.
"""

from __future__ import annotations

import json
import linecache
import os
import resource
import signal
import sys
import time
import traceback

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import accel  # noqa: E402


class _Deadline(BaseException):
    """Raised in the main thread when the run's time limit expires."""


def _arm_deadline() -> None:
    seconds = float(os.environ.get("MOJOJOJO_TIMEOUT_MS", "0")) / 1000.0
    if seconds <= 0:
        return

    def fire(_signum, _frame):
        raise _Deadline()

    signal.signal(signal.SIGALRM, fire)
    signal.setitimer(signal.ITIMER_REAL, seconds)


def _install_mojo_aliases() -> list[str]:
    """Make `import PIL` resolve to the Mojo port, when the caller asked.

    Off unless MOJOJOJO_MOJO_ALIASES carries a map, because a port covers a
    subset of the original's API and a silent substitution turns a speedup into
    an AttributeError nobody can explain. The finder is deliberately narrow: it
    maps only the exact names in the map and their submodules, and it defers to
    the real module if the port cannot be imported at all.
    """
    raw = os.environ.get("MOJOJOJO_MOJO_ALIASES", "")
    if not raw:
        return []
    try:
        aliases = json.loads(raw)
    except ValueError:
        return []
    if not isinstance(aliases, dict) or not aliases:
        return []

    import importlib
    import importlib.abc
    import importlib.machinery

    class _AliasFinder(importlib.abc.MetaPathFinder):
        def find_spec(self, fullname, path=None, target=None):
            root = fullname.split(".")[0]
            replacement = aliases.get(root)
            if not replacement or fullname.startswith(replacement):
                return None
            target_name = replacement + fullname[len(root):]
            try:
                module = importlib.import_module(target_name)
            except ImportError:
                return None  # the port does not cover this submodule
            sys.modules[fullname] = module
            return importlib.machinery.ModuleSpec(fullname, None)

    sys.meta_path.insert(0, _AliasFinder())
    return sorted(aliases)


def _apply_limits() -> None:
    """Apply this run's resource limits, inside the jail.

    They cannot be inherited: `sudo` runs the jail, and PAM resets the target
    user's limits on the way through, so anything set before the exec is gone
    by the time user code starts. This is the first thing that happens in the
    child, before a single line of the program is parsed.
    """
    raw = os.environ.get("MOJOJOJO_LIMITS", "")
    if not raw:
        return
    try:
        limits = json.loads(raw)
    except ValueError:
        return
    for name, value in limits.items():
        what = getattr(resource, name, None)
        if what is None:
            continue
        try:
            soft, hard = resource.getrlimit(what)
            # Never raise a limit, only lower it: a warm slot may already be
            # more restricted than this run asked for.
            target = value if hard in (resource.RLIM_INFINITY,) else min(value, hard)
            resource.setrlimit(what, (target, target))
        except (ValueError, OSError):
            pass


def _print_user_traceback(exc: BaseException) -> None:
    """Print the user's traceback without our own frame on top of it.

    The first frame is always bootstrap's `exec(code, ns)`, which tells the
    caller nothing except that we exist.
    """
    tb = exc.__traceback__
    traceback.print_exception(type(exc), exc, tb.tb_next if tb else None)


def _cpu_ms() -> float:
    me = resource.getrusage(resource.RUSAGE_SELF)
    kids = resource.getrusage(resource.RUSAGE_CHILDREN)
    return (me.ru_utime + me.ru_stime + kids.ru_utime + kids.ru_stime) * 1000.0


def main() -> int:
    program_path = sys.argv[1]
    sys.argv = [program_path] + sys.argv[2:]
    return run_program(program_path)


def run_program(program_path: str) -> int:
    """Execute one program. Also the entry point the fork server uses."""
    with open(program_path, "r", encoding="utf-8") as fh:
        source = fh.read()

    trace = {"accelerated": [], "accel": {}, "error": None}
    exit_code = 0
    started = time.perf_counter()

    # Tracebacks quote source lines by looking the filename up in linecache.
    # Registering it there is what makes a user's error message show their
    # code instead of a bare "<mojojojo>, line 12".
    linecache.cache["<mojojojo>"] = (len(source), None, source.splitlines(True),
                                     "<mojojojo>")
    _apply_limits()
    # A warm slot imported mojosub before this run's environment existed.
    accel.rebind_environment()
    trace["mojo_aliases"] = _install_mojo_aliases()
    accel.set_source(source)

    try:
        import ast

        tree = ast.parse(source, filename="<mojojojo>")
        trace["accelerated"] = accel.accelerate(tree)
        code = compile(tree, "<mojojojo>", "exec")
    except SyntaxError:
        traceback.print_exc()
        _write_trace(trace, started, 1)
        return 1

    ns: dict = {"__name__": "__main__", "__file__": program_path}
    ns.update(accel.namespace())
    # The user's deadline is enforced here rather than by the outer `timeout`,
    # so that a program which runs out of time still gets a clean traceback,
    # still reports what it printed, and still lets the warm phase below put
    # its compiled kernels in the cache. The outer kill is the backstop.
    _arm_deadline()
    try:
        exec(code, ns)  # noqa: S102 - executing user code is the product
    except SystemExit as exc:
        exit_code = int(exc.code or 0)
    except _Deadline:
        print("mojojojo: time limit exceeded", file=sys.stderr)
        trace["error"] = "timeout"
        exit_code = 124
    except BaseException as exc:  # noqa: BLE001 - user tracebacks belong on stderr
        _print_user_traceback(exc)
        trace["error"] = "exception"
        exit_code = 1
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)

    # Stop the billing clock here, before the warm phase below.
    trace["wall_ms"] = round((time.perf_counter() - started) * 1000.0, 3)
    # And release any compile thread still waiting on the host: a library that
    # arrives now has no call left to serve, and the caller would be sitting
    # through the wait. The host finishes and caches it regardless.
    _release_compile_waiters()
    trace["accel"] = accel.stats(ns)
    _write_trace(trace, started, exit_code)

    # Unbilled: let any in-flight compile finish so the cache keeps it.
    warm_ms = accel.drain(ns)
    if warm_ms:
        trace["warm_ms"] = warm_ms
        trace["accel"] = accel.stats(ns)
        _write_trace(trace, started, exit_code)
    return exit_code


def _release_compile_waiters() -> None:
    requests = os.environ.get("MOJOJOJO_COMPILE_REQUESTS", "")
    if not requests:
        return
    try:
        open(os.path.join(requests, "stop"), "w").close()
    except OSError:
        pass


def _write_trace(trace: dict, started: float, exit_code: int) -> None:
    trace.setdefault("wall_ms", round((time.perf_counter() - started) * 1000.0, 3))
    trace["cpu_ms"] = round(_cpu_ms(), 3)
    trace["exit_code"] = exit_code
    path = os.environ.get("MOJOJOJO_TRACE")
    if not path:
        return
    try:
        sys.stdout.flush()
        sys.stderr.flush()
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(trace, fh)
    except OSError:
        pass


if __name__ == "__main__":
    sys.exit(main())
