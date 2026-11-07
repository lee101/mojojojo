"""Content-addressed virtualenvs, built and cached with uv.

A run that asks for `["numpy", "pandas==2.2.*"]` gets a venv keyed by the hash
of that sorted list. The first request for a set pays the install; every later
one pays a `stat`. uv's own package cache is shared across all of them and
hardlinks into the venv, so even a cold set is usually a second or two rather
than a pip-shaped minute.

Environments are immutable once built: nothing ever installs into an existing
env, because two concurrent runs would then see each other's packages.
"""

from __future__ import annotations

import hashlib
import os
import re
import shutil
import subprocess
import threading
import time

DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
ENV_ROOT = os.path.join(DATA_ROOT, "envs")
UV_CACHE = os.environ.get("UV_CACHE_DIR", os.path.join(DATA_ROOT, "uv-cache"))
# uv's managed interpreters land under ~/.local/share/uv by default, and the
# sandbox user cannot traverse another user's home — the venv symlink resolves
# to something it is not allowed to execute. Keeping interpreters beside the
# environments they belong to is what makes the jail able to run them.
UV_PYTHON_DIR = os.environ.get("UV_PYTHON_INSTALL_DIR",
                               os.path.join(DATA_ROOT, "python"))


def _find_uv() -> str:
    # systemd gives the unit a minimal PATH, and uv usually lives in the
    # operator's ~/.local/bin — so PATH alone is not enough to find it.
    for candidate in (os.environ.get("MOJOJOJO_UV"), shutil.which("uv"),
                      os.path.expanduser("~/.local/bin/uv"), "/usr/local/bin/uv"):
        if candidate and os.path.exists(candidate):
            return candidate
    return "uv"


UV = _find_uv()
PYTHON_VERSION = os.environ.get("MOJOJOJO_PYTHON", "3.13")
# Interpreters the sandbox can actually reach. The jail hides /home, so a venv
# whose `bin/python` symlinks into a user's uv cache is a venv that cannot be
# executed inside it — which fails as `Permission denied` at exec, a long way
# from the cause.
VISIBLE_PREFIXES = ("/usr/", os.path.join(DATA_ROOT, "python"))
# Everything gets these without asking. numpy in particular is what makes the
# native path zero-copy: an array crosses to Mojo as an address, a list has to
# be copied element by element.
BASE_PACKAGES = [p for p in os.environ.get(
    "MOJOJOJO_BASE_PACKAGES", "numpy").split(",") if p.strip()]
INSTALL_TIMEOUT = float(os.environ.get("MOJOJOJO_INSTALL_TIMEOUT", "300"))
MAX_PACKAGES = int(os.environ.get("MOJOJOJO_MAX_PACKAGES", "32"))

# Deliberately strict: this string reaches an argv. Name, optional extras,
# optional PEP 440 specifier. No URLs, no paths, no flags.
_SPEC = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}"
                   r"(\[[A-Za-z0-9,._-]{1,64}\])?"
                   r"((==|>=|<=|~=|!=|<|>)[A-Za-z0-9._*+!-]{1,32})?$")

_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()


class EnvError(RuntimeError):
    pass


def normalize(packages) -> list[str]:
    """Validate and canonicalize a requested package set."""
    out: set[str] = set(BASE_PACKAGES)
    for raw in packages or []:
        spec = str(raw).strip()
        if not spec:
            continue
        if not _SPEC.match(spec):
            raise EnvError(f"invalid package spec: {spec!r}")
        out.add(spec)
    if len(out) > MAX_PACKAGES:
        raise EnvError(f"too many packages (max {MAX_PACKAGES})")
    return sorted(out)


_interpreter: str | None = None


def base_interpreter() -> str:
    """An absolute interpreter path every sandbox can see.

    Passing `--python 3.13` lets uv choose, and what it chooses depends on
    which machine and which user ran it. Pinning the path here is what keeps
    the environments the worker builds runnable inside the jail.
    """
    global _interpreter
    if _interpreter is not None:
        return _interpreter
    candidates = [os.environ.get("MOJOJOJO_PYTHON_BIN", "")]
    candidates += [f"/usr/bin/python{PYTHON_VERSION}", "/usr/bin/python3"]
    for candidate in candidates:
        if candidate and os.path.exists(candidate) and _visible(candidate):
            _interpreter = candidate
            return _interpreter
    # Nothing on the system: let uv manage one, but inside our own directory
    # so that it lands somewhere the sandbox is allowed to look.
    env = dict(os.environ, UV_PYTHON_INSTALL_DIR=UV_PYTHON_DIR)
    try:
        _run([UV, "python", "install", PYTHON_VERSION], env)
    except EnvError:
        pass
    for root, _dirs, files in os.walk(UV_PYTHON_DIR):
        if "python3" in files and root.endswith("/bin"):
            _interpreter = os.path.join(root, "python3")
            return _interpreter
    _interpreter = PYTHON_VERSION  # last resort: uv picks, and we check after
    return _interpreter


def _visible(path: str) -> bool:
    real = os.path.realpath(path)
    return any(real.startswith(prefix) for prefix in VISIBLE_PREFIXES)


def key_for(packages: list[str]) -> str:
    payload = PYTHON_VERSION + "\n" + "\n".join(packages)
    return hashlib.sha256(payload.encode()).hexdigest()[:24]


def _lock(key: str) -> threading.Lock:
    with _locks_guard:
        return _locks.setdefault(key, threading.Lock())


def ensure(packages) -> tuple[str, dict]:
    """Return (python_executable, info) for this package set, building if new."""
    specs = normalize(packages)
    key = key_for(specs)
    path = os.path.join(ENV_ROOT, key)
    python = os.path.join(path, "bin", "python")
    ready = os.path.join(path, ".ready")

    if os.path.exists(ready):
        return python, {"key": key, "cached": True, "build_ms": 0.0, "packages": specs}

    with _lock(key):
        if os.path.exists(ready):
            return python, {"key": key, "cached": True, "build_ms": 0.0, "packages": specs}
        started = time.perf_counter()
        if os.path.exists(path):
            shutil.rmtree(path, ignore_errors=True)
        os.makedirs(ENV_ROOT, exist_ok=True)
        # Scrubbed: the runner's own environment holds credentials, and a
        # package install must never see them.
        scratch = os.path.join(ENV_ROOT, ".home")
        os.makedirs(scratch, exist_ok=True)
        env = {
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "HOME": scratch,
            "TMPDIR": scratch,
            "LANG": os.environ.get("LANG", "C.UTF-8"),
            "UV_CACHE_DIR": UV_CACHE,
            "UV_LINK_MODE": "hardlink",
            "UV_PYTHON_INSTALL_DIR": UV_PYTHON_DIR,
            "UV_NO_CONFIG": "1",
        }
        for var in ("UV_PYTHON_PREFERENCE", "UV_OFFLINE", "SSL_CERT_FILE", "SSL_CERT_DIR"):
            if os.environ.get(var):
                env[var] = os.environ[var]
        _run([UV, "venv", "--python", base_interpreter(), path], env)
        if not _visible(python):
            # Refuse to hand back an environment the sandbox cannot execute.
            # Failing here is a bad build; failing later is a bad run.
            shutil.rmtree(path, ignore_errors=True)
            raise EnvError(
                f"interpreter {os.path.realpath(python)} is outside the paths "
                "the sandbox can see — set MOJOJOJO_PYTHON_BIN")
        if specs:
            _run([UV, "pip", "install", "--python", python, "--quiet",
                  "--only-binary", ":all:", *specs], env)
        with open(ready, "w", encoding="utf-8") as fh:
            fh.write("\n".join(specs))
        build_ms = (time.perf_counter() - started) * 1000.0
        return python, {"key": key, "cached": False, "build_ms": round(build_ms, 1),
                        "packages": specs}


def _run(cmd: list[str], env: dict) -> None:
    proc = subprocess.run(cmd, capture_output=True, text=True,
                          timeout=INSTALL_TIMEOUT, env=env)
    if proc.returncode != 0:
        raise EnvError((proc.stderr or proc.stdout).strip()[:2000])


def warm() -> dict:
    """Build the base environment at startup so the first user run is warm."""
    _, info = ensure([])
    return info
