"""A pre-warmed interpreter inside the jail, forked per run.

Cold start is the tax every serverless platform pays and mostly hides. Ours is
about 90ms of `sudo` + `unshare` + `setpriv` + interpreter startup + importing
numpy, on every single run. That is not billed — the clock starts when the
program does — but it is latency the caller feels on every call, and for a
service whose unit of work is milliseconds it is the difference between "run
this in a loop" and "batch it up first".

So the expensive half happens once. This process starts inside the jail, with
the network namespace and the unprivileged uid already applied, imports the
interpreter's heavy modules, and then waits. Each run is a `fork()`: the child
inherits the warm heap copy-on-write, applies the run's own environment, and
executes the program. ~10ms instead of ~90ms, with the same isolation, because
the isolation was established before the pool existed.

What a forked child can and cannot do is worth being precise about:

- It cannot see another run's data: there is none in here. The forkserver never
  executes user code, so its memory is the interpreter, numpy, and this file.
- It runs as the same uid as every other run, exactly as before. Forking does
  not weaken that; it just skips re-establishing it.
- It can signal the forkserver — same uid — which kills the pool slot. The host
  notices the socket closing and starts another. That is a denial of service
  against a process whose only job is to be replaceable.

The protocol is one JSON object per connection, newline-terminated, in each
direction: a job in, `{"pid": N}` immediately, `{"exit_code": N}` when it is
over.
"""

from __future__ import annotations

import json
import os
import socket
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import bootstrap  # noqa: E402


def _preimport() -> None:
    """Pay for the imports every run would otherwise pay for.

    numpy is the expensive one (~60ms) and the one nearly every numeric
    program wants. mojosub is ours and is needed to dispatch a cached kernel.
    Anything that fails to import here simply is not warm.
    """
    for module in ("numpy", "math", "json", "random", "itertools",
                   "collections", "statistics", "mojosub"):
        try:
            __import__(module)
        except Exception:  # noqa: BLE001 - warmth is optional, correctness is not
            pass


def _run_job(job: dict) -> int:
    """In the forked child: become the run, then execute it."""
    os.setsid()
    work = job["work"]
    os.chdir(work)

    # The environment is per-run (trace path, deadline, cache view), so the
    # inherited one is replaced wholesale rather than updated.
    os.environ.clear()
    os.environ.update(job["env"])

    stdin_path = job.get("stdin_path")
    fd_in = os.open(stdin_path if stdin_path and os.path.exists(stdin_path)
                    else os.devnull, os.O_RDONLY)
    fd_out = os.open(job["stdout"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    fd_err = os.open(job["stderr"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    os.dup2(fd_in, 0)
    os.dup2(fd_out, 1)
    os.dup2(fd_err, 2)
    for fd in (fd_in, fd_out, fd_err):
        if fd > 2:
            os.close(fd)
    sys.stdin = open(0, "r", closefd=False)
    sys.stdout = open(1, "w", buffering=1, closefd=False)
    sys.stderr = open(2, "w", buffering=1, closefd=False)

    sys.argv = [job["program"], *job.get("args", [])]
    # A fresh child inherits the parent's rlimits; the per-run ones are set
    # here because they depend on the run.
    _apply_limits(job.get("limits") or {})
    return bootstrap.run_program(job["program"])


def _apply_limits(limits: dict) -> None:
    import resource

    for name, value in limits.items():
        what = getattr(resource, name, None)
        if what is None:
            continue
        try:
            resource.setrlimit(what, (value, value))
        except (ValueError, OSError):
            pass


def _report(job: dict, exc: BaseException) -> None:
    """Write why a warm start failed where the caller will see it."""
    import traceback

    try:
        with open(job["stderr"], "a", encoding="utf-8") as fh:
            fh.write("mojojojo: warm start failed\n")
            traceback.print_exception(type(exc), exc, exc.__traceback__, file=fh)
    except Exception:  # noqa: BLE001
        pass


def serve(path: str) -> None:
    _preimport()
    if os.path.exists(path):
        os.unlink(path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    # The host connects as the service user, so the socket has to be reachable
    # by it. Unguessable name plus a non-listable directory is what keeps
    # another sandboxed run from finding it; the mode cannot do that job.
    os.chmod(path, 0o666)
    server.listen(8)
    # The host waits for this line before sending work.
    print("ready", flush=True)

    while True:
        try:
            conn, _ = server.accept()
        except OSError:
            break
        try:
            _handle(conn)
        except Exception as exc:  # noqa: BLE001 - one bad job must not end the pool
            try:
                conn.sendall(json.dumps({"error": str(exc)}).encode() + b"\n")
            except OSError:
                pass
        finally:
            conn.close()


def _handle(conn: socket.socket) -> None:
    data = b""
    while not data.endswith(b"\n"):
        chunk = conn.recv(65536)
        if not chunk:
            return
        data += chunk
    job = json.loads(data)

    started = time.perf_counter()
    pid = os.fork()
    if pid == 0:
        code = 1
        try:
            code = _run_job(job)
        except BaseException as exc:  # noqa: BLE001 - never unwind into the loop
            # Said out loud, into the run's own stderr file. Swallowing it made
            # a broken warm path indistinguishable from a program that printed
            # nothing and exited 1 — which is how it stayed broken.
            code = 1
            _report(job, exc)
        finally:
            os._exit(code)

    conn.sendall(json.dumps({"pid": pid}).encode() + b"\n")
    _, status = os.waitpid(pid, 0)
    exit_code = os.waitstatus_to_exitcode(status)
    conn.sendall(json.dumps({
        "exit_code": exit_code,
        "wall_ms": round((time.perf_counter() - started) * 1000.0, 3),
    }).encode() + b"\n")


if __name__ == "__main__":
    serve(sys.argv[1])
