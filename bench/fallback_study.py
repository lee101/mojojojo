"""What does it cost to find out that a function has to run on CPython?

mojojojo hands every submitted program to an eligibility filter, and whatever
survives goes to mojosub, which transpiles it, compiles it (~5 s of host
compiler), and then races the compiled variant against CPython before trusting
it. Each of those stages can say no. The later it says no, the more it cost:

    filter      microseconds, free
    transpile   ~1 ms, and it has already decorated the function
    compile     ~5 s of host compiler, plus a cache entry nobody will use
    race        one extra interpreted call, per signature, forever if not
                persisted

So the question this measures is not "is Mojo fast" — it is "how early do we
know". Run it against the corpus:

    bench/run.sh bench/fallback_study.py                 # stages, no compiling
    bench/run.sh bench/fallback_study.py --compile       # + real builds (slow)
    bench/run.sh bench/fallback_study.py --crossover     # list-vs-array sizes
    bench/run.sh bench/fallback_study.py --json out.json

Every number printed is measured here, on this machine, now.
"""

from __future__ import annotations

import argparse
import ast
import contextlib
import io
import json
import os
import statistics
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "runner"))

from bench.corpus import CASES, Case  # noqa: E402
import accel  # noqa: E402


def timed(fn, *args, repeat: int = 5):
    """Best-of-N wall clock, in milliseconds. Best, not mean: we are measuring
    how long the work takes, and a descheduled sample measures the scheduler."""
    best = float("inf")
    value = None
    for _ in range(repeat):
        t0 = time.perf_counter()
        value = fn(*args)
        best = min(best, (time.perf_counter() - t0) * 1000.0)
    return value, best


def define(case: Case):
    """Exec a case's source and hand back the entry function.

    `__mojosub_source__` goes into the namespace because user code is exec'd
    from an AST and `inspect.getsource` has nothing to read — the same reason
    the runner passes `source=` to `jit`. Without it every compile in here dies
    with "could not get source code", which is a property of the harness and
    not of the thing being measured.
    """
    namespace: dict = {"__mojosub_source__": case.source}
    with quiet():
        exec(compile(ast.parse(case.source), "<study>", "exec"), namespace)  # noqa: S102
    return namespace[case.entry], namespace


@contextlib.contextmanager
def quiet():
    """Swallow a probed function's own output. Some corpus cases print, and a
    study whose report is interleaved with the corpus is unreadable."""
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
        yield


# --------------------------------------------------------------- the stages
def stage_filter(case: Case) -> tuple[bool, float]:
    """What the cheap AST filter says, and how long it took to say it."""
    tree = ast.parse(case.source)
    # name -> node, exactly what accel.accelerate() passes: a call to a sibling
    # can only be judged against that sibling's body.
    names = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
    fn = next(n for n in tree.body
              if isinstance(n, ast.FunctionDef) and n.name == case.entry)
    # Module-level constants are folded into the emitted Mojo, so the filter
    # has to be asked the same question `accelerate` asks — measuring it
    # without them reports a gap the service does not have.
    consts = accel.module_constants(tree)

    def run():
        return accel.eligible(fn, names, consts=consts)

    verdict, ms = timed(run, repeat=50)
    return bool(verdict), ms


def stage_transpile(case: Case) -> tuple[bool, str, float]:
    """What the transpiler says for this case's real argument types."""
    from mojosub.api import type_of
    from mojosub.transpile import (Signature, Unsupported,
                                   signature_from_annotations, transpile)
    from mojosub.types import NONE

    args = case.args()
    tree = ast.parse(case.source)
    fndef = next((n for n in tree.body
                  if isinstance(n, ast.FunctionDef) and n.name == case.entry), None)
    if fndef is None:
        return False, "no such function", 0.0

    def run():
        try:
            argtypes = [type_of(a) for a in args]
        except Unsupported as exc:
            return False, f"argument: {exc}"
        annotated = signature_from_annotations(fndef)
        ret = annotated.ret if annotated else None
        if ret is None or ret is NONE:
            # The real pipeline learns this by running the function once; here
            # the return type is taken from the annotation or probed, because
            # what is being measured is the transpiler, not the tier below it.
            namespace: dict = {}
            try:
                with quiet():
                    exec(compile(tree, "<study>", "exec"), namespace)  # noqa: S102
                    probe = namespace[case.entry](*case.args())
                ret = type_of(probe) if probe is not None else NONE
            except Exception as exc:  # noqa: BLE001
                return False, f"probe: {type(exc).__name__}: {exc}"
        names = [a.arg for a in fndef.args.args]
        sigs = {n.name: signature_from_annotations(n)
                for n in tree.body
                if isinstance(n, ast.FunctionDef) and n.name != case.entry}
        sigs = {k: v for k, v in sigs.items() if v is not None}
        sigs[case.entry] = Signature(case.entry, list(zip(names, argtypes)), ret)
        try:
            transpile(tree, sigs, case.entry)
        except Unsupported as exc:
            return False, str(exc)
        except Exception as exc:  # noqa: BLE001
            return False, f"{type(exc).__name__}: {exc}"
        return True, ""

    (ok, reason), ms = timed(run, repeat=3)
    return ok, reason, ms


def stage_race(case: Case) -> dict:
    """Compile for real and time both tiers. This is the expensive truth."""
    from mojosub.api import compile_function, type_of
    from mojosub.transpile import Unsupported
    from mojosub.types import NONE

    fn, _ns = define(case)

    try:
        argtypes = [type_of(a) for a in case.args()]
        with quiet():
            probe = fn(*case.args())
        ret = type_of(probe) if probe is not None else NONE
    except Exception as exc:  # noqa: BLE001
        return {"compiled": False, "error": f"{type(exc).__name__}: {exc}"}

    started = time.perf_counter()
    try:
        call, _src = compile_function(fn, argtypes, ret)
    except Unsupported as exc:
        return {"compiled": False, "error": str(exc),
                "compile_ms": (time.perf_counter() - started) * 1000.0}
    except Exception as exc:  # noqa: BLE001
        return {"compiled": False, "error": f"{type(exc).__name__}: {exc}",
                "compile_ms": (time.perf_counter() - started) * 1000.0}
    compile_ms = (time.perf_counter() - started) * 1000.0

    # Fresh arguments per tier: a kernel that writes through a buffer must not
    # see the other tier's writes.
    py = [timed(fn, *case.args(), repeat=1)[1] for _ in range(3)]
    nat = [timed(call.dispatch, *case.args(), repeat=1)[1] for _ in range(3)]
    python_ms = statistics.median(py)
    native_ms = statistics.median(nat)
    return {
        "compiled": True,
        "compile_ms": compile_ms,
        "python_ms": python_ms,
        "native_ms": native_ms,
        "speedup": (python_ms / native_ms) if native_ms > 0 else None,
    }


# ------------------------------------------------------------------- report
def study(with_compile: bool) -> list[dict]:
    rows = []
    for case in CASES:
        row: dict = {"name": case.name, "label": case.label, "why": case.why,
                     "tags": list(case.tags)}
        row["filter_ok"], row["filter_ms"] = stage_filter(case)
        ok, reason, ms = stage_transpile(case)
        row["transpile_ok"], row["transpile_reason"], row["transpile_ms"] = ok, reason, ms
        if with_compile and ok:
            row.update(stage_race(case))
        rows.append(row)
    return rows


def verdict_of(row: dict) -> str:
    """What the pipeline would actually do with this case."""
    if not row["filter_ok"]:
        return "cpython"
    if not row["transpile_ok"]:
        return "cpython"
    if "speedup" in row and row.get("speedup") is not None:
        return "native" if row["speedup"] > 1.0 else "cpython"
    return "native"


def report(rows: list[dict], with_compile: bool) -> int:
    wide = max(len(r["name"]) for r in rows)
    print(f"\n{'case':<{wide}}  filter  transpile  {'measured':>22}  want")
    print("-" * (wide + 56))
    wasted_transpiles = 0
    wasted_compiles = 0
    wrong = []
    for r in rows:
        f = "pass" if r["filter_ok"] else "STOP"
        t = "pass" if r["transpile_ok"] else "STOP"
        measured = ""
        if r.get("compiled"):
            measured = (f"{r['python_ms']:8.2f}ms py {r['native_ms']:7.2f}ms mojo "
                        f"{r['speedup']:6.1f}x")
        elif r["transpile_ok"] and with_compile:
            measured = f"compile failed: {r.get('error', '')[:30]}"
        got = verdict_of(r)
        mark = " " if (got == r["label"] or r["label"] == "either") else "!"
        print(f"{r['name']:<{wide}}  {f:<6}  {t:<9}  {measured:>22}  {r['label']}{mark}")
        if r["filter_ok"] and not r["transpile_ok"]:
            wasted_transpiles += 1
        if r.get("compiled") and r.get("speedup") is not None and r["speedup"] < 1.0:
            wasted_compiles += 1
        if mark == "!":
            wrong.append((r["name"], got, r["label"], r["transpile_reason"] or ""))

    n = len(rows)
    should_stop = [r for r in rows if r["label"] == "cpython"]
    stopped_early = [r for r in should_stop if not r["filter_ok"]]
    filter_ms = statistics.median(r["filter_ms"] for r in rows)
    transpile_ms = statistics.median(r["transpile_ms"] for r in rows if r["transpile_ms"])

    print(f"\n{n} cases. Filter cost {filter_ms * 1000:.0f}us median, "
          f"transpile {transpile_ms:.2f}ms median.")
    print(f"Of {len(should_stop)} cases that must end on CPython, the cheap filter "
          f"catches {len(stopped_early)} ({len(stopped_early) / max(1, len(should_stop)):.0%}).")
    print(f"{wasted_transpiles} case(s) passed the filter and then failed to transpile "
          f"— work done for nothing.")
    if with_compile:
        print(f"{wasted_compiles} case(s) compiled successfully and were still slower "
              f"than CPython — ~5s of compiler each, for a variant that must be retired.")
    if wrong:
        print("\nDisagreements with the hand label:")
        for name, got, want, reason in wrong:
            print(f"  {name}: pipeline says {got}, want {want}"
                  + (f" ({reason})" if reason else ""))
    return len(wrong)


def crossover() -> None:
    """Where a copied list stops being worth compiling, by size.

    This is the measurement the argument-shape heuristic is built on: the
    element count at which crossing the ABI costs more than the loop saves.
    """
    from mojosub.api import compile_function, type_of
    from mojosub.types import FLOAT

    src = '''
def dot(a, b) -> float:
    total = 0.0
    for i in range(len(a)):
        total += a[i] * b[i]
    return total
'''
    namespace: dict = {"__mojosub_source__": src}
    exec(compile(src, "<study>", "exec"), namespace)  # noqa: S102
    fn = namespace["dot"]

    print("\nlist vs buffer, same kernel, same element count")
    print(f"{'n':>10}  {'python':>9}  {'mojo(list)':>11}  {'speedup':>8}  "
          f"{'mojo(array)':>12}  {'speedup':>8}")
    for n in (1_000, 10_000, 100_000, 400_000, 1_000_000):
        from bench.corpus import _f64, _list_f64
        la, lb = _list_f64(n, 1), _list_f64(n, 2)
        aa, ab = _f64(n, 1), _f64(n, 2)
        py = statistics.median(timed(fn, la, lb, repeat=1)[1] for _ in range(3))
        call_l, _ = compile_function(fn, [type_of(la), type_of(lb)], FLOAT)
        call_a, _ = compile_function(fn, [type_of(aa), type_of(ab)], FLOAT)
        ml = statistics.median(timed(call_l.dispatch, la, lb, repeat=1)[1] for _ in range(3))
        ma = statistics.median(timed(call_a.dispatch, aa, ab, repeat=1)[1] for _ in range(3))
        print(f"{n:>10}  {py:8.2f}ms  {ml:10.2f}ms  {py / ml:7.2f}x  "
              f"{ma:11.2f}ms  {py / ma:7.2f}x")


def opsweep() -> None:
    """How much arithmetic per element pays for copying the element in.

    This is the measurement the whole argument-shape heuristic rests on. A
    `list` argument is copied element by element across the C ABI; a numpy array
    is passed by address. So for a copied argument:

        native  = n * copy_cost + n * ops * native_op
        python  = n * ops * python_op

    Native wins when `ops > copy_cost / (python_op - native_op)` — a threshold
    on arithmetic *per element*, with n cancelling out. If that is true, the
    crossover should sit at the same ops count for every array size, and the
    heuristic can be a static op count rather than a size check.
    """
    from mojosub.api import compile_function, type_of
    from mojosub.types import FLOAT
    from bench.corpus import _f64, _list_f64

    def kernel(ops: int) -> str:
        # Each extra term is one multiply and one add on the same loaded value,
        # so `ops` counts arithmetic per element and nothing else varies.
        terms = " + ".join(f"x * {1.0 + i}" for i in range(ops))
        return f'''
def work(xs) -> float:
    total = 0.0
    for i in range(len(xs)):
        x = xs[i]
        total += {terms}
    return total
'''

    print("\narithmetic per element vs the cost of copying it (n cancels out?)")
    print(f"{'ops':>4}  {'n':>9}  {'python':>9}  {'mojo(list)':>11}  {'list x':>7}  "
          f"{'mojo(arr)':>10}  {'arr x':>7}")
    for ops in (1, 2, 4, 8, 16, 32):
        src = kernel(ops)
        ns = {"__mojosub_source__": src}
        exec(compile(src, "<study>", "exec"), ns)  # noqa: S102
        fn = ns["work"]
        for n in (100_000, 1_000_000):
            lst = _list_f64(n, 1)
            arr = _f64(n, 1)
            py = statistics.median(timed(fn, lst, repeat=1)[1] for _ in range(3))
            call_l, _ = compile_function(fn, [type_of(lst)], FLOAT)
            call_a, _ = compile_function(fn, [type_of(arr)], FLOAT)
            ml = statistics.median(timed(call_l.dispatch, lst, repeat=1)[1] for _ in range(3))
            ma = statistics.median(timed(call_a.dispatch, arr, repeat=1)[1] for _ in range(3))
            flag = "  <- list crossover" if 0.9 < py / ml < 1.4 else ""
            print(f"{ops:>4}  {n:>9}  {py:8.2f}ms  {ml:10.2f}ms  {py / ml:6.2f}x  "
                  f"{ma:9.2f}ms  {py / ma:6.2f}x{flag}")


def copyback() -> None:
    """What the needless copy back off a read-only list argument costs.

    A sequence that is not already a buffer is copied into one for the call.
    Copying it *back* afterwards is only meaningful if the compiled code stored
    through it — for a reduction it cannot have. `Signature.writes` says which,
    so this measures the same kernel with the copy back forced on and off.
    """
    from mojosub.api import compile_function, type_of
    from mojosub.types import FLOAT
    from bench.corpus import _list_f64

    src = '''
def dot(a, b) -> float:
    total = 0.0
    for i in range(len(a)):
        total += a[i] * b[i]
    return total
'''
    ns = {"__mojosub_source__": src}
    exec(compile(src, "<study>", "exec"), ns)  # noqa: S102
    fn = ns["dot"]

    print("\nread-only list arguments: with and without the copy back")
    print(f"{'n':>9}  {'python':>9}  {'copy back':>10}  {'no copy back':>13}  {'saved':>7}")
    for n in (100_000, 400_000, 1_000_000):
        a, b = _list_f64(n, 1), _list_f64(n, 2)
        py = statistics.median(timed(fn, a, b, repeat=1)[1] for _ in range(3))
        call, _ = compile_function(fn, [type_of(a), type_of(b)], FLOAT)
        # The analysed signature says neither argument is written. Forcing
        # `writes = None` is how the runtime behaved before it was analysed.
        analysed = call.sig.writes
        call.sig.writes = None
        call._may_write = [True, True]
        before = statistics.median(timed(call, a, b, repeat=1)[1] for _ in range(3))
        call.sig.writes = analysed
        call._may_write = [False, False]
        after = statistics.median(timed(call, a, b, repeat=1)[1] for _ in range(3))
        print(f"{n:>9}  {py:8.2f}ms  {before:9.2f}ms  {after:12.2f}ms  "
              f"{(1 - after / before) * 100:6.0f}%")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--compile", action="store_true",
                    help="actually build and race each eligible case (slow)")
    ap.add_argument("--crossover", action="store_true",
                    help="measure the list-vs-buffer crossover by size")
    ap.add_argument("--opsweep", action="store_true",
                    help="measure the arithmetic-per-element threshold")
    ap.add_argument("--copyback", action="store_true",
                    help="measure the copy back off read-only list arguments")
    ap.add_argument("--json", help="write the raw rows here")
    args = ap.parse_args()

    if not accel.enabled():
        print("mojosub unavailable — set MOJOSUB_MOJO (see bench/run.sh)",
              file=sys.stderr)
        return 2

    rows = study(args.compile)
    disagreements = report(rows, args.compile)
    if args.crossover:
        crossover()
    if args.opsweep:
        opsweep()
    if args.copyback:
        copyback()
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(rows, fh, indent=2)
        print(f"\nwrote {args.json}")
    return 0 if disagreements == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
