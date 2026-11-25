"""mojojojo — run Python on somebody else's machine, billed by the millisecond.

    pip install mojojojo

    from mojojojo import run
    r = run("print(sum(range(10)))")
    r.stdout          # '45\\n'
    r.charge.credits  # what it cost

Or, for code you already have as a function:

    from mojojojo import remote

    @remote(packages=["numpy"])
    def backtest(prices, window):
        ...

    backtest(prices, 64)   # runs on mojojojo, returns the value

The API key comes from `MOJOJOJO_API_KEY`. Get one at
https://mojojojo.cc/keys — new accounts get $1 of compute free.

No dependencies: this is one file over `urllib`, because a client library that
drags in `requests` to make one POST is a liability in somebody else's
environment.
"""

from __future__ import annotations

import base64
import inspect
import json
import os
import textwrap
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable

__version__ = "0.3.0"
__all__ = ["run", "precompile", "remote", "Result", "PrecompileResult",
           "Charge", "MojojojoError", "InsufficientCredits", "AuthError", "Client"]

DEFAULT_ENDPOINT = os.environ.get("MOJOJOJO_URL", "https://mojojojo.cc")


class MojojojoError(RuntimeError):
    """Something went wrong before your code ran, or instead of it."""

    def __init__(self, message: str, status: int = 0, body: dict | None = None):
        super().__init__(message)
        self.status = status
        self.body = body or {}


class AuthError(MojojojoError):
    """No key, a revoked key, or a key for a different account."""


class InsufficientCredits(MojojojoError):
    """The account is out of credits. Manage them at https://mojojojo.cc/credits."""


@dataclass
class Charge:
    billed_ms: float = 0.0
    credits: int = 0
    owed_ms: float = 0.0
    balance: int = 0
    ms_per_credit: float = 0.0

    @property
    def usd(self) -> float:
        """What this run cost, in dollars. A credit is $0.001."""
        if not self.ms_per_credit:
            return 0.0
        return (self.billed_ms / self.ms_per_credit) * 0.001


@dataclass
class Accel:
    compiled: int = 0
    native_calls: int = 0
    cache_hits: int = 0
    compile_ms: float = 0.0
    queued: int = 0
    error: str = ""


@dataclass
class Result:
    ok: bool = False
    stdout: str = ""
    stderr: str = ""
    exit_code: int = 0
    timed_out: bool = False
    wall_ms: float = 0.0
    cpu_ms: float = 0.0
    billable_ms: float = 0.0
    queue_ms: float = 0.0
    # A Mojo program's build: waited for, never billed, and zero once anybody
    # has run that exact source.
    build_ms: float = 0.0
    build_cached: bool = False
    language: str = "python"
    warm_start: bool = False
    isolated: bool = False
    accelerated: list[str] = field(default_factory=list)
    accel: Accel = field(default_factory=Accel)
    packages: list[str] = field(default_factory=list)
    auto_installed: list[str] = field(default_factory=list)
    charge: Charge = field(default_factory=Charge)
    welcome_credits: int = 0
    ran_on: str = ""
    raw: dict = field(default_factory=dict)

    def __str__(self) -> str:
        return self.stdout

    def check(self) -> "Result":
        """Raise if the program failed. Returns self, so it chains."""
        if not self.ok:
            raise MojojojoError(
                f"program exited {self.exit_code}"
                + (" (timed out)" if self.timed_out else "")
                + (f"\n{self.stderr.strip()}" if self.stderr else ""))
        return self

    @property
    def value(self) -> Any:
        """The value a `@remote` function returned.

        Encoded as a base64 JSON line on the last line of stdout so that
        anything the program printed stays exactly what it printed.
        """
        return self.raw.get("_value")


@dataclass
class PrecompileResult:
    """The explicit prepare stage before a latency-sensitive run.

    Python kernels need representative argument types, so preparation executes
    the supplied workload once. User execution is billed normally; environment
    builds and Mojo compilation remain free. The complete run result is kept
    here so stdout, diagnostics, and charges never disappear behind the stage.
    """

    result: Result
    ready: bool
    language: str
    kernels: list[str] = field(default_factory=list)
    message: str = ""

    @property
    def charge(self) -> Charge:
        return self.result.charge


class Client:
    """A configured connection. `run()` uses a default instance."""

    def __init__(self, api_key: str | None = None, endpoint: str | None = None,
                 timeout: float = 960.0):
        self.api_key = api_key or os.environ.get("MOJOJOJO_API_KEY", "")
        self.endpoint = (endpoint or DEFAULT_ENDPOINT).rstrip("/")
        self.timeout = timeout

    def run(self, code: str = "", *, packages: Iterable[str] | None = None,
            stdin: str = "", args: Iterable[str] | None = None,
            timeout_ms: int | None = None, gpu: bool = False,
            accel: bool = True, auto_install: bool = True,
            env: dict[str, str] | None = None, source: str = "sdk-python",
            language: str = "python", check: bool = False,
            files: dict[str, str] | None = None, entrypoint: str | None = None,
            mojo_libs: bool = False) -> Result:
        """Run `code` on mojojojo.

        `language="mojo"` sends a whole Mojo program instead of Python: it is
        built on the server (once, cached by content hash, never billed) and
        run with no interpreter in the way. `packages`, `accel` and
        `auto_install` are Python-only and are ignored for a Mojo run.
        """
        payload = {
            "code": "" if files else textwrap.dedent(code),
            "packages": list(packages or []),
            "stdin": stdin,
            "args": [str(a) for a in (args or [])],
            "language": language,
            "gpu": gpu,
            "accel": accel,
            "auto_install": auto_install,
            "env": env or {},
            "source": source,
            "mojo_libs": mojo_libs,
        }
        if files:
            payload["files"] = {name: textwrap.dedent(source)
                                for name, source in files.items()}
            payload["entrypoint"] = entrypoint or "main.mojo"
        if timeout_ms:
            payload["timeout_ms"] = int(timeout_ms)

        data = self._post("/v1/run", payload)
        result = _to_result(data)
        return result.check() if check else result

    def precompile(self, code: str = "", **kwargs) -> PrecompileResult:
        """Run a representative workload and prepare its compiled artifacts.

        For accelerated Python, the sample run supplies the argument and return
        types mojosub needs. For Mojo, the content-addressed program build is
        populated. Call this during deploy or warm-up, then use ``run`` on the
        latency-sensitive path.
        """
        kwargs = dict(kwargs)
        kwargs["source"] = kwargs.get("source", "sdk-python:precompile")
        kwargs["check"] = True
        result = self.run(code, **kwargs)
        language = kwargs.get("language", "python")
        if language == "mojo":
            ready = result.ok
            message = ("Mojo build already cached" if result.build_cached
                       else "Mojo build prepared")
        else:
            ready = bool(result.accel.compiled or result.accel.cache_hits or
                         result.accel.native_calls)
            if result.accel.native_calls or result.accel.cache_hits:
                message = "compiled kernels already cached"
            elif result.accel.compiled:
                message = "compiled kernels prepared for the next run"
            elif result.accel.queued:
                message = "kernel compilation is still queued"
            else:
                message = result.accel.error or "no eligible hot numeric kernels were observed"
        return PrecompileResult(result=result, ready=ready, language=language,
                                kernels=list(result.accelerated), message=message)

    def pricing(self) -> dict:
        return self._get("/v1/pricing")

    def me(self) -> dict:
        return self._get("/v1/me")

    def usage(self) -> dict:
        return self._get("/v1/usage")

    def credits(self) -> dict:
        """Balance, what it buys, and the ledger behind it.

        Worth calling before a long job: `credits()["buys"]["cpu_ms"]` is how
        many milliseconds the account can still pay for, and finding that out
        beforehand is cheaper than a 402 halfway through a backtest.
        """
        return self._get("/v1/credits")

    def projects(self) -> list[dict]:
        return self._get("/v1/projects").get("projects", [])

    def create_project(self, name: str, *, slug: str = "",
                       runtime: str = "static", category: str = "app",
                       description: str = "", tags: Iterable[str] = ()) -> dict:
        return self._post("/v1/projects", {
            "name": name, "slug": slug, "runtime": runtime,
            "category": category, "description": description,
            "tags": list(tags),
        })["project"]

    def project_files(self, project_id: str) -> dict[str, str]:
        """Load the editable Studio workspace for a project."""
        return self._get(f"/v1/projects/{project_id}/files").get("files", {})

    def save_project_files(self, project_id: str,
                           files: dict[str, str]) -> dict:
        """Atomically replace a project's editable text workspace."""
        return self._request("PUT", f"/v1/projects/{project_id}/files",
                             {"files": files})

    def publish_project(self, project_id: str) -> dict:
        """Publish the current Studio workspace as an immutable deployment."""
        return self._post(f"/v1/projects/{project_id}/publish", {})["deployment"]

    def submit_project(self, project_id: str) -> dict:
        """Submit the active deployment to the moderated apps/tools showcase."""
        return self._post(f"/v1/projects/{project_id}/submit", {})["submission"]

    def storage(self) -> dict:
        return self._get("/v1/storage")

    def databases(self) -> list[dict]:
        """List tenant-isolated managed JSON databases."""
        return self._get("/v1/data/namespaces").get("databases", [])

    def create_database(self, name: str, *, slug: str = "",
                        vector_project_id: str = "") -> dict:
        return self._post("/v1/data/namespaces", {
            "name": name, "slug": slug, "vector_project_id": vector_project_id,
        })["database"]

    def data_records(self, database_id: str) -> list[dict]:
        return self._get(f"/v1/data/namespaces/{database_id}/records").get("records", [])

    def data_upsert(self, database_id: str, key: str, value: Any, *,
                    search_text: str = "") -> dict:
        return self._request("PUT", f"/v1/data/namespaces/{database_id}/records", {
            "key": key, "value": value, "search_text": search_text,
        })

    def data_delete(self, database_id: str, key: str) -> dict:
        return self._request("DELETE", f"/v1/data/namespaces/{database_id}/records",
                             {"key": key})

    def vector_projects(self) -> list[dict]:
        """List managed retrieval projects owned by this account."""
        return self._get("/v1/vector/projects").get("projects", [])

    def create_vector_project(self, name: str, *, slug: str = "",
                              tier: str = "exact") -> dict:
        return self._post("/v1/vector/projects", {
            "name": name, "slug": slug, "tier": tier,
        })["project"]

    def vector_upsert(self, project_id: str, documents: Iterable[dict]) -> dict:
        """Embed and upsert as many as 256 {id,text,metadata} documents."""
        return self._post(f"/v1/vector/projects/{project_id}/upsert", {
            "documents": list(documents),
        })

    def vector_search(self, project_id: str, query: str, *, limit: int = 10) -> dict:
        return self._post(f"/v1/vector/projects/{project_id}/search", {
            "query": query, "limit": limit,
        })

    def vector_usage(self, project_id: str) -> dict:
        return self._get(f"/v1/vector/projects/{project_id}/usage")

    def delete_vector_project(self, project_id: str) -> dict:
        return self._request("DELETE", f"/v1/vector/projects/{project_id}")

    def deploy(self, project_id: str, bundle: str | os.PathLike | bytes) -> dict:
        """Deploy a ZIP without a multipart dependency."""
        if isinstance(bundle, (str, os.PathLike)):
            with open(bundle, "rb") as fh:
                data = fh.read()
        else:
            data = bytes(bundle)
        return self._bytes("POST", "/v1/deployments", data, {
            "Content-Type": "application/zip", "X-Project-ID": project_id,
        })["deployment"]

    def upload(self, filename: str, data: bytes, *, content_type: str =
               "application/octet-stream", project_id: str = "") -> dict:
        """Sign and upload an asset; provider endpoints never enter the client."""
        signed = self._post("/v1/uploads/sign", {
            "filename": os.path.basename(filename), "content_type": content_type,
            "size_bytes": len(data), "project_id": project_id,
        })
        request = urllib.request.Request(
            signed["upload_url"], data=data, method="PUT",
            headers={"Content-Type": content_type,
                     "User-Agent": f"mojojojo-python/{__version__}"})
        return self._send(request)

    # -- transport --

    def _request(self, method: str, path: str, payload: dict | None = None) -> dict:
        body = json.dumps(payload).encode() if payload is not None else None
        request = urllib.request.Request(
            self.endpoint + path, data=body, method=method,
            headers={
                "Content-Type": "application/json",
                # openpaths.io's edge 403s the default urllib agent, and this
                # client will end up behind the same sort of thing.
                "User-Agent": f"mojojojo-python/{__version__}",
                **({"Authorization": f"Bearer {self.api_key}"} if self.api_key else {}),
            })
        return self._send(request)

    def _send(self, request: urllib.request.Request) -> dict:
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                parsed = json.loads(raw)
            except ValueError:
                parsed = {"error": raw.decode("utf-8", "replace")[:500]}
            message = parsed.get("error", str(exc))
            if exc.code == 401:
                raise AuthError(
                    f"{message}\nSet MOJOJOJO_API_KEY — get one at "
                    f"{self.endpoint}/keys", exc.code, parsed) from None
            if exc.code == 402:
                raise InsufficientCredits(message, exc.code, parsed) from None
            raise MojojojoError(message, exc.code, parsed) from None
        except urllib.error.URLError as exc:
            raise MojojojoError(f"could not reach {self.endpoint}: {exc.reason}") from None

    def _bytes(self, method: str, path: str, body: bytes,
               headers: dict[str, str]) -> dict:
        request = urllib.request.Request(
            self.endpoint + path, data=body, method=method,
            headers={"User-Agent": f"mojojojo-python/{__version__}", **headers,
                     **({"Authorization": f"Bearer {self.api_key}"}
                        if self.api_key else {})})
        return self._send(request)

    def _post(self, path: str, payload: dict) -> dict:
        return self._request("POST", path, payload)

    def _get(self, path: str) -> dict:
        return self._request("GET", path)


_default = Client()


def run(code: str, **kwargs) -> Result:
    """Run `code` on mojojojo. See `Client.run` for the options."""
    client = kwargs.pop("client", None) or _default
    if "api_key" in kwargs:
        client = Client(api_key=kwargs.pop("api_key"),
                        endpoint=kwargs.pop("endpoint", None))
    return client.run(code, **kwargs)


def precompile(code: str, **kwargs) -> PrecompileResult:
    """Prepare a representative Python or Mojo workload. See ``Client.precompile``."""
    client = kwargs.pop("client", None) or _default
    if "api_key" in kwargs:
        client = Client(api_key=kwargs.pop("api_key"),
                        endpoint=kwargs.pop("endpoint", None))
    return client.precompile(code, **kwargs)


# --- @remote ---

_MARKER = "__mojojojo_value__:"

_PREAMBLE = f"""
import base64 as _b64, json as _json, sys as _sys

def _mojojojo_emit(value):
    try:
        payload = _json.dumps(value)
    except TypeError:
        payload = _json.dumps(repr(value))
    _sys.stdout.write("\\n{_MARKER}" + _b64.b64encode(payload.encode()).decode() + "\\n")
"""


def remote(fn: Callable | None = None, *, packages: Iterable[str] | None = None,
           gpu: bool = False, timeout_ms: int | None = None,
           client: Client | None = None):
    """Run this function's body on mojojojo instead of here.

    The function's source is shipped, its arguments are JSON, and its return
    value comes back the same way. That is the whole contract, and it is worth
    being blunt about what it excludes: no closures, no globals from the
    calling module, no objects that are not JSON. A function that needs those
    is a function that should be a script.

        @remote(packages=["numpy"])
        def sharpe(returns, window):
            import numpy as np
            ...
    """
    def decorate(func: Callable) -> Callable:
        try:
            source = textwrap.dedent(inspect.getsource(func))
        except (OSError, TypeError) as exc:  # a REPL, stdin, or a builtin
            raise MojojojoError(
                f"cannot read the source of {func.__name__}: {exc}. "
                "@remote ships source, so the function has to live in a file "
                "— use run() with a string from a REPL or a piped script."
            ) from None
        # Drop our own decorator line; the remote side has no mojojojo package.
        body = "\n".join(line for line in source.splitlines()
                         if not line.lstrip().startswith("@remote"))

        def wrapper(*args, **kwargs):
            call_args = json.dumps({"args": args, "kwargs": kwargs})
            program = (
                _PREAMBLE
                + "\n" + body + "\n"
                + f"_call = _json.loads({call_args!r})\n"
                + f"_mojojojo_emit({func.__name__}(*_call['args'], **_call['kwargs']))\n"
            )
            result = (client or _default).run(
                program, packages=packages, gpu=gpu, timeout_ms=timeout_ms,
                source="sdk-python:remote")
            result.check()
            return result.value

        wrapper.__name__ = func.__name__
        wrapper.__doc__ = func.__doc__
        wrapper.local = func  # escape hatch: run it here instead
        return wrapper

    return decorate(fn) if fn is not None else decorate


def _to_result(data: dict) -> Result:
    accel_raw = data.get("accel") or {}
    charge_raw = data.get("charge") or {}
    env_raw = data.get("env") or {}
    stdout = data.get("stdout", "")

    value = None
    lines = stdout.rstrip("\n").split("\n")
    if lines and lines[-1].startswith(_MARKER):
        try:
            value = json.loads(base64.b64decode(lines[-1][len(_MARKER):]))
            stdout = "\n".join(lines[:-1])
            if stdout:
                stdout += "\n"
        except (ValueError, TypeError):
            value = None

    raw = dict(data)
    raw["_value"] = value
    return Result(
        ok=bool(data.get("ok")),
        stdout=stdout,
        stderr=data.get("stderr", ""),
        exit_code=int(data.get("exit_code") or 0),
        timed_out=bool(data.get("timed_out")),
        wall_ms=float(data.get("wall_ms") or 0),
        cpu_ms=float(data.get("cpu_ms") or 0),
        billable_ms=float(data.get("billable_ms") or 0),
        queue_ms=float(data.get("queue_ms") or 0),
        build_ms=float(data.get("build_ms") or 0),
        build_cached=bool(data.get("build_cached")),
        language=data.get("language") or "python",
        warm_start=bool(data.get("warm_start")),
        isolated=bool(data.get("isolated")),
        accelerated=list(data.get("accelerated") or []),
        accel=Accel(
            compiled=int(accel_raw.get("compiled") or 0),
            native_calls=int(accel_raw.get("native_calls") or 0),
            cache_hits=int(accel_raw.get("cache_hits") or 0),
            compile_ms=float(accel_raw.get("compile_ms") or 0),
            queued=int(accel_raw.get("queued") or 0),
            error=accel_raw.get("error", ""),
        ),
        packages=list(env_raw.get("packages") or []),
        auto_installed=list(env_raw.get("auto") or []),
        charge=Charge(
            billed_ms=float(charge_raw.get("billed_ms") or 0),
            credits=int(charge_raw.get("credits") or 0),
            owed_ms=float(charge_raw.get("owed_ms") or 0),
            balance=int(charge_raw.get("balance") or 0),
            ms_per_credit=float(charge_raw.get("ms_per_credit") or 0),
        ),
        welcome_credits=int(data.get("welcome_credits") or 0),
        ran_on=data.get("ran_on", ""),
        raw=raw,
    )
