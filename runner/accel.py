"""Decide which of the submitted functions get compiled to native code.

mojojojo runs arbitrary user Python. Most of it is glue and should just run on
CPython. The parts worth native code are the numeric loops — a backtest, a
scoring pass, a simulation — which CPython runs 50-100x slower than they need
to. `mojosub` (github.com/lee101/mojosub) transpiles that typed-numeric subset
of Python to Mojo, caches the shared library by content hash, and dispatches
into it in about a microsecond.

Policy here, since we are billing by the millisecond and a wrong answer is
worse than a slow one:

- Only top-level `def`s, only bodies inside the subset (cheap AST pre-filter
  before the transpiler is asked).
- Nothing compiles until a function is hot (call count or CPU time), because a
  cold compile costs ~4.5s of wall clock on a background thread.
- The first native result is verified against CPython; a mismatch retires the
  variant instead of quietly returning a different number.
- Every failure path falls back to plain CPython.

The cache is content-addressed on disk and shared across runs, so the second
user to submit the same kernel gets native code on the first call.
"""

from __future__ import annotations

import ast
import os
from typing import Any

# One call, then compile. Lower than a long-lived process would want, because
# a run is short and the compile lands in a cache shared by every run on the
# box: paying it once for a kernel nobody submits again is cheap, and paying
# it once for a kernel everybody submits is the whole product.
HOT_CALLS = int(os.environ.get("MOJOJOJO_HOT_CALLS", "1"))
HOT_SECONDS = float(os.environ.get("MOJOJOJO_HOT_SECONDS", "0.0"))
# How long to keep the process alive after the program finishes, letting
# in-flight compiles reach the disk cache. This time is deliberately NOT
# billed — it buys the next caller's speed, not this one's.
WARM_MS = float(os.environ.get("MOJOJOJO_WARM_MS", "8000"))
DECORATOR = "__mojo_jit"

_available: bool | None = None
_jit = None
_source: str | None = None


def set_source(source: str) -> None:
    """Hand the program text to mojosub.

    User code is exec'd from an AST, not imported from a file, so
    `inspect.getsource` inside mojosub has nothing to read — without this
    every function fails to transpile with "could not get source code".
    """
    global _source
    _source = source


def rebind_environment() -> None:
    """Re-read the environment mojosub captured at import time.

    In a warm pool slot mojosub is imported once, in the forkserver, long
    before any run exists — so `cache.DEFAULT_ROOT` and the memoised toolchain
    lookup are bound to the *pool's* environment, not the run's. Without this,
    a forked run reads and writes a cache directory that has nothing in it, and
    every kernel looks new forever. Cheap and idempotent; called once per run.
    """
    try:
        import mojosub
    except Exception:  # noqa: BLE001
        return
    root = os.environ.get("MOJOSUB_CACHE")
    if root:
        mojosub.cache.DEFAULT_ROOT = os.path.expanduser(root)
    clear = getattr(getattr(mojosub.toolchain, "mojo_command", None),
                    "cache_clear", None)
    if clear:
        clear()

    # The version is a cache-key input, so mojosub asks the compiler for it —
    # which in here means starting a whole interpreter to run mojo_stub, ~35ms
    # on the latency path of every accelerated run, to be told the string the
    # host already put in our environment. `cache` and the package root bind
    # the function by value at import, so all three have to be replaced.
    version = os.environ.get("MOJOJOJO_MOJO_VERSION", "")
    if not version:
        clear = getattr(getattr(mojosub.toolchain, "mojo_version", None),
                        "cache_clear", None)
        if clear:
            clear()
        return

    def _known_version(_v: str = version) -> str:
        return _v

    for module in (mojosub, mojosub.cache, mojosub.toolchain):
        if getattr(module, "mojo_version", None) is not None:
            module.mojo_version = _known_version


def enabled() -> bool:
    """True when acceleration is on and mojosub can actually build."""
    global _available, _jit
    if os.environ.get("MOJOJOJO_ACCEL", "1") == "0":
        return False
    if _available is not None:
        return _available
    try:
        import mojosub

        mojosub.mojo_version()
        _record_signatures_before_building(mojosub)
        _jit = mojosub.jit
        _available = True
    except Exception:  # noqa: BLE001 - absence is a normal state
        _available = False
    return _available


def _record_signatures_before_building(mojosub) -> None:
    """Write the learned return type down before asking for a compile.

    mojosub records it *after* a successful build, which is the right order
    when the process doing the building is the one that will use the result.
    Here the build is deferred to the host, so the type — which only running
    the function under CPython can reveal — has to be written first or it is
    lost with the sandbox. It is the one thing a run teaches the cache.
    """
    api = mojosub.api
    if getattr(api.JitFunction, "_mojojojo_patched", False):
        return
    original = api.JitFunction._build

    def _build(self, key, argtypes, ret):
        try:
            mojosub.cache.remember_signature(self._source(), key, ret.key)
        except Exception:  # noqa: BLE001 - the memo is an optimisation
            pass
        return original(self, key, argtypes, ret)

    api.JitFunction._build = _build
    api.JitFunction._mojojojo_patched = True


def jit(fn):
    """Wrap `fn` with mojosub under this module's policy.

    Also exposed to user code as `mojo` so a caller can request acceleration
    explicitly on a function the automatic pass skipped.
    """
    if not enabled():
        return fn
    try:
        return _jit(fn, mode="tiered", hot_calls=HOT_CALLS,
                    hot_seconds=HOT_SECONDS, verify=True, source=_source)
    except Exception:  # noqa: BLE001
        return fn


# --- the static filter ------------------------------------------------------
#
# Whether a function can be compiled at all is decided here, statically, for
# nothing. Getting it wrong in the permissive direction is expensive and the
# cost is paid by somebody else: the function is decorated, it runs, it is
# declared hot, mojosub transpiles it, the transpiler says no — and in the
# service the *host* has already been asked for a five-second compile of a unit
# that will never load. Measured on the corpus in bench/corpus.py, a filter that
# only blacklists statement types lets 5 of 14 impossible cases through.
#
# So this is a whitelist of the transpiler's actual grammar rather than a
# blacklist of obvious offenders, plus the two checks a node-type test cannot
# express: every name has to be bound to something the transpiler can see, and
# every function called has to itself be compilable.
#
# Being wrong in the *restrictive* direction only costs a speedup, so where the
# two could disagree this side stays narrower than the transpiler. Every entry
# below corresponds to something `mojosub.transpile` can actually emit.

_EXPR_NODES = (
    ast.Constant, ast.Name, ast.UnaryOp, ast.BinOp, ast.BoolOp, ast.Compare,
    ast.IfExp, ast.Subscript, ast.Call, ast.Load, ast.Store, ast.Index,
    # operator tokens, which walk() yields as their own nodes
    ast.Add, ast.Sub, ast.Mult, ast.Div, ast.FloorDiv, ast.Mod, ast.Pow,
    ast.LShift, ast.RShift, ast.BitOr, ast.BitXor, ast.BitAnd,
    ast.UAdd, ast.USub, ast.Not, ast.And, ast.Or,
    ast.Eq, ast.NotEq, ast.Lt, ast.LtE, ast.Gt, ast.GtE,
)

# A tuple has exactly one meaning in the subset: the index of a 2-D buffer,
# `m[i, j]`. It is allowed as a node type and then checked for that shape below,
# because `a, b = b, a` and `return x, y` are not in the subset and never will be
# without a product type to return them in.
_INDEX_NODES = (ast.Tuple,)

_STMT_NODES = (
    ast.FunctionDef, ast.arguments, ast.arg, ast.Return, ast.Assign,
    ast.AnnAssign, ast.AugAssign, ast.If, ast.While, ast.For, ast.Pass,
    ast.Break, ast.Continue, ast.Expr,
)

_ALLOWED_NODES = _EXPR_NODES + _STMT_NODES + _INDEX_NODES

_ALLOWED_CALLS = frozenset({
    "int", "float", "bool", "abs", "min", "max", "len", "range",
    "sqrt", "exp", "log", "log2", "log10", "sin", "cos", "tan", "tanh",
    "atan", "asin", "acos", "sinh", "cosh", "floor", "ceil", "erf", "atan2",
    "pow",
})

# `math.sqrt` and `np.sqrt` resolve to the same emitted call; nothing else
# behind a dot does.
_MODULE_ROOTS = frozenset({"math", "np", "numpy"})

# The numeric constants the transpiler folds as literals (`mojosub`
# `MATH_CONSTS`). The filter must know them too, or every kernel that reads
# `math.pi` is rejected for `attribute .pi` despite the transpiler emitting
# it — the same failure mode as `math.sqrt` above. This set is deliberately
# the exact keys of the transpiler's table; adding a name here without
# adding it there would only cost a wasted transpile, never a wrong answer.
_MATH_CONSTS = frozenset({"pi", "e", "tau"})


def _bound_names(fn: ast.FunctionDef) -> set[str]:
    """Every name this function binds: parameters, assignments, loop targets.

    The transpiler has no environment beyond the function itself — a module
    global is not a constant it can fold, it is an `Unsupported: unbound name`.
    That is the single most common way a numeric-looking function fails to
    compile, because pulling a window size out to the top of the file is good
    Python style and invisible to a node-type check.
    """
    names = {a.arg for a in fn.args.args}
    for node in ast.walk(fn):
        if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Store):
            names.add(node.id)
        elif isinstance(node, ast.arg):
            names.add(node.arg)
    return names


def _annotation_names(fn: ast.FunctionDef) -> set[str]:
    """Names that appear only inside annotations, which are never evaluated."""
    out: set[str] = set()
    for tree in [a.annotation for a in fn.args.args] + [fn.returns]:
        if tree is None:
            continue
        for node in ast.walk(tree):
            if isinstance(node, ast.Name):
                out.add(node.id)
    return out


def module_constants(tree: ast.Module) -> frozenset[str]:
    """Module-level names the transpiler will fold in as literals.

    Kept in step with `mojosub.transpile.module_constants` — this side decides
    whether to *offer* the function, that side decides what the Mojo says, and
    a disagreement in this direction costs a wasted transpile rather than a
    wrong answer.
    """
    try:
        from mojosub.transpile import module_constants as folded
    except Exception:  # noqa: BLE001 - without mojosub nothing compiles anyway
        return frozenset()
    try:
        return frozenset(folded(tree))
    except Exception:  # noqa: BLE001 - a filter must never fail a run
        return frozenset()


def eligible(fn: ast.FunctionDef, local_names, _seen: frozenset[str] = frozenset(),
             consts: frozenset[str] = frozenset()) -> bool:
    """Whether this function should be handed to the transpiler.

    Two separate questions, and conflating them costs real speedups: *can* the
    transpiler take it (`compilable`), and is there anything to win. Only the
    second one needs a loop — a helper called from inside somebody else's loop
    is worth compiling with no loop of its own.

    `local_names` is the sibling top-level functions: a dict of name -> node
    when their bodies are available (so a call to one can be checked), or just
    their names, in which case such a call is trusted.
    """
    if not any(isinstance(n, (ast.For, ast.While)) for n in ast.walk(fn)):
        return False  # no loop, nothing to win
    return compilable(fn, local_names, _seen, consts)


def _module_call_nodes(fn: ast.FunctionDef) -> set[int]:
    """The nodes making up a `math.sqrt(x)` or `math.pi`, and nothing else.

    `math.sqrt` and `np.exp` are the most ordinary thing in a numeric kernel and
    the transpiler emits them, but an `Attribute` is not in the allowed node
    list and `math` is not a name the body binds — so the walk below used to
    reject the function twice over before it ever reached the branch written to
    accept it. Measured on this box the same loop is 67x faster native, and no
    function using `math.` had ever been compiled.

    `math.pi` / `math.e` / `math.tau` are the same story one step earlier:
    the transpiler folds them as literals, but the filter had no exception
    for a bare attribute read, so every kernel mentioning pi was rejected
    for `attribute .pi`. Financial and statistical kernels hit this
    constantly (log-returns, normal PDFs, moving-average spans).

    Both the attribute and its root are collected, so a `math` used as anything
    other than the root of an allowed call still fails the ordinary checks.
    """
    ok: set[int] = set()
    for node in ast.walk(fn):
        # Bare constant read: `math.pi` (not under a Call).
        if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name) \
                and node.value.id in _MODULE_ROOTS and node.attr in _MATH_CONSTS:
            ok.add(id(node))
            ok.add(id(node.value))
            continue
        if not isinstance(node, ast.Call) or not isinstance(node.func, ast.Attribute):
            continue
        func = node.func
        root = func.value
        if (isinstance(root, ast.Name) and root.id in _MODULE_ROOTS
                and func.attr in _ALLOWED_CALLS):
            ok.add(id(func))
            ok.add(id(root))
    return ok


def compilable(fn: ast.FunctionDef, local_names,
               _seen: frozenset[str] = frozenset(),
               consts: frozenset[str] = frozenset()) -> bool:
    """Whether the transpiler can emit this function at all."""
    return why_not(fn, local_names, _seen, consts) is None


def why_not(fn: ast.FunctionDef, local_names,
            _seen: frozenset[str] = frozenset(),
            consts: frozenset[str] = frozenset()) -> str | None:
    """The first thing that stops this function compiling, or None.

    A bool was enough while the only consumer was "decorate or do not". It is not
    enough for anything else: the converter puts this reason in the agent's
    prompt, and `bench/corpus_scan.py` counts reasons across a corpus. "no" is
    not a work item.
    """
    found = blockers(fn, local_names, _seen, consts, first_only=True)
    return found[0] if found else None


def blockers(fn: ast.FunctionDef, local_names,
             _seen: frozenset[str] = frozenset(),
             consts: frozenset[str] = frozenset(),
             first_only: bool = False) -> list[str]:
    """*Every* distinct reason this function cannot be compiled.

    All of them, not the first, because the useful question about a corpus is
    "how many functions would compile if I added X" — and a function blocked by
    both a method call and a list literal counts towards neither until both
    exist. Reporting only the first reason makes the second look rare, which is
    how a work queue ends up sorted by accident.
    """
    out: list[str] = []

    def note(reason: str) -> bool:
        """Record a blocker. Returns True when the caller should stop early."""
        if reason not in out:
            out.append(reason)
        return first_only

    args = fn.args
    if args.vararg or args.kwarg or args.kwonlyargs or args.defaults:
        if note("only positional parameters are supported"):
            return out

    bound = _bound_names(fn)
    module_calls = _module_call_nodes(fn)
    shape_reads = _shape_read_nodes(fn)
    index_tuples = _index_tuple_nodes(fn)
    # An annotation is not evaluated by the transpiler, so `list[float]` must
    # not make `list` look like a name the body reads.
    annotations = _annotation_names(fn)
    docstring = (fn.body[0].value
                 if fn.body and isinstance(fn.body[0], ast.Expr) else None)

    # The Name under a Call is reported as `call to X`; counting it again as an
    # unbound name doubles every callee and makes the numbers unusable.
    callee_names = {id(n.func) for n in ast.walk(fn)
                    if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)}
    for node in ast.walk(fn):
        if id(node) in module_calls:
            continue   # `math.sqrt` and the `math` under it
        if id(node) in shape_reads:
            continue   # `m.shape` under a `m.shape[0]`
        if isinstance(node, ast.alias):
            continue   # the `heapq` in `import heapq`, already counted as Import
        if id(node) in callee_names:
            continue
        if not isinstance(node, _ALLOWED_NODES):
            if note(f"statement {type(node).__name__}"):
                return out
            continue
        if isinstance(node, ast.Tuple) and id(node) not in index_tuples:
            # A tuple anywhere but a 2-D index: unpacking, a multiple return, a
            # literal. None of those are in the subset.
            if note("tuple outside a 2-D index"):
                return out
            continue
        if isinstance(node, ast.Constant):
            if node is docstring:
                continue
            # Numbers and booleans only: a string, a byte string or an Ellipsis
            # has no representation in the numeric subset.
            if not isinstance(node.value, (int, float, bool)):
                if note(f"{type(node.value).__name__} literal"):
                    return out
        elif isinstance(node, ast.Name):
            if isinstance(node.ctx, ast.Load) and node.id not in bound:
                if node.id in _ALLOWED_CALLS or node.id in local_names:
                    continue  # a callee, checked below
                if node.id in annotations or node.id in consts:
                    continue
                if note(f"unbound name {node.id}"):
                    return out
        elif isinstance(node, ast.Call):
            if node.keywords and note("keyword arguments"):
                return out
            func = node.func
            if isinstance(func, ast.Name):
                if func.id in _ALLOWED_CALLS:
                    continue
                if func.id not in local_names:
                    if note(f"call to {func.id}"):
                        return out
                    continue
                inner = _callee_reason(func.id, local_names,
                                       _seen | {fn.name}, consts)
                if inner is not None and note(f"helper {func.id}: {inner}"):
                    return out
            elif isinstance(func, ast.Attribute):
                root = func.value
                if not (isinstance(root, ast.Name) and root.id in _MODULE_ROOTS):
                    if note("method call"):
                        return out
                    continue
                if func.attr not in _ALLOWED_CALLS:
                    if note(f"call to {getattr(root, 'id', '?')}.{func.attr}"):
                        return out
            elif note("indirect call"):
                return out
        elif isinstance(node, ast.Attribute):
            # Reached only for an attribute that is neither a module call nor
            # the `.shape` of a buffer — `row.value`, `arr.dtype`. Nothing else
            # behind a dot is in the subset.
            if note(f"attribute .{node.attr}"):
                return out
    return out


def _shape_read_nodes(fn: ast.FunctionDef) -> set[int]:
    """Nodes belonging to a `<name>.shape[0]` / `[1]`, which the transpiler emits.

    A 2-D kernel written against numpy says `m.shape[1]`, and asking the caller
    to rewrite that in order to make it fast defeats the point. Only this exact
    shape is recognised: a bare name, `.shape`, and a literal 0 or 1.
    """
    out: set[int] = set()
    for node in ast.walk(fn):
        if not isinstance(node, ast.Subscript):
            continue
        value = node.value
        if not (isinstance(value, ast.Attribute) and value.attr == "shape"
                and isinstance(value.value, ast.Name)):
            continue
        if not (isinstance(node.slice, ast.Constant) and node.slice.value in (0, 1)):
            continue
        out.add(id(value))          # the Attribute
        out.add(id(value.value))    # the Name under it
        out.add(id(node.slice))     # the literal axis
    return out


def _index_tuple_nodes(fn: ast.FunctionDef) -> set[int]:
    """Tuples that are a 2-D subscript index, `m[i, j]` — the only legal ones."""
    out: set[int] = set()
    for node in ast.walk(fn):
        if isinstance(node, ast.Subscript) and isinstance(node.slice, ast.Tuple):
            if len(node.slice.elts) == 2:
                out.add(id(node.slice))
    return out


def _callee_reason(name: str, local_names, _seen: frozenset[str],
                   consts: frozenset[str] = frozenset()) -> str | None:
    """Why a call to a sibling makes the caller uncompilable, or None."""
    if name in _seen:
        return None
    if not isinstance(local_names, dict):
        return None
    node = local_names.get(name)
    if node is None:
        return "not defined in this module"
    if node.returns is None or any(a.annotation is None for a in node.args.args):
        return "not fully annotated, so it is not in the unit"
    return why_not(node, local_names, _seen, consts)


def _callee_ok(name: str, local_names, _seen: frozenset[str],
               consts: frozenset[str] = frozenset()) -> bool:
    """Whether a call to a sibling function keeps the caller compilable.

    A helper that is itself outside the subset takes its caller down with it —
    the transpiler emits both or neither, so the caller of an uncompilable
    helper is uncompilable however clean its own body is. Recursion is fine: a
    function already on the stack is being checked by the frame below.
    """
    if name in _seen:
        return True
    if not isinstance(local_names, dict):
        return True   # names only: nothing to check against
    node = local_names.get(name)
    if node is None:
        return False
    # mojosub builds helper signatures from annotations only, so an unannotated
    # helper is not in the unit at all and the call to it cannot be emitted.
    if node.returns is None or any(a.annotation is None for a in node.args.args):
        return False
    return compilable(node, local_names, _seen, consts)


def accelerate(tree: ast.Module) -> list[str]:
    """Decorate eligible top-level functions in `tree`, in place.

    The AST is rewritten rather than the functions wrapped afterwards, because
    a function already captured by a plain reference would keep the unwrapped
    one.
    """
    # The filter runs first, on the AST alone. Most programs have nothing worth
    # compiling, and for those the whole machinery — importing mojosub, asking
    # the toolchain who it is — is pure latency they should not pay.
    local_names = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
    consts = module_constants(tree)
    candidates = [n for n in tree.body
                  if isinstance(n, ast.FunctionDef) and not n.decorator_list
                  and eligible(n, local_names, consts=consts)]
    if not candidates or not enabled():
        return []
    for node in candidates:
        node.decorator_list = [ast.Name(id=DECORATOR, ctx=ast.Load())]
    ast.fix_missing_locations(tree)
    return [n.name for n in candidates]


def namespace() -> dict[str, Any]:
    """Names this module contributes to every run."""
    return {DECORATOR: jit, "mojo": jit}


def drain(ns_after: dict[str, Any], budget_ms: float = WARM_MS) -> float:
    """Wait (briefly, unbilled) for background compiles to reach the cache.

    A `@jit` function compiles on a daemon thread while the program keeps
    running. Short programs exit first, the thread dies, and the cache entry is
    never written — so the same kernel submitted tomorrow would compile from
    scratch again, forever. Holding the process open for a few seconds after
    the user's code has finished is what turns the content-addressed cache into
    something that actually accumulates.
    """
    if budget_ms <= 0:
        return 0.0
    import time

    pending = [v for v in ns_after.values() if hasattr(v, "_pending")]
    if not pending:
        return 0.0
    started = time.perf_counter()
    deadline = started + budget_ms / 1000.0
    while time.perf_counter() < deadline:
        if not any(fn._pending for fn in pending):
            break
        time.sleep(0.01)
    return round((time.perf_counter() - started) * 1000.0, 3)


# Below this many copied elements the ABI copy is not worth mentioning; the
# advice would be noise on a kernel that takes a 50-element list.
HINT_MIN_ELEMENTS = int(os.environ.get("MOJOJOJO_HINT_MIN_ELEMENTS", "10000"))


def hints(ns_after: dict[str, Any]) -> list[str]:
    """Things the caller could change that we cannot.

    There is exactly one of these worth saying today, and it is worth saying
    loudly: a `list` argument is copied element by element to cross the C ABI,
    while anything exposing `__array_interface__` is passed by address. Measured
    on this machine, the same reduction over a million float64s is 2.2x on a
    list and 147x on a numpy array — the copy is the whole difference, it is
    invisible in the user's own code, and they are the only one who can remove
    it.
    """
    out: list[str] = []
    for name, value in ns_after.items():
        cost = getattr(value, "copy_cost", None)
        if cost is None:
            continue
        try:
            elements, params = cost()
        except Exception:  # noqa: BLE001 - a hint must never fail a run
            continue
        if elements < HINT_MIN_ELEMENTS:
            continue
        which = ", ".join(params) if params else "its sequence arguments"
        out.append(
            f"{name}() copied {elements:,} elements across the native boundary "
            f"because {which} arrived as Python lists. Passing numpy arrays (or "
            f"array.array('d', ...)) hands them over by address instead — "
            f"measured up to 100x faster on the same kernel."
        )
    return out


def stats(ns_after: dict[str, Any]) -> dict[str, Any]:
    """Summarize what acceleration did, for the run trace."""
    compiled = native_calls = fallbacks = cache_hits = 0
    compile_seconds = 0.0
    queued = 0
    last_error = None
    for value in ns_after.values():
        st = getattr(value, "stats", None)
        if st is None or not hasattr(st, "mojo_calls"):
            continue
        compiled += st.compiles
        native_calls += st.mojo_calls
        fallbacks += st.verify_failures
        cache_hits += st.cache_hits
        compile_seconds += st.compile_seconds
        if st.last_error and "queued for compilation" in str(st.last_error):
            queued += 1
        last_error = last_error or st.last_error
    advice = hints(ns_after)
    if not (compiled or native_calls or cache_hits or last_error or queued or advice):
        return {}
    out = {"compiled": compiled, "native_calls": native_calls,
           "cache_hits": cache_hits, "verify_failures": fallbacks,
           # Billing subtracts this: the compiler runs on our clock, not the
           # caller's, even when it overlaps their run.
           "compile_ms": round(compile_seconds * 1000.0, 3)}
    if advice:
        out["hints"] = advice
    if last_error:
        text = str(last_error)
        if "queued for compilation" in text:
            # Not a failure: the sandbox has no compiler by design, so a first
            # sighting of a kernel is a request the host fills afterwards.
            out["queued"] = queued
        else:
            # Surfaced, not raised: the answer the caller got is CPython's
            # either way, but "why is my kernel not native" needs an answer.
            out["error"] = text[:400]
    return out
