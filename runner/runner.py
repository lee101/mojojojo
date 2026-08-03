"""The mojojojo execution worker.

One HTTP endpoint, `POST /run`, which takes a Python program and returns what
it printed plus how long it took. Deliberately stdlib-only: this process is on
the latency path of every billed millisecond, so it starts in tens of
milliseconds and has nothing to keep up to date.

The web tier (Go, port 4341) owns auth and billing and is the only thing that
should be able to reach this port.
"""

from __future__ import annotations

import atexit
import contextlib
import ipaddress
import json
import os
import signal
import secrets
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import autoinstall
import envs
import hostcompile
import mojolibs
import mojoprog
import pool
import sandbox

HOST = os.environ.get("MOJOJOJO_RUNNER_HOST", "127.0.0.1")
PORT = int(os.environ.get("MOJOJOJO_RUNNER_PORT", "4342"))
# Capped well below the core count on purpose: every extra concurrent run
# makes every other run's wall clock — which is what we bill — worse.
MAX_CONCURRENCY = max(1, int(os.environ.get(
    "MOJOJOJO_CONCURRENCY", str(max(2, min(16, (os.cpu_count() or 4) - 2))))))
MAX_CODE_BYTES = int(os.environ.get("MOJOJOJO_MAX_CODE", str(512 * 1024)))
# JSON escaping can make a valid source string several times larger than its
# decoded form (for example, control characters become ``\u0000``).  Keep the
# wire limit separate from the source limit so valid programs are not rejected
# merely because of how a client serialised them.
MAX_REQUEST_BYTES = int(os.environ.get(
    "MOJOJOJO_MAX_REQUEST", str(MAX_CODE_BYTES * 6 + 64 * 1024)))
MAX_STDIN_BYTES = int(os.environ.get("MOJOJOJO_MAX_STDIN", str(1 << 20)))
MAX_ARGS = int(os.environ.get("MOJOJOJO_MAX_ARGS", "256"))
MAX_ARG_BYTES = int(os.environ.get("MOJOJOJO_MAX_ARG_BYTES", str(64 * 1024)))
MAX_ENV_VARS = int(os.environ.get("MOJOJOJO_MAX_ENV_VARS", "128"))
MAX_ENV_BYTES = int(os.environ.get("MOJOJOJO_MAX_ENV_BYTES", str(128 * 1024)))
DEFAULT_TIMEOUT_MS = int(os.environ.get("MOJOJOJO_DEFAULT_TIMEOUT_MS", "10000"))
MAX_TIMEOUT_MS = int(os.environ.get("MOJOJOJO_MAX_TIMEOUT_MS", "300000"))
# Set on rented workers, which are reachable over the public internet. Empty on
# the local box, where the only thing that can reach this port is the web tier
# on loopback.
TOKEN = os.environ.get("MOJOJOJO_RUNNER_TOKEN", "")
REQUEST_TIMEOUT_S = float(os.environ.get("MOJOJOJO_REQUEST_TIMEOUT_S", "15"))
# Authentication and JSON parsing happen before the execution semaphore.  A
# separate ceiling keeps slow or malformed clients from creating an unbounded
# number of handler threads while all execution slots are occupied.
MAX_HTTP_CONCURRENCY = max(1, int(os.environ.get(
    "MOJOJOJO_HTTP_CONCURRENCY", str(max(16, MAX_CONCURRENCY * 4)))))

# One GPU, shared: concurrent CUDA programs contend for VRAM and each other's
# SMs, so they queue rather than thrash. Raise only with more devices.
GPU_CONCURRENCY = max(1, int(os.environ.get("MOJOJOJO_GPU_CONCURRENCY", "2")))

_slots = threading.BoundedSemaphore(MAX_CONCURRENCY)
_gpu_slots = threading.BoundedSemaphore(GPU_CONCURRENCY)
_base_python_path: str | None = None
_started = time.time()
_stats = {"runs": 0, "errors": 0, "ms": 0.0, "gpu_queue_s": 0.0}
# Depth of the wait for a slot, which is the signal the autoscaler watches.
_queued = {"cpu": 0, "gpu": 0, "since": 0.0}
_stats_lock = threading.Lock()


@contextlib.contextmanager
def _gpu_slot(wants_gpu: bool):
    """Time-slice the GPU.

    One GPU, many tenants: without a limit, four concurrent CUDA programs
    thrash VRAM and every one of them takes longer, which — since we bill wall
    clock — means everybody pays more for the same work. Runs queue instead,
    and queue time is not billed.
    """
    if not wants_gpu:
        yield
        return
    started = time.perf_counter()
    with _gpu_slots:
        waited = time.perf_counter() - started
        with _stats_lock:
            _stats["gpu_queue_s"] += waited
        yield


def _enter_queue(gpu: bool) -> None:
    with _stats_lock:
        key = "gpu" if gpu else "cpu"
        if _queued["cpu"] + _queued["gpu"] == 0:
            _queued["since"] = time.time()
        _queued[key] += 1


def _leave_queue(gpu: bool) -> None:
    with _stats_lock:
        key = "gpu" if gpu else "cpu"
        _queued[key] = max(0, _queued[key] - 1)
        if _queued["cpu"] + _queued["gpu"] == 0:
            _queued["since"] = 0.0


def _queue_state() -> dict:
    """How deep the wait is and how long it has been there.

    The autoscaler needs both: depth alone cannot tell a burst from a
    shortage, and renting a machine for a burst is how a cloud bill happens.
    """
    with _stats_lock:
        since = _queued["since"]
        return {
            "cpu": _queued["cpu"],
            "gpu": _queued["gpu"],
            "for_s": round(time.time() - since, 1) if since else 0.0,
        }


def _base_python() -> str:
    global _base_python_path
    if _base_python_path is None:
        _base_python_path, _ = envs.ensure([])
    return _base_python_path


def _environment(code: str, req: dict) -> tuple[str, dict]:
    """Resolve the interpreter for this run, installing what the code imports.

    A guessed package that will not install must never fail a run that would
    otherwise work, so a failed resolution falls back to exactly what the
    caller asked for and says so. Environment builds are not billed either way.
    """
    explicit = list(req.get("packages") or [])
    auto: list[str] = []
    if req.get("auto_install", True) is not False:
        try:
            auto = autoinstall.detect(code, _base_python())
        except Exception:  # noqa: BLE001 - detection is a convenience
            auto = []

    try:
        python, info = envs.ensure(explicit + auto)
        info["auto"] = auto
        return python, info
    except envs.EnvError:
        if not auto:
            raise
    python, info = envs.ensure(explicit)
    info["auto"] = []
    info["auto_failed"] = auto
    return python, info


LANGUAGES = ("python", "mojo")


def _encoded_size(value: str) -> int:
    """UTF-8 size, treating an unpaired JSON surrogate as invalid text."""
    try:
        return len(value.encode("utf-8"))
    except UnicodeEncodeError as exc:
        raise ValueError("text contains an invalid Unicode surrogate") from exc


def _reject_json_constant(value: str):
    """The stdlib decoder otherwise accepts NaN and infinities as extensions."""
    raise ValueError(f"invalid JSON number: {value}")


def _validate_request(req: dict) -> str | None:
    """Reject malformed work before it can occupy an execution slot."""
    code = req.get("code")
    if code is not None and not isinstance(code, str):
        return "code must be a string"
    if isinstance(code, str):
        try:
            if _encoded_size(code) > MAX_CODE_BYTES:
                return f"code exceeds {MAX_CODE_BYTES} bytes"
        except ValueError as exc:
            return str(exc)

    language = req.get("language")
    if language is not None and not isinstance(language, str):
        return "language must be a string"
    if req.get("entrypoint") is not None and not isinstance(req["entrypoint"], str):
        return "entrypoint must be a string"
    for field in ("gpu", "accel", "auto_install", "mojo_libs"):
        if req.get(field) is not None and not isinstance(req[field], bool):
            return f"{field} must be a boolean"

    stdin = req.get("stdin")
    if stdin is not None:
        if not isinstance(stdin, str):
            return "stdin must be a string"
        try:
            if _encoded_size(stdin) > MAX_STDIN_BYTES:
                return f"stdin exceeds {MAX_STDIN_BYTES} bytes"
        except ValueError as exc:
            return str(exc)

    args = req.get("args")
    if args is not None:
        if not isinstance(args, list) or len(args) > MAX_ARGS:
            return f"args must be a list of at most {MAX_ARGS} values"
        total = 0
        for arg in args:
            if not isinstance(arg, (str, int, float, bool)) or arg is None:
                return "args values must be strings or numbers"
            text = str(arg)
            if "\x00" in text:
                return "args may not contain NUL bytes"
            try:
                total += _encoded_size(text)
            except ValueError as exc:
                return str(exc)
        if total > MAX_ARG_BYTES:
            return f"args exceed {MAX_ARG_BYTES} bytes"

    env = req.get("env")
    if env is not None:
        if not isinstance(env, dict) or len(env) > MAX_ENV_VARS:
            return f"env must contain at most {MAX_ENV_VARS} variables"
        total = 0
        for key, value in env.items():
            if not isinstance(key, str) or not isinstance(value, (str, int, float, bool)):
                return "env must map string names to scalar values"
            text = str(value)
            if "\x00" in key or "\x00" in text:
                return "env may not contain NUL bytes"
            try:
                total += _encoded_size(key) + _encoded_size(text)
            except ValueError as exc:
                return str(exc)
        if total > MAX_ENV_BYTES:
            return f"env exceeds {MAX_ENV_BYTES} bytes"

    packages = req.get("packages")
    if packages is not None and (
            not isinstance(packages, list)
            or any(not isinstance(spec, str) for spec in packages)):
        return "packages must be a list of strings"
    try:
        # Package specs otherwise reach validation only after the execution
        # semaphore is held, allowing requests that can never run to occupy
        # scarce execution capacity while environments are busy.
        envs.normalize(packages or [])
    except envs.EnvError as exc:
        return str(exc)

    timeout = req.get("timeout_ms")
    if timeout is not None and (isinstance(timeout, bool)
                                or not isinstance(timeout, int)):
        return "timeout_ms must be an integer"
    return None


def execute(req: dict) -> dict:
    if not isinstance(req, dict):
        return {"error": "request must be an object"}
    invalid = _validate_request(req)
    if invalid:
        return {"error": invalid}
    code = req.get("code")
    files = req.get("files")
    has_files = files is not None
    if (not isinstance(code, str) or not code.strip()) and not has_files:
        return {"error": "code is required"}
    language = (req.get("language") or "python").lower()
    if language not in LANGUAGES:
        return {"error": f"language must be one of {', '.join(LANGUAGES)}"}
    if has_files and not isinstance(files, dict):
        return {"error": "files must map Mojo paths to source strings"}
    if has_files and language != "mojo":
        return {"error": "files and entrypoint are only supported for Mojo"}
    if req.get("entrypoint") is not None and not has_files:
        return {"error": "entrypoint is only supported with Mojo project files"}
    if has_files and isinstance(code, str) and code.strip():
        return {"error": "send either code or files, not both"}
    if has_files:
        try:
            mojoprog._project(code or "", files, req.get("entrypoint"))
        except ValueError as exc:
            return {"error": str(exc)}

    timeout_ms = req.get("timeout_ms") or DEFAULT_TIMEOUT_MS
    timeout_ms = max(100, min(timeout_ms, MAX_TIMEOUT_MS))

    queued = time.perf_counter()
    wants_gpu = bool(req.get("gpu"))
    _enter_queue(wants_gpu)
    try:
        _slots.acquire()
    finally:
        _leave_queue(wants_gpu)
    try:
        with _gpu_slot(wants_gpu):
            queue_ms = (time.perf_counter() - queued) * 1000.0
            if language == "mojo":
                result = _run_mojo(code, req, timeout_ms, wants_gpu)
                result["queue_ms"] = round(queue_ms, 3)
                result["language"] = "mojo"
                with _stats_lock:
                    _stats["runs"] += 1
                    _stats["ms"] += result.get("billable_ms") or 0.0
                    if not result.get("ok"):
                        _stats["errors"] += 1
                return result
            try:
                python, env_info = _environment(code, req)
            except envs.EnvError as exc:
                return {"error": str(exc), "stage": "env"}

            # Ported Mojo libraries for whatever this program imports. Offered
            # as importable packages by default; substituted for the original
            # only when the caller asks with mojo_libs.
            ports = mojolibs.prepare(
                autoinstall.top_level_imports(code),
                enable_aliases=bool(req.get("mojo_libs")))

            result = sandbox.run(
                python=python,
                code=code,
                stdin=req.get("stdin") or "",
                args=req.get("args"),
                timeout_ms=timeout_ms,
                accel=req.get("accel", True) is not False,
                gpu=wants_gpu,
                env_extra=req.get("env") if isinstance(req.get("env"), dict) else None,
                mojo_paths=ports["paths"],
                mojo_aliases=ports["aliases"],
            )
            result["mojo"] = {k: ports[k] for k in ("used", "building", "aliases")}
    finally:
        _slots.release()

    result["env"] = env_info
    result["language"] = "python"
    result["queue_ms"] = round(queue_ms, 3)
    # What the caller is billed for: the program's own wall clock, less any
    # time our compiler spent competing with it. Not the queue, not the env
    # build, not the warm phase, not our own overhead.
    ran_ms = result.get("user_ms") or result["wall_ms"]
    compile_ms = float((result.get("accel") or {}).get("compile_ms") or 0.0)
    # The compiler runs on another core, so it inflates the caller's wall clock
    # by less than its own duration — and subtracting all of it would make a
    # run that triggers a compile nearly free, which is an invitation to submit
    # a stream of unique kernels for free compute. Half the run is the cap: a
    # compiling run is discounted, never given away.
    discount = min(compile_ms, ran_ms * 0.5)
    result["billable_ms"] = round(max(1.0, ran_ms - discount), 3)
    with _stats_lock:
        _stats["runs"] += 1
        _stats["ms"] += result["billable_ms"]
        if not result.get("ok"):
            _stats["errors"] += 1
    return result


def _run_mojo(code: str, req: dict, timeout_ms: int, wants_gpu: bool) -> dict:
    """Build a Mojo program on the host, then run the binary in the jail.

    The build is not billed and not charged for on failure: a program that does
    not compile costs its author nothing but the diagnostics. A build that
    succeeded once is content-addressed, so everybody after the first person
    pays only for the running.
    """
    if not mojoprog.available():
        return {"error": "no Mojo toolchain on this worker", "stage": "build"}
    try:
        built = mojoprog.build(code or "", req.get("files"), req.get("entrypoint"))
    except ValueError as exc:
        return {"error": str(exc), "stage": "build"}
    if not built["ok"]:
        return {
            "ok": False, "exit_code": 1, "timed_out": False,
            "stdout": "", "stderr": built["log"],
            "wall_ms": built["build_ms"], "cpu_ms": 0.0, "user_ms": 0.0,
            "billable_ms": 0.0, "build_ms": built["build_ms"],
            "build_cached": built["cached"], "stage": "build",
            "retry_elsewhere": bool(built.get("transient")),
            "accelerated": [], "accel": {}, "isolated": sandbox.jail_available(),
            "warm_start": False, "language": "mojo",
        }

    result = sandbox.run_binary(
        binary=built["binary"], stdin=req.get("stdin") or "",
        args=req.get("args"), timeout_ms=timeout_ms, gpu=wants_gpu,
        env_extra=req.get("env") if isinstance(req.get("env"), dict) else None)
    result["build_ms"] = built["build_ms"]
    result["build_cached"] = built["cached"]
    # The program's own wall clock inside the jail. Not the build, which is
    # ours, and not the jail setup, which is also ours.
    result["billable_ms"] = round(max(1.0, result.get("user_ms")
                                      or result["wall_ms"]), 3)
    return result


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "mojojojo-runner"

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(REQUEST_TIMEOUT_S)

    def log_message(self, fmt, *a):  # quieter: journald gets one line per run
        pass

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if status >= 400:
            # Error paths often reject before consuming a request body. Do not
            # parse those leftover bytes as another request on this connection.
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        self.wfile.write(body)

    def _header_values(self, name: str) -> list[str]:
        """Return every occurrence, including for simple test doubles."""
        if hasattr(self.headers, "get_all"):
            return self.headers.get_all(name, [])
        value = self.headers.get(name)
        return [] if value is None else [value]

    def _authorised(self) -> bool:
        """A rented worker is on the public internet; the local one is not.

        With no token configured this is a no-op, because the local worker is
        bound to loopback and only the web tier can reach it. With one, every
        request must carry it — a worker that would run anonymous code is a
        worker somebody else is mining crypto on within the hour.
        """
        if not TOKEN:
            if _loopback_host(HOST):
                return True
            self._send(503, {"error": "runner token is required on a non-loopback host"})
            return False
        headers = self._header_values("Authorization")
        if len(headers) != 1:
            self._send(401, {"error": "unauthorised"})
            return False
        header = headers[0]
        supplied = header[7:].strip() if header.lower().startswith("bearer ") else ""
        if not secrets.compare_digest(supplied, TOKEN):
            self._send(401, {"error": "unauthorised"})
            return False
        return True

    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        if not self._authorised():
            return
        if self.path.split("?")[0] not in ("/health", "/healthz"):
            self._send(404, {"error": "not found"})
            return
        with _stats_lock:
            stats = dict(_stats)
        self._send(200, {
            "ok": True,
            "isolated": sandbox.jail_available(),
            "accel": _accel_status(),
            "concurrency": MAX_CONCURRENCY,
            "gpu_concurrency": GPU_CONCURRENCY,
            "cache": hostcompile.stats(),
            "pool": pool.stats(),
            "mojo_libs": mojolibs.stats(),
            "uptime_s": round(time.time() - _started, 1),
            "queue": _queue_state(),
            "stats": stats,
        })

    def do_POST(self) -> None:  # noqa: N802 - stdlib naming
        if not self._authorised():
            return
        if self.path.split("?")[0] != "/run":
            self._send(404, {"error": "not found"})
            return
        if self._header_values("Transfer-Encoding"):
            self._send(400, {"error": "chunked request bodies are not supported"})
            return
        lengths = self._header_values("Content-Length")
        if len(lengths) != 1:
            self._send(400, {"error": "invalid content length"})
            return
        try:
            length = int(lengths[0])
        except (TypeError, ValueError, OverflowError):
            self._send(400, {"error": "invalid content length"})
            return
        if length <= 0 or length > MAX_REQUEST_BYTES:
            self._send(400, {"error": "bad request body"})
            return
        try:
            req = json.loads(
                self.rfile.read(length),
                parse_constant=_reject_json_constant)
        except (ValueError, RecursionError):
            self._send(400, {"error": "invalid json"})
            return
        if not isinstance(req, dict):
            self._send(400, {"error": "invalid json"})
            return
        try:
            result = execute(req)
        except Exception as exc:  # noqa: BLE001 - never drop a connection
            incident = secrets.token_hex(6)
            # Diagnostics belong in the service journal, not in an API that a
            # tenant can use to discover host paths or configuration details.
            detail = ascii(str(exc)).replace("\\n", " ")[:500]
            print(f"mojojojo-runner: incident={incident} "
                  f"type={type(exc).__name__} detail={detail}", flush=True)
            self._send(500, {"error": f"runner internal error ({incident})"})
            return
        self._send(400 if result.get("error") else 200, result)


def _accel_status() -> dict:
    try:
        import sys
        sys.path.insert(0, sandbox.MOJOSUB_PATH)
        os.environ.setdefault("MOJOSUB_MOJO", sandbox.mojo_binary())
        os.environ["MODULAR_HOME"] = sandbox.modular_home()
        import mojosub

        return {"available": True, "mojo": mojosub.mojo_version()}
    except Exception as exc:  # noqa: BLE001
        return {"available": False, "reason": str(exc)[:200]}


def _loopback_host(host: str) -> bool:
    """Whether an unauthenticated listener is unreachable off this machine."""
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host.strip("[]")).is_loopback
    except ValueError:
        return False


class BoundedThreadingHTTPServer(ThreadingHTTPServer):
    """ThreadingHTTPServer with a hard ceiling on pre-execution work."""

    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, *args, **kwargs) -> None:
        self._request_slots = threading.BoundedSemaphore(MAX_HTTP_CONCURRENCY)
        super().__init__(*args, **kwargs)

    def process_request(self, request: socket.socket, client_address) -> None:
        if not self._request_slots.acquire(blocking=False):
            try:
                request.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Content-Length: 22\r\nConnection: close\r\n\r\n"
                    b'{"error":"overloaded"}')
            except OSError:
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._request_slots.release()
            raise

    def process_request_thread(self, request, client_address) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._request_slots.release()


def _shutdown(_signum=None, _frame=None) -> None:
    """Take the warm pool with us.

    A slot outlives this process unless it is killed on the way out, and a slot
    that outlives us is threads and memory nobody will ever use again, counted
    against a limit every tenant shares.
    """
    pool.shutdown()
    raise SystemExit(0)


def main() -> None:
    if not TOKEN and not _loopback_host(HOST):
        raise SystemExit("MOJOJOJO_RUNNER_TOKEN is required for a non-loopback listener")
    os.makedirs(sandbox.RUN_ROOT, exist_ok=True)
    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)
    atexit.register(pool.shutdown)
    warm = envs.warm()
    # Fill the pool before the first request rather than on it.
    pool.warm(_base_python())
    how = "cached" if warm["cached"] else "built in %.0fms" % warm["build_ms"]
    print("mojojojo-runner base env %s (%s) isolated=%s concurrency=%d" %
          (warm["key"], how, sandbox.jail_available(), MAX_CONCURRENCY),
          flush=True)
    BoundedThreadingHTTPServer((HOST, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
