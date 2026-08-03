"""Run one user program under the tightest isolation this host allows.

Three layers, in order of how much they actually protect:

1. `mojojail` (installed to /usr/local/bin by deploy.sh, root-owned, invoked
   through a single NOPASSWD sudoers rule). It puts the program in a fresh
   network namespace — no network at all — drops to the unprivileged `mojorun`
   user, sets no-new-privs, and enforces the hard timeout with `timeout -s
   KILL`. This is the layer that matters.
2. rlimits set in the child before exec: address space, CPU seconds, file
   size, process count, open files. These bound a program that is merely
   greedy rather than hostile.
3. A scrubbed environment and a per-run scratch directory that is deleted
   afterwards.

Without mojojail — a dev laptop, a fresh checkout — everything still runs, but
`isolated` comes back false and the caller is expected to say so out loud
rather than quietly accept untrusted code.
"""

from __future__ import annotations

import contextlib
import errno
import itertools
import json
import os
import re
import resource
import shutil
import signal
import subprocess
import threading
import time
import uuid

import hostcompile
import mojoprog
import pool

DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
RUN_ROOT = os.path.join(DATA_ROOT, "runs")
JAIL = os.environ.get("MOJOJOJO_JAIL", "/usr/local/bin/mojojail")
MOJOSUB_PATH = os.environ.get("MOJOJOJO_MOJOSUB", "/nvme0n1-disk/code/mojosub")
RUNNER_DIR = os.path.dirname(os.path.abspath(__file__))

MAX_OUTPUT = int(os.environ.get("MOJOJOJO_MAX_OUTPUT", str(256 * 1024)))
MEM_BYTES = int(os.environ.get("MOJOJOJO_MEM_BYTES", str(2 * 1024 * 1024 * 1024)))
MAX_FILE_BYTES = int(os.environ.get("MOJOJOJO_MAX_FILE_BYTES", str(64 * 1024 * 1024)))
MAX_PROCS = int(os.environ.get("MOJOJOJO_MAX_PROCS", "256"))

# What a caller is allowed to put in the run's environment: ordinary names,
# none of ours, and nothing that changes how code is found or loaded.
_CALLER_ENV = re.compile(r"^[A-Za-z][A-Za-z0-9_]{0,63}$")
_RESERVED_ENV = frozenset({
    "PATH", "HOME", "TMPDIR", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
    "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONEXECUTABLE",
    "PYTHONWARNINGS", "PYTHONINSPECT", "BASH_ENV", "ENV", "IFS",
})


# The warm phase (compiles finishing after the user's code returns) is not
# billed, but it does need to fit inside the kill deadline.
WARM_MS = float(os.environ.get("MOJOJOJO_WARM_MS", "8000"))
# How long a run's compile thread waits for the host to build the unit it asked
# for. The wait is off the program's own thread, so what it buys is the rest of
# *this* run going native instead of only the next one. 0 disables it.
COMPILE_WAIT_MS = float(os.environ.get("MOJOJOJO_COMPILE_WAIT_MS", "8000"))

_mojo_version: str | None = None


def jail_available() -> bool:
    return os.path.exists(JAIL) and shutil.which("sudo") is not None


def mojo_binary() -> str:
    """The host compiler path. Never handed to the sandbox — see mojo_stub."""
    return hostcompile.mojo_binary()


def modular_home() -> str:
    return hostcompile.modular_home()


class Result(dict):
    """Plain dict; named for readability at call sites."""


# The user's program, timed from inside the jail. Everything before this line
# runs — sudo, the namespaces, the mounts — is ours and is not billed, and
# there is no interpreter in a compiled program to write a trace file the way
# bootstrap.py does. `$0` is the binary and `$@` its arguments; the exit code is
# carried back out so a program that fails still fails.
_TIMED = ('_s=$(date +%s%N); "$0" "$@"; _c=$?; _e=$(date +%s%N); '
          'echo "$_s $_e" > "$MOJOJOJO_RUN_TRACE"; exit $_c')


def _workdir() -> tuple[str, str]:
    """A fresh run directory. See the comment in `run` about its mode."""
    run_id = uuid.uuid4().hex
    work = os.path.join(RUN_ROOT, run_id)
    os.makedirs(work, mode=0o777, exist_ok=True)
    os.chmod(work, 0o777)
    os.chmod(RUN_ROOT, 0o733)
    return run_id, work


def _merge_caller_env(child_env: dict, env_extra: dict | None) -> dict:
    """Caller variables go *under* ours, never over.

    Merging the other way round let a request set PATH=/evil/bin — which,
    because the sandbox command is built from that environment, broke the jail
    launch outright.
    """
    caller = {}
    for key, value in (env_extra or {}).items():
        if not _CALLER_ENV.match(key) or key in _RESERVED_ENV or key in child_env:
            continue
        caller[key] = str(value)[:4096]
    merged = {**caller, **child_env}
    return {k: v for k, v in merged.items() if v != ""}


def run_binary(*, binary: str, stdin: str = "", args=None,
               timeout_ms: int = 10_000, env_extra: dict | None = None,
               gpu: bool = False) -> Result:
    """Run a compiled program: no interpreter, no accelerator, no compiler.

    Same jail, same limits, same clock as a Python run — the only difference is
    that there is nothing between the caller's code and the CPU. The binary was
    built on the host (see mojoprog) and is hardlinked into the run directory,
    so the shared cache stays unreachable from inside.
    """
    args = [str(a) for a in (args or [])]
    timeout_s = max(1, min(int(round(timeout_ms / 1000.0)), 900))
    run_id, work = _workdir()
    staged = mojoprog.stage(binary, work)
    trace_path = os.path.join(work, "mojo-trace")

    child_env = {
        "PATH": "/usr/bin:/bin",
        "HOME": work,
        "TMPDIR": work,
        "MOJOJOJO_RUN_TRACE": trace_path,
        # A compiled program has no OpenBLAS to talk out of turn, but a Mojo
        # runtime does size its thread pool from the machine, and one tenant's
        # cost must not depend on how many cores another tenant is using.
        "MOJO_NUM_THREADS": "1",
        "OMP_NUM_THREADS": "1",
    }
    if gpu:
        for var in ("CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES", "LD_LIBRARY_PATH"):
            if os.environ.get(var):
                child_env[var] = os.environ[var]
    child_env = _merge_caller_env(child_env, env_extra)

    inner = ["/bin/sh", "-c", _TIMED, staged, *args]
    hard_s = min(900, timeout_s + 2)
    isolated = jail_available()
    if isolated:
        assignments = [f"{k}={v}" for k, v in child_env.items()]
        cmd = jail_command(hard_s, work, [], assignments, inner)
    else:
        cmd = ["timeout", "-s", "KILL", str(hard_s), *inner]

    out_path = os.path.join(work, "stdout")
    err_path = os.path.join(work, "stderr")
    started = time.perf_counter()
    timed_out = False
    try:
        with open(out_path, "wb") as out, open(err_path, "wb") as err:
            proc = subprocess.Popen(
                cmd, cwd=work, env=child_env, stdout=out, stderr=err,
                stdin=subprocess.PIPE, close_fds=True,
                # Affinity is inherited through sudo, unshare and exec, so it
                # is set here, once, on the process that becomes the program.
                preexec_fn=_binary_limits(hard_s, isolated, _cpu_slice()))
            try:
                proc.communicate(stdin.encode()[:1 << 20], timeout=hard_s + 3)
            except subprocess.TimeoutExpired:
                timed_out = True
                _reap(proc, run_id)
                proc.wait(timeout=5)
            exit_code = proc.returncode or 0
        if exit_code in (124, 137, -9, -24):
            timed_out = True
        wall_ms = (time.perf_counter() - started) * 1000.0
        return _result(exit_code, timed_out, out_path, err_path, wall_ms,
                       _read_binary_trace(trace_path), isolated, run_id, 0,
                       False)
    finally:
        shutil.rmtree(work, ignore_errors=True)


# How many cores one compiled program may use. The Mojo runtime starts a
# worker thread per visible CPU before `main` does anything: on this 72-core
# box that is 50ms of the ~55ms it takes to print "hi", and it is 50ms every
# tenant pays for every run. Bounding the affinity cuts it to ~30ms and matches
# what Python runs already get — one tenant's cost must not depend on how big
# the machine happens to be.
# Eight, not one: unlike a Python run, a compiled program is expected to use
# the machine, and billing is wall clock — capping a `parallelize` kernel at one
# core would make it slower *and* dearer. Eight concurrent runs at eight cores
# is the whole box, which is the point at which they start costing each other.
MOJO_CPUS = int(os.environ.get("MOJOJOJO_MOJO_CPUS", "8"))
_cpu_turn = itertools.count()


def _cpu_slice() -> set[int]:
    """A rotating block of cores, so concurrent runs do not all land on 0-3."""
    try:
        available = sorted(os.sched_getaffinity(0))
    except (AttributeError, OSError):
        return set()
    if not available or MOJO_CPUS <= 0 or MOJO_CPUS >= len(available):
        return set()
    start = (next(_cpu_turn) * MOJO_CPUS) % len(available)
    picked = [available[(start + i) % len(available)] for i in range(MOJO_CPUS)]
    return set(picked)


def _binary_limits(timeout_s: int, isolated: bool, cpus: set[int]):
    base = _limits(timeout_s, isolated)

    def apply() -> None:
        base()
        if cpus:
            try:
                os.sched_setaffinity(0, cpus)
            except OSError:
                pass  # an affinity we cannot set is not a reason to refuse

    return apply


def _read_binary_trace(path: str) -> dict:
    """The program's own wall clock, as measured inside the jail."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            start, end = fh.read().split()[:2]
        return {"wall_ms": round((int(end) - int(start)) / 1e6, 3)}
    except (OSError, ValueError):
        return {}


def run(*, python: str, code: str, stdin: str = "", args=None,
        timeout_ms: int = 10_000, accel: bool = True, gpu: bool = False,
        env_extra: dict | None = None, mojo_paths=None,
        mojo_aliases: dict | None = None) -> Result:
    """Execute `code` and return its output plus a measured cost."""
    args = [str(a) for a in (args or [])]
    timeout_s = max(1, min(int(round(timeout_ms / 1000.0)), 900))
    # The sandbox user is not us and has to write in the run directory. Every
    # run shares the uid, so what keeps one run out of another's is that
    # RUN_ROOT is not listable (0733) and this name is 32 random hex digits.
    run_id, work = _workdir()

    program = os.path.join(work, "program.py")
    trace_path = os.path.join(work, "trace.json")
    _write(program, code, 0o644)

    cache_dir = os.path.join(work, "cache")
    requests_dir = os.path.join(work, "compile-requests")
    os.makedirs(requests_dir, mode=0o777, exist_ok=True)
    os.chmod(requests_dir, 0o777)
    seeded = 0
    if accel:
        # Seeded with this program's own kernels only — see hostcompile.seed.
        seeded = hostcompile.seed(cache_dir, code)

    child_env = {
        "PATH": "/usr/bin:/bin",
        "HOME": work,
        "TMPDIR": work,
        # The ported Mojo libraries the run may use go on the path here; they
        # are read-only repo directories and carry their own prebuilt kernels.
        "PYTHONPATH": os.pathsep.join([RUNNER_DIR, MOJOSUB_PATH, *(mojo_paths or [])]),
        "PYTHONDONTWRITEBYTECODE": "1",
        "PYTHONUNBUFFERED": "1",
        # One thread unless the program asks otherwise: billing is per
        # millisecond of wall clock, and a numpy import that quietly grabs 32
        # cores makes one tenant's cost depend on another tenant's load.
        "OPENBLAS_NUM_THREADS": "1",
        "OMP_NUM_THREADS": "1",
        "MKL_NUM_THREADS": "1",
        "MOJOJOJO_TRACE": trace_path,
        "MOJOJOJO_ACCEL": "1" if accel else "0",
        "MOJOJOJO_TIMEOUT_MS": str(timeout_ms),
        # Applied by bootstrap.py *inside* the jail. Inheriting them through
        # `sudo` does not work — PAM resets the target user's limits — so the
        # cold path was silently unlimited until this was measured.
        "MOJOJOJO_LIMITS": json.dumps({
            "RLIMIT_DATA": MEM_BYTES,
            "RLIMIT_FSIZE": MAX_FILE_BYTES,
            "RLIMIT_NPROC": MAX_PROCS,
            "RLIMIT_NOFILE": 1024,
            "RLIMIT_CORE": 0,
            "RLIMIT_CPU": min(900, timeout_s + int(WARM_MS / 1000.0) + 4),
        }),
        "MOJOJOJO_WARM_MS": str(int(WARM_MS)),
        # A private view of the shared compile cache: symlinks in, writes stay
        # here and die with the run. See hostcompile.py for why the sandbox is
        # never given write access to the real thing.
        "MOJOSUB_CACHE": cache_dir,
        # There is no compiler in here. This stub queues a build request and
        # fails, and the host compiles it afterwards.
        "MOJOSUB_MOJO": f"{python} {os.path.join(RUNNER_DIR, 'mojo_stub.py')}",
        "MOJOJOJO_COMPILE_REQUESTS": requests_dir,
        "MOJOJOJO_COMPILE_WAIT_MS": str(int(COMPILE_WAIT_MS)),
        "MOJOJOJO_MOJO_VERSION": host_mojo_version(),
        # Only set when the caller opted in; see bootstrap._install_mojo_aliases.
        "MOJOJOJO_MOJO_ALIASES": (json.dumps(mojo_aliases) if mojo_aliases else ""),
    }
    if gpu:
        for var in ("CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES", "LD_LIBRARY_PATH"):
            if os.environ.get(var):
                child_env[var] = os.environ[var]
    # A caller may add variables; it may never replace ours.
    child_env = _merge_caller_env(child_env, env_extra)

    inner = [python, os.path.join(RUNNER_DIR, "bootstrap.py"), program, *args]
    # bootstrap.py enforces the user's deadline itself; this outer one only has
    # to survive the unbilled warm phase and then act as the backstop for a
    # program that has wedged the interpreter past its own alarm.
    hard_s = min(900, timeout_s + int(WARM_MS / 1000.0) + 2)
    isolated = jail_available()
    if isolated:
        # sudo scrubs the environment (env_reset), which would silently strip
        # PYTHONPATH, the trace path, and the compiler location — the program
        # would still run, just with no acceleration and no timing. Rebuild the
        # environment inside the jail instead of fighting sudoers over it.
        assignments = [f"{k}={v}" for k, v in child_env.items()]
        cmd = jail_command(hard_s, work, mojo_paths, assignments, inner)
    else:
        cmd = ["timeout", "-s", "KILL", str(hard_s), *inner]

    out_path = os.path.join(work, "stdout")
    err_path = os.path.join(work, "stderr")
    started = time.perf_counter()
    timed_out = False
    exit_code = 0
    warm_start = False
    # The host fills this run's compile requests while it is still running, so
    # a kernel's first sighting can go native part-way through rather than only
    # on the next run. Stopped before promotion, in the same finally.
    watcher = (hostcompile.Watcher(requests_dir)
               if accel and COMPILE_WAIT_MS > 0 else None)
    watch = contextlib.ExitStack()
    if watcher is not None:
        watch.enter_context(watcher)
    # One try/finally around both paths: the warm path used to return from
    # inside its own try, which skipped promotion and leaked the run directory.
    try:
        try:
            # A warm slot skips sudo, unshare, setpriv, interpreter startup and
            # the numpy import — about 90ms of the ~110ms that is not the
            # program. Anything unexpected about the slot falls through to the
            # cold path below, so this can only ever make a run faster.
            slot = pool.acquire(python) if isolated else None
            if slot is not None:
                stdin_path = os.path.join(work, "stdin")
                _write(stdin_path, stdin[: 1 << 20], 0o644)
                job = {
                    "program": program, "work": work, "env": child_env,
                    "args": args, "stdout": out_path, "stderr": err_path,
                    "stdin_path": stdin_path,
                    "limits": {"RLIMIT_DATA": MEM_BYTES,
                               "RLIMIT_FSIZE": MAX_FILE_BYTES,
                               "RLIMIT_NPROC": MAX_PROCS,
                               "RLIMIT_CPU": hard_s + 2,
                               "RLIMIT_CORE": 0},
                }
                done = pool.submit(slot, job, timeout_s=hard_s + 3)
                pool.release(slot, healthy=not (done or {}).get("unhealthy"))
                # `done is None` means the job never started, so the cold path
                # below is a retry and not a second execution.
                if done is not None and "exit_code" in done:
                    warm_start = True
                    exit_code = int(done["exit_code"])
                    timed_out = (bool(done.get("timed_out"))
                                 or exit_code in (124, 137, -9, -24))
                    wall_ms = (time.perf_counter() - started) * 1000.0
                    return _result(exit_code, timed_out, out_path, err_path,
                                   wall_ms, _read_trace(trace_path), isolated,
                                   run_id, seeded, warm_start)
        except Exception:  # noqa: BLE001 - the cold path is always available
            pass

        with open(out_path, "wb") as out, open(err_path, "wb") as err:
            proc = subprocess.Popen(
                cmd, cwd=work, env=child_env, stdout=out, stderr=err,
                stdin=subprocess.PIPE, preexec_fn=_limits(hard_s, isolated),
                close_fds=True,
            )
            try:
                proc.communicate(stdin.encode()[:1 << 20], timeout=hard_s + 3)
            except subprocess.TimeoutExpired:
                timed_out = True
                _reap(proc, run_id)
                proc.wait(timeout=5)
            exit_code = proc.returncode or 0
        # 124 is bootstrap's own deadline or `timeout`'s; 137/-9 is the KILL
        # backstop; -24 is RLIMIT_CPU.
        if exit_code in (124, 137, -9, -24):
            timed_out = True
        wall_ms = (time.perf_counter() - started) * 1000.0
        return _result(exit_code, timed_out, out_path, err_path, wall_ms,
                       _read_trace(trace_path), isolated, run_id, seeded,
                       warm_start)
    finally:
        watch.close()
        # The program text goes with it: what the compiled units get indexed
        # against is the program, not whatever this run happened to learn.
        _finish(work, cache_dir, requests_dir, code, watcher)


def jail_command(hard_s: int, work: str, mojo_paths, assignments, inner) -> list:
    """The jailed command line for one run.

    `--ports` names only the Mojo library ports this program imports. Binding
    all ~175 of them costs three mount syscalls each — 2.3 seconds of kernel
    time on the front of every cold run, for directories the program has no
    reference to. A warm pool slot still asks for all of them, once, because it
    builds its mounts before it knows whose run it will serve.
    """
    ports = ",".join(sorted({
        os.path.basename(os.path.dirname(p)) for p in (mojo_paths or [])}))
    return ["sudo", "-n", JAIL, str(hard_s), "--work", work,
            *(["--ports", ports] if ports else []),
            "/usr/bin/env", "-i", *assignments, *inner]


def _result(exit_code, timed_out, out_path, err_path, wall_ms, trace,
            isolated, run_id, seeded, warm_start) -> Result:
    """One shape for both paths, warm and cold, so callers cannot tell them
    apart except by `warm_start`."""
    return Result(
        ok=(exit_code == 0 and not timed_out),
        exit_code=exit_code,
        timed_out=timed_out,
        stdout=_read_capped(out_path),
        stderr=_read_capped(err_path) + ("\nmojojojo: timed out\n" if timed_out else ""),
        wall_ms=round(wall_ms, 3),
        cpu_ms=trace.get("cpu_ms", 0.0),
        user_ms=trace.get("wall_ms", 0.0),
        # Everything between the user's last statement and this answer: the
        # unbilled warm phase, plus whatever the sandbox took to start and
        # stop. Reported because a run that takes ten seconds to return an
        # answer it had after three is a bug you cannot see from wall_ms.
        warm_ms=trace.get("warm_ms", 0.0),
        accelerated=trace.get("accelerated", []),
        accel=trace.get("accel", {}),
        mojo_aliases=trace.get("mojo_aliases", []),
        cache_seeded=seeded,
        isolated=isolated,
        warm_start=warm_start,
        run_id=run_id,
    )


def _finish(work: str, cache_dir: str, requests_dir: str, code: str = "",
            watcher=None) -> None:
    """Promote what the run learned, then delete it.

    Off the caller's clock in a thread: compiling is seconds and it buys the
    *next* run's speed. If there is nothing to promote — the overwhelmingly
    common case — the directory goes immediately.
    """
    # Only real build requests count. The directory also holds the run's own
    # bookkeeping — the `stop` marker, a failed build's `.err` — and treating
    # those as work leaves the run directory behind for a program that never
    # asked for a compile at all.
    queued = any(n.endswith(".mojo") for n in _listdir(requests_dir))
    has_work = queued or _has_real_files(os.path.join(cache_dir, "sigs"))
    if not has_work:
        shutil.rmtree(work, ignore_errors=True)
        return

    def worker() -> None:
        try:
            if watcher is not None:
                watcher.join()
            hostcompile.promote(cache_dir, requests_dir, code)
        finally:
            shutil.rmtree(work, ignore_errors=True)

    threading.Thread(target=worker, daemon=False, name="promote").start()


def _listdir(path: str) -> list[str]:
    try:
        return os.listdir(path)
    except OSError:
        return []


def _has_real_files(path: str) -> bool:
    """True when the run wrote something here rather than reading a seed."""
    return any(not os.path.islink(os.path.join(path, n)) for n in _listdir(path))


def host_mojo_version() -> str:
    """The version string the host compiler will report.

    It goes into mojosub's cache key, so the sandbox has to hash the same
    string the host will — otherwise nothing the host compiles is ever found
    by the run that asked for it.
    """
    global _mojo_version
    if _mojo_version is None:
        _mojo_version = hostcompile.mojo_version(default="mojojojo-host-compiler")
    return _mojo_version


def _limits(timeout_s: int, isolated: bool):
    def apply() -> None:
        os.setsid()
        soft_cpu = timeout_s + 2
        _setrlimit(resource.RLIMIT_CPU, (soft_cpu, soft_cpu + 2))
        # RLIMIT_DATA, not RLIMIT_AS: since Linux 4.7 it covers anonymous
        # mappings, and unlike AS it does not count the enormous virtual
        # reservations OpenBLAS makes per thread — capping AS at anything
        # sane makes `import numpy` itself fail.
        _setrlimit(resource.RLIMIT_DATA, (MEM_BYTES, MEM_BYTES))
        _setrlimit(resource.RLIMIT_FSIZE, (MAX_FILE_BYTES, MAX_FILE_BYTES))
        if isolated:
            # RLIMIT_NPROC counts every process owned by the uid, so it is
            # only meaningful once the jail has switched us to the dedicated
            # sandbox user. Applied unjailed it counts the whole dev box and
            # the first fork fails.
            _setrlimit(resource.RLIMIT_NPROC, (MAX_PROCS, MAX_PROCS))
        _setrlimit(resource.RLIMIT_NOFILE, (1024, 1024))
        _setrlimit(resource.RLIMIT_CORE, (0, 0))
    return apply


def _setrlimit(what: int, limits: tuple[int, int]) -> None:
    try:
        resource.setrlimit(what, limits)
    except (ValueError, OSError):
        pass  # a limit we cannot lower is not a reason to refuse the run


def _reap(proc: subprocess.Popen, run_id: str) -> None:
    """Kill a run that outlived its deadline, jailed or not."""
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except OSError as exc:
        if exc.errno not in (errno.ESRCH, errno.EPERM):
            raise
    if jail_available():
        # The program runs as `mojorun`, so we cannot signal it directly; the
        # jail script matches on the run id, which appears in its argv.
        subprocess.run(["sudo", "-n", JAIL, "--reap-run", run_id],
                       capture_output=True, timeout=10, check=False)


def _write(path: str, data: str, mode: int) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(data)
    os.chmod(path, mode)


def _read_capped(path: str) -> str:
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as fh:
            if size > MAX_OUTPUT:
                head = fh.read(MAX_OUTPUT)
                return head.decode("utf-8", "replace") + \
                    f"\n... [truncated {size - MAX_OUTPUT} bytes]"
            return fh.read().decode("utf-8", "replace")
    except OSError:
        return ""


def _read_trace(path: str) -> dict:
    import json
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return {}
