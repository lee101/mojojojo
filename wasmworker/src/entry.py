"""mojojojo at the edge: the same `/run` contract, in a Cloudflare Worker.

Three places can run a caller's Python, and this is the middle one:

  browser   free, private, ~2s to boot Pyodide the first time, no packages
  edge      this — ~0ms to reach, CPython-in-wasm, no compiler, no filesystem
  our box   warm pools, the Mojo compile cache, GPUs, every package on PyPI

The edge exists because most submitted programs are small. A program that runs
for four milliseconds does not deserve a round trip to one machine in New
Zealand, and it does not deserve a rented VM either. It deserves the nearest
CPU that can hold CPython, which is a Worker isolate — and Cloudflare bills us
for the milliseconds it actually burns, which is the same shape as what we bill
the caller, so the margin is arithmetic rather than hope.

What this cannot do, and says so rather than pretending: no Mojo (there is no
compiler here and no way to load a shared library), no GPU, no threads, no
filesystem, no network, and only the packages Pyodide ships. Anything outside
that is routed elsewhere by `web/route.js` before it ever gets here.

The isolate is the sandbox. That is the same boundary the browser tier trusts,
with one difference that matters: in the browser the user is running their own
code on their own machine, and here they are running it on ours, next to our
bindings. So the escape hatches Pyodide deliberately provides — `import js`,
`pyodide.ffi`, the ffi on any handle we might leak — are closed before user
code is given control, and the program is executed in a namespace that has
never seen `self.env`.
"""

import base64
import hashlib
import hmac
import io
import json
import sys
import time
import traceback

from workers import DurableObject, Response, WorkerEntrypoint

# Everything that reaches the host from inside Pyodide. `js` is the whole
# JavaScript global scope — with it, user code has our Worker's identity, our
# bindings and our outbound network. The rest are either the same door under a
# different name or an escape that has no meaning in an isolate anyway.
BLOCKED = frozenset({
    "js", "pyodide", "pyodide_js", "_pyodide", "pyodide.ffi", "pyodide.http",
    "workers", "cloudflare", "ctypes", "socket", "ssl", "subprocess",
    "multiprocessing", "threading", "_thread", "asyncio.subprocess",
})

MAX_CODE_BYTES = 512 * 1024
MAX_OUTPUT = 256 * 1024
# Cloudflare kills the isolate at the configured CPU limit; this is our own,
# softer one, checked between lines so a runaway loop gets a clean answer and
# an exit code instead of a dropped connection.
DEFAULT_TIMEOUT_MS = 10_000
MAX_TIMEOUT_MS = 30_000
# How often the deadline is checked, in Python lines. Checking every line costs
# more than the programs we are trying to run; 2000 is under a millisecond of
# drift on anything realistic.
TRACE_STRIDE = 2000


class _Deadline(BaseException):
    """Raised in the running program when its time is up."""


class _Blocker:
    """Refuses the imports that would let a program leave the isolate."""

    def find_module(self, name, path=None):      # noqa: D102 - legacy protocol
        return self if self._blocked(name) else None

    def find_spec(self, name, path=None, target=None):  # noqa: D102
        if self._blocked(name):
            raise ImportError(
                f"mojojojo: `{name}` is not available at the edge — "
                "this tier has no host access. Run it on the server tier "
                "(where=\"server\") if you need it.")
        return None

    @staticmethod
    def _blocked(name: str) -> bool:
        root = name.split(".")[0]
        return name in BLOCKED or root in BLOCKED


def _seal() -> None:
    """Close the host doors, once per isolate.

    Installed at the front of `sys.meta_path` and left there: an isolate is
    reused across requests, so this both blocks the import and makes sure a
    module imported by an earlier request cannot be found by a later one.
    """
    if any(isinstance(f, _Blocker) for f in sys.meta_path):
        return
    sys.meta_path.insert(0, _Blocker())
    for name in list(sys.modules):
        if _Blocker._blocked(name):
            sys.modules.pop(name, None)


def _tracer(deadline: float):
    """A line tracer that raises when the program has had its time."""
    state = {"n": 0}

    def trace(frame, event, _arg):
        state["n"] += 1
        if state["n"] % TRACE_STRIDE == 0 and time.perf_counter() > deadline:
            raise _Deadline()
        return trace

    return trace


def _capped(text: str) -> str:
    if len(text) <= MAX_OUTPUT:
        return text
    return text[:MAX_OUTPUT] + f"\n... [truncated {len(text) - MAX_OUTPUT} bytes]"


def execute(code: str, stdin: str = "", args=None, timeout_ms: int = DEFAULT_TIMEOUT_MS) -> dict:
    """Run one program and report it the way every other tier does."""
    timeout_ms = max(1, min(int(timeout_ms or DEFAULT_TIMEOUT_MS), MAX_TIMEOUT_MS))
    _seal()

    out, err = io.StringIO(), io.StringIO()
    real_out, real_err, real_in = sys.stdout, sys.stderr, sys.stdin
    real_argv = sys.argv
    exit_code = 0
    timed_out = False

    # A namespace of its own, with nothing of ours in it.
    namespace = {"__name__": "__main__", "__builtins__": __builtins__}
    started = time.perf_counter()
    deadline = started + timeout_ms / 1000.0
    try:
        compiled = compile(code, "<mojojojo>", "exec")
    except SyntaxError:
        traceback.print_exc(file=err)
        return _result(1, False, "", err.getvalue(),
                       (time.perf_counter() - started) * 1000.0)

    sys.stdout, sys.stderr = out, err
    sys.stdin = io.StringIO(stdin or "")
    sys.argv = ["<mojojojo>"] + [str(a) for a in (args or [])]
    sys.settrace(_tracer(deadline))
    try:
        exec(compiled, namespace)  # noqa: S102 - running the caller's code is the product
    except _Deadline:
        timed_out = True
        exit_code = 124
        print("mojojojo: time limit exceeded", file=err)
    except SystemExit as exc:
        exit_code = int(exc.code or 0)
    except BaseException as exc:  # noqa: BLE001 - the traceback is the answer
        exit_code = 1
        tb = exc.__traceback__
        traceback.print_exception(type(exc), exc, tb.tb_next if tb else None,
                                  file=err)
    finally:
        sys.settrace(None)
        sys.stdout, sys.stderr, sys.stdin = real_out, real_err, real_in
        sys.argv = real_argv

    return _result(exit_code, timed_out, out.getvalue(), err.getvalue(),
                   (time.perf_counter() - started) * 1000.0)


def _result(exit_code: int, timed_out: bool, stdout: str, stderr: str,
            wall_ms: float) -> dict:
    """One shape, shared with the server tier, so a caller cannot tell the
    difference except by `where`."""
    wall_ms = round(wall_ms, 3)
    return {
        "ok": exit_code == 0 and not timed_out,
        "stdout": _capped(stdout),
        "stderr": _capped(stderr),
        "exit_code": exit_code,
        "timed_out": timed_out,
        "wall_ms": wall_ms,
        "cpu_ms": wall_ms,
        # The edge has no compiler and no warm phase, so what the program took
        # is exactly what it is billed for.
        "billable_ms": max(0.001, wall_ms),
        "queue_ms": 0.0,
        "accelerated": [],
        "accel": {},
        "language": "python",
        "isolated": True,
        "warm_start": True,
        "where": "edge",
    }


def _sign(secret: str, payload: str) -> str:
    return hmac.new(secret.encode(), payload.encode(), hashlib.sha256).hexdigest()


def open_ticket(secret: str, raw: str):
    """A ticket is a signed statement that an account may spend milliseconds here.

    It is what lets a browser talk to this Worker directly instead of going
    through the control plane — which is the entire point of an edge tier,
    because a round trip to the control plane is the latency we came here to
    avoid. Verified against a secret only we and the control plane hold; a
    ticket that does not verify is not an error to explain, it is a 401.
    """
    if not raw or "." not in raw:
        return None
    payload, signature = raw.split(".", 1)
    if not hmac.compare_digest(_sign(secret, payload), signature):
        return None
    try:
        padded = payload + "=" * (-len(payload) % 4)
        ticket = json.loads(base64.urlsafe_b64decode(padded))
    except Exception:  # noqa: BLE001 - a malformed ticket is just a refusal
        return None
    if not isinstance(ticket, dict) or time.time() > float(ticket.get("x", 0)):
        return None
    return ticket


class TicketLedger(DurableObject):
    """One instance per ticket nonce: the first claim wins, every later one is
    refused. Storage is cleared by an alarm when the ticket would have expired
    anyway, so the used-set never outlives the tickets it guards."""

    def __init__(self, ctx, env):
        self.ctx = ctx
        self.env = env

    async def fetch(self, request):
        if await self.ctx.storage.get("used"):
            return Response("used", status=409)
        await self.ctx.storage.put("used", 1)
        expires = request.url.rsplit("=", 1)[-1]
        await self.ctx.storage.setAlarm(int(float(expires) * 1000) + 60_000)
        return Response("ok")

    async def alarm(self):
        await self.ctx.storage.deleteAll()


async def claim_ticket(env, ticket) -> bool:
    ledger = getattr(env, "TICKETS", None)
    if ledger is None:
        return False
    stub = ledger.get(ledger.idFromName(str(ticket.get("n") or "")))
    response = await stub.fetch(f"https://tickets.internal/claim?x={float(ticket.get('x', 0))}")
    return response.status == 200


class Default(WorkerEntrypoint):
    """POST /run, GET /health. Nothing else exists."""

    async def fetch(self, request):
        url = str(request.url)
        path = "/" + url.split("://", 1)[-1].split("/", 1)[-1].split("?")[0] \
            if "://" in url else url
        path = path.rstrip("/") or "/"

        if request.method == "GET" and path.endswith("/health"):
            return _json({"ok": True, "where": "edge",
                          "python": sys.version.split()[0],
                          "runtime": "pyodide"})

        if request.method != "POST" or not path.endswith("/run"):
            return _json({"error": "not found"}, 404)

        # The edge is on the public internet and burns our Cloudflare budget,
        # so it answers two callers and nobody else: the control plane, with the
        # shared secret, and a browser holding a ticket the control plane signed.
        token = getattr(self.env, "EDGE_TOKEN", "") or ""
        if not token:
            return _json({"error": "edge is not configured"}, 503)
        ticket = None
        supplied = request.headers.get("Authorization") or ""
        supplied = supplied[7:].strip() if supplied.lower().startswith("bearer ") else ""
        if not hmac.compare_digest(supplied, token):
            ticket = open_ticket(token, request.headers.get("X-Mojojojo-Ticket") or "")
            if ticket is None or not ticket.get("n"):
                return _json({"error": "unauthorised"}, 401)

        try:
            body = await request.json()
            body = body.to_py() if hasattr(body, "to_py") else body
        except Exception:  # noqa: BLE001
            return _json({"error": "invalid json"}, 400)
        if not isinstance(body, dict):
            return _json({"error": "invalid json"}, 400)

        code = body.get("code")
        if not isinstance(code, str) or not code.strip():
            return _json({"error": "code is required"}, 400)
        if len(code.encode()) > MAX_CODE_BYTES:
            return _json({"error": f"code exceeds {MAX_CODE_BYTES} bytes"}, 400)
        # Refused rather than silently ignored: a caller who asked for a GPU and
        # got a wasm CPU has been lied to about what ran their code.
        if body.get("gpu"):
            return _json({"error": "the edge has no GPU", "retry_elsewhere": True}, 409)
        if (body.get("language") or "python") != "python":
            return _json({"error": "the edge runs Python only",
                          "retry_elsewhere": True}, 409)
        if body.get("packages"):
            return _json({"error": "the edge ships only Pyodide's packages",
                          "retry_elsewhere": True}, 409)

        timeout_ms = body.get("timeout_ms") or DEFAULT_TIMEOUT_MS
        if ticket is not None:
            # A ticket says how much the bearer may spend. It is a limit, not a
            # request: an unsettled run is one we have already paid for.
            timeout_ms = min(int(timeout_ms), int(ticket.get("m") or MAX_TIMEOUT_MS))

        if ticket is not None and not await claim_ticket(self.env, ticket):
            return _json({"error": "ticket already used"}, 401)

        result = execute(code, body.get("stdin") or "", body.get("args"), timeout_ms)

        if ticket is not None:
            # The receipt is what makes the caller's own report of their bill
            # unforgeable: the control plane recomputes this over the same two
            # values and refuses anything else.
            ms = result["billable_ms"]
            result["ticket"] = ticket.get("n")
            result["receipt"] = _sign(token, f"{ticket.get('n')}:{ms:.3f}")
        return _json(result)


def _json(payload: dict, status: int = 200):
    return Response(json.dumps(payload), status=status,
                    headers={"Content-Type": "application/json"})
