"""Run a whole program written in Mojo.

The accelerated Python path (accel.py, mojo_stub.py, hostcompile.py) compiles
*part* of a program — the numeric functions mojosub can take — and leaves the
rest on CPython. This is the other end of the same idea: the caller writes Mojo
themselves and we run all of it, with no interpreter in the way at all.

The trust boundary does not move. The sandbox has no compiler, so the build
happens here, on the host, as the service user, from the exact source the
caller sent; what goes into the jail is a binary and no toolchain. The result
is content-addressed exactly like a compiled kernel, so the second person to
run a program is not charged the wait — and neither was the first, because a
build is on our clock.

A build that fails is cached too. A program that does not compile is a program
somebody is about to submit again after editing one line, and answering the
second attempt in a millisecond is worth one small file.
"""

from __future__ import annotations

import contextlib
import hashlib
import os
import posixpath
import signal
import shutil
import subprocess
import tempfile
import threading
import time

import cachestore
import hostcompile

SHARED = hostcompile.SHARED
# A build is bounded: the compiler is ours and a caller who submits pathological
# Mojo must not be able to hold a core for ever.
BUILD_TIMEOUT_S = float(os.environ.get("MOJOJOJO_MOJO_BUILD_TIMEOUT", "180"))
OPT = os.environ.get("MOJOJOJO_MOJO_OPT", "3")
MAX_LOG = 8000
MAX_PROJECT_FILES = 32
MAX_PROJECT_BYTES = 512 * 1024
MAX_PROJECT_PATH_BYTES = 1024
MAX_PROJECT_COMPONENT_BYTES = 255

_stats = {"built": 0, "failed": 0, "cache_hits": 0, "build_seconds": 0.0}
_lock = threading.Lock()


def stats() -> dict:
    with _lock:
        return dict(_stats)


def available() -> bool:
    return bool(hostcompile.mojo_binary())


def _project(source: str, files: dict[str, str] | None = None,
             entrypoint: str | None = None) -> tuple[dict[str, str], str]:
    """Validate and normalize the source tree before it reaches the host FS."""
    if files is None:
        if not isinstance(source, str) or not source.strip():
            raise ValueError("code is required")
        return {"program.mojo": source}, "program.mojo"
    if not isinstance(files, dict) or len(files) > MAX_PROJECT_FILES:
        raise ValueError(f"a Mojo project may contain at most {MAX_PROJECT_FILES} files")
    project: dict[str, str] = {}
    total = 0
    for name, text in files.items():
        if not isinstance(name, str) or not isinstance(text, str):
            raise ValueError("Mojo project files must map paths to source strings")
        if "\x00" in name or "\\" in name or name.startswith("/") or posixpath.normpath(name) != name or \
                name in ("", ".", "..") or name.startswith("../") or not name.endswith(".mojo"):
            raise ValueError(f"unsafe Mojo project path: {name!r}")
        try:
            path_size = len(name.encode("utf-8"))
            component_sizes = [len(part.encode("utf-8")) for part in name.split("/")]
            source_size = len(text.encode("utf-8"))
        except UnicodeEncodeError as exc:
            raise ValueError("Mojo project contains an invalid Unicode surrogate") from exc
        if (path_size > MAX_PROJECT_PATH_BYTES
                or any(size > MAX_PROJECT_COMPONENT_BYTES for size in component_sizes)):
            raise ValueError(f"Mojo project path is too long: {name!r}")
        total += source_size
        if total > MAX_PROJECT_BYTES:
            raise ValueError(f"Mojo project exceeds {MAX_PROJECT_BYTES} bytes")
        project[name] = text
    entrypoint = entrypoint or "main.mojo"
    if entrypoint not in project:
        raise ValueError(f"entrypoint {entrypoint!r} is not in files")
    return project, entrypoint


def key_for(source: str, files: dict[str, str] | None = None,
            entrypoint: str | None = None) -> str:
    """Content address for a whole program.

    The same three things a compiled kernel is keyed on: the source, the
    compiler that will build it, and the CPU it will be built for — `mojo
    build` targets the host by default and the cache is synced across rented
    workers, so a binary built where AVX-512 exists must not be handed to a
    machine without it.
    """
    h = hashlib.sha256()
    project, entrypoint = _project(source, files, entrypoint)
    h.update(b"mojoprog|v2|")
    h.update(hostcompile.mojo_version(default="mojojojo-host-compiler").encode())
    h.update(f"|O{OPT}|".encode())
    h.update(_host_target().encode())
    h.update(entrypoint.encode())
    for name in sorted(project):
        h.update(b"\0")
        h.update(name.encode())
        h.update(b"\0")
        h.update(project[name].encode())
    return h.hexdigest()[:32]


def _host_target() -> str:
    try:
        return hostcompile._mojosub().cache.host_target()
    except Exception:  # noqa: BLE001 - the coarse identity still separates archs
        return os.uname().machine


def binary_path(key: str) -> str:
    return os.path.join(SHARED, key + ".bin")


def _error_path(key: str) -> str:
    return os.path.join(SHARED, key + ".bin.err")


def build(source: str, files: dict[str, str] | None = None,
          entrypoint: str | None = None) -> dict:
    """Compile `source`, or say why it will not compile.

    Returns {"ok", "binary", "log", "build_ms", "cached"}. Never raises: a
    program that does not build is an ordinary answer, not an incident.
    """
    project, entrypoint = _project(source, files, entrypoint)
    key = key_for(source, files, entrypoint)
    binary = binary_path(key)
    hit = _cached(key, binary)
    if hit is not None:
        with _lock:
            _stats["cache_hits"] += 1
        return hit

    with _build_lock(key):
        # Somebody else may have built it while we waited for the lock.
        hit = _cached(key, binary)
        if hit is not None:
            with _lock:
                _stats["cache_hits"] += 1
            return hit
        started = time.perf_counter()
        try:
            ok, log = _compile(project, entrypoint, binary)
        except hostcompile.CompileBusy as exc:
            # Capacity pressure is transient. In particular, do not write it
            # to the negative cache under this program's content hash.
            return {"ok": False, "binary": "", "log": f"mojojojo: {exc}",
                    "build_ms": 0.0, "cached": False, "transient": True}
        elapsed = (time.perf_counter() - started) * 1000.0
        with _lock:
            _stats["built" if ok else "failed"] += 1
            _stats["build_seconds"] += elapsed / 1000.0
        if ok:
            cachestore.publish(os.path.basename(binary))
        else:
            _write(_error_path(key), log)
        return {"ok": ok, "binary": binary if ok else "", "log": log,
                "build_ms": round(elapsed, 3), "cached": False}


def _cached(key: str, binary: str) -> dict | None:
    if os.path.exists(binary):
        os.utime(binary, None)  # eviction is least-recently-used
        return {"ok": True, "binary": binary, "log": "", "build_ms": 0.0,
                "cached": True}
    if cachestore.restore(os.path.basename(binary)):
        return {"ok": True, "binary": binary, "log": "", "build_ms": 0.0,
                "cached": True}
    log = _read(_error_path(key))
    if log:
        return {"ok": False, "binary": "", "log": log, "build_ms": 0.0,
                "cached": True}
    return None


_locks: dict[str, list] = {}


@contextlib.contextmanager
def _build_lock(key: str):
    """One compiler per program, without retaining every key forever."""
    with _lock:
        entry = _locks.setdefault(key, [threading.Lock(), 0])
        entry[1] += 1
    lock = entry[0]
    lock.acquire()
    try:
        yield
    finally:
        lock.release()
        with _lock:
            entry[1] -= 1
            if entry[1] == 0 and _locks.get(key) is entry:
                del _locks[key]


def _compile(project: dict[str, str], entrypoint: str,
             binary: str) -> tuple[bool, str]:
    mojo = hostcompile.mojo_binary()
    if not mojo:
        return False, "mojojojo: no Mojo toolchain on this worker"
    os.makedirs(SHARED, exist_ok=True)
    env = hostcompile.compile_env()
    # Must match the resolved compiler or it starts and then cannot find `std`.
    home = hostcompile.modular_home()
    if home:
        env["MODULAR_HOME"] = home
    with tempfile.TemporaryDirectory(dir=SHARED) as tmp:
        src = os.path.join(tmp, *entrypoint.split("/"))
        out = os.path.join(tmp, "program")
        for name, source in project.items():
            target = os.path.join(tmp, *name.split("/"))
            os.makedirs(os.path.dirname(target), exist_ok=True)
            _write(target, source)
        try:
            # Shared with accelerated-Python unit builds: a flood of distinct
            # sources must queue, not start enough compiler processes to make
            # every build (and every running tenant) slower.
            with hostcompile.compiler_slot():
                cachestore.enforce_reserve()
                proc = _invoke_compiler(
                    [mojo, "build", f"--optimization-level={OPT}", src, "-o", out],
                    env=env, cwd=tmp)
        except TimeoutError:
            return False, f"mojojojo: the build did not finish in {BUILD_TIMEOUT_S:.0f}s"
        if proc.returncode != 0 or not os.path.exists(out):
            return False, _diagnostics(proc, tmp)
        os.chmod(out, 0o755)
        os.replace(out, binary)
        # The source next to the binary, under the same name the cache store
        # uses for units, so an evicted binary can be rebuilt from the archive.
        if len(project) == 1:
            _write(binary + ".mojo", next(iter(project.values())))
    return True, ""


def _invoke_compiler(argv: list[str], *, env: dict, cwd: str):
    """Run a bounded compiler and reap its whole process group on timeout."""
    proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, cwd=cwd, start_new_session=True)
    try:
        stdout, stderr = proc.communicate(timeout=BUILD_TIMEOUT_S)
    except subprocess.TimeoutExpired as exc:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except OSError:
            proc.kill()
        proc.communicate()
        raise TimeoutError from exc
    return subprocess.CompletedProcess(argv, proc.returncode, stdout, stderr)


def _diagnostics(proc, tmp: str) -> str:
    """The compiler's own message, with our scratch path taken out of it.

    The caller wrote the code and needs the line number; where we happened to
    put the file is noise, and telling them is a small leak of the host's
    layout for no benefit.
    """
    text = (proc.stderr or proc.stdout or "").strip()
    text = text.replace(os.path.join(tmp, "program.mojo"), "program.mojo")
    text = text.replace(tmp + os.sep, "").replace(SHARED + os.sep, "")
    # The compiler names itself by absolute path in its summary line, which is
    # the one place a caller learns where our toolchain lives.
    mojo = hostcompile.mojo_binary()
    if mojo:
        text = text.replace(mojo, "mojo")
    # Modular's crash-reporter warning appears on every invocation on this box
    # and means nothing to a caller.
    lines = [ln for ln in text.splitlines()
             if "Failed to initialize Crashpad" not in ln]
    return "\n".join(lines)[:MAX_LOG]


def _write(path: str, text: str) -> None:
    tmp = f"{path}.{os.getpid()}.tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.replace(tmp, path)


def _read(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read()
    except OSError:
        return ""


def stage(binary: str, work: str) -> str:
    """Put a copy of the binary where the jailed run can execute it.

    Hardlinked when the filesystem allows it, exactly like a seeded kernel: the
    run then executes it without the shared cache being reachable from inside
    the jail at all.
    """
    target = os.path.join(work, "program")
    for attempt in range(2):
        if not os.path.exists(binary):
            cachestore.restore(os.path.basename(binary))
        try:
            os.link(binary, target)
            break
        except OSError:
            try:
                shutil.copyfile(binary, target)
                break
            except OSError:
                if attempt:
                    raise
    os.chmod(target, 0o755)
    return target
