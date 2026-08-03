"""What the accelerated path does to ordinary Python, program by program.

Two questions, asked separately because they have different costs and different
consequences:

1. *Does the filter agree with the transpiler?* Cheap — no compiler runs. Being
   wrong in the permissive direction costs five seconds of host compiler for a
   unit nobody loads; being wrong the other way only costs a speedup. The cases
   below are labelled with which functions should be offered for compilation.

2. *Does compiling change the answer?* The expensive one, and the only one that
   matters to somebody's bill: every program in `DIFFERENTIAL` is run twice
   through the real sandbox — once with acceleration and once without — and the
   two must print exactly the same thing. A compile that finishes and returns a
   different number is worse than no compile at all.

The second set really does build. Units are content-addressed, so the first run
of this file is slow and every run after it is not.
"""

from __future__ import annotations

import ast
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import accel  # noqa: E402
import envs  # noqa: E402
import sandbox  # noqa: E402


@pytest.fixture(scope="session")
def python_exe():
    exe, _ = envs.ensure(["numpy"])
    return exe


# --- 1. what the filter offers ---------------------------------------------
#
# (name, source, functions that should be decorated). A function is offered
# only if the transpiler can take it *and* there is something to win.

OFFERED: list[tuple[str, str, list[str]]] = [
    ("scalar loop", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i
    return t
""", ["k"]),
    ("float accumulate", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += i * 0.5
    return t
""", ["k"]),
    ("nested loops", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        for j in range(i):
            t += j
    return t
""", ["k"]),
    ("while with break", """
def k(n: int) -> int:
    t = 0
    i = 0
    while True:
        if i >= n:
            break
        t += i
        i += 1
    return t
""", ["k"]),
    ("continue and elif", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        if i % 3 == 0:
            continue
        elif i % 5 == 0:
            t += 2
        else:
            t += 1
    return t
""", ["k"]),
    ("integer division and power", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += (i // 3) % 7 + i ** 2
    return t
""", ["k"]),
    ("boolean operators", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        if i > 2 and i < 100 or i == 0:
            t += 1
    return t
""", ["k"]),
    ("conditional expression", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += 1.0 if i % 2 == 0 else 0.5
    return t
""", ["k"]),
    ("builtins in the subset", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += min(float(i), 10.0) + max(abs(-i), 1)
    return t
""", ["k"]),
    ("math module calls", """
import math
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += math.sqrt(i) + math.exp(-0.001 * i)
    return t
""", ["k"]),
    ("numpy scalar calls", """
import numpy as np
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += np.log(i + 1.0)
    return t
""", ["k"]),
    ("module constant", """
WINDOW = 32
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i % WINDOW
    return t
""", ["k"]),
    ("annotated helper, both offered", """
def helper(x: float) -> float:
    return x * 2.0 + 1.0
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += helper(i * 1.0)
    return t
""", ["k"]),
    ("buffer argument", """
def k(xs, n: int) -> float:
    t = 0.0
    for i in range(n):
        t += xs[i]
    return t
""", ["k"]),
    ("writes through a buffer", """
def k(xs, n: int) -> int:
    for i in range(n):
        xs[i] = xs[i] * 2.0
    return n
""", ["k"]),
    ("recursion with a loop", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i
    if n > 1:
        t += k(n - 1)
    return t
""", ["k"]),
    # --- and what it must not offer ---
    ("no loop, nothing to win", """
def k(a: float, b: float) -> float:
    return a * b + 1.0
""", []),
    ("string work", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += len(str(i))
    return t
""", []),
    ("list append", """
def k(n: int) -> int:
    out = []
    for i in range(n):
        out.append(i)
    return len(out)
""", []),
    ("dict accumulate", """
def k(n: int) -> int:
    seen = {}
    for i in range(n):
        seen[i % 7] = i
    return len(seen)
""", []),
    ("try/except", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        try:
            t += 1
        except ValueError:
            pass
    return t
""", []),
    ("comprehension", """
def k(n: int) -> int:
    return sum([i * i for i in range(n)])
""", []),
    ("f-string", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += len(f"{i}")
    return t
""", []),
    ("attribute access on a value", """
def k(xs, n: int) -> float:
    t = 0.0
    for i in range(n):
        t += xs.mean()
    return t
""", []),
    ("tuple unpacking", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        a, b = i, i + 1
        t += a + b
    return t
""", []),
    ("enumerate", """
def k(xs) -> float:
    t = 0.0
    for i, x in enumerate(xs):
        t += x * i
    return t
""", []),
    ("unbound global that is not a constant", """
import numpy as np
TABLE = np.arange(10)
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += TABLE[i % 10]
    return t
""", []),
    ("rebound module name", """
LIMIT = 10
LIMIT = 20
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += LIMIT
    return t
""", []),
    ("keyword argument in a call", """
def helper(x: float, scale: float) -> float:
    return x * scale
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += helper(i * 1.0, scale=2.0)
    return t
""", []),
    ("unannotated helper drags the caller down", """
def helper(x):
    return x * 2
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += helper(i)
    return t
""", []),
    ("helper outside the subset drags the caller down", """
def helper(x: int) -> int:
    return len(str(x))
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += helper(i)
    return t
""", []),
    ("default argument", """
def k(n: int = 10) -> int:
    t = 0
    for i in range(n):
        t += i
    return t
""", []),
    ("varargs", """
def k(*sizes) -> int:
    t = 0
    for s in sizes:
        t += s
    return t
""", []),
    ("generator", """
def k(n: int):
    for i in range(n):
        yield i
""", []),
    ("closure over a local", """
def outer(n: int) -> int:
    def inner(x: int) -> int:
        return x + n
    t = 0
    for i in range(n):
        t += inner(i)
    return t
""", []),
    ("global statement", """
COUNT = 0
def k(n: int) -> int:
    global COUNT
    for i in range(n):
        COUNT += 1
    return COUNT
""", []),
    ("math function we do not emit", """
import math
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += math.gamma(i * 0.01 + 1.0)
    return t
""", []),
    ("nested attribute call", """
import numpy as np
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += np.random.rand()
    return t
""", []),
    ("already decorated", """
def deco(f):
    return f
@deco
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i
    return t
""", []),
]


@pytest.mark.parametrize("name, source, expected",
                         OFFERED, ids=[c[0] for c in OFFERED])
def test_the_filter_offers_exactly_these(name, source, expected):
    if not accel.enabled():
        pytest.skip("no Mojo toolchain on this machine")
    tree = ast.parse(source)
    assert accel.accelerate(tree) == expected


@pytest.mark.parametrize("name, source, expected",
                         OFFERED, ids=[c[0] for c in OFFERED])
def test_anything_offered_can_actually_be_transpiled(name, source, expected):
    """The expensive half of being wrong: a function the filter offers and the
    transpiler then refuses has cost a host compile for nothing."""
    if not expected:
        return
    mojosub = pytest.importorskip("mojosub")
    tree = ast.parse(source)
    entry = expected[0]
    fndef = next(n for n in tree.body
                 if isinstance(n, ast.FunctionDef) and n.name == entry)
    sig = mojosub.transpile.signature_from_annotations(fndef)
    if sig is None:
        return  # buffer arguments are typed at call time, not from annotations
    sigs = {entry: sig}
    for node in tree.body:
        if isinstance(node, ast.FunctionDef) and node.name != entry:
            helper = mojosub.transpile.signature_from_annotations(node)
            if helper is not None:
                sigs[node.name] = helper
    mojosub.transpile.transpile(tree, sigs, entry)


# --- 2. compiling must not change the answer -------------------------------
#
# Every one of these is run twice through the real sandbox, accelerated and
# not. Anything that differs is a wrong answer somebody is being charged for.

DIFFERENTIAL: list[tuple[str, str]] = [
    ("integer accumulation", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i * i - i // 2
    return t
print(k(50000), k(1), k(0))
"""),
    ("float accumulation", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += i * 0.25 - 1.5
    return t
print(round(k(50000), 6), round(k(3), 6))
"""),
    ("mixed int and float", """
def k(n: int) -> float:
    t = 0.0
    c = 0
    for i in range(n):
        c += 1
        t += c / (i + 1.0)
    return t
print(round(k(20000), 8))
"""),
    ("branchy", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        if i % 3 == 0:
            t += 2
        elif i % 5 == 0:
            t -= 1
        else:
            t += 1
    return t
print(k(30000), k(7))
"""),
    ("while loop with break and continue", """
def k(n: int) -> int:
    t = 0
    i = 0
    while i < n:
        i += 1
        if i % 2 == 0:
            continue
        if i > n - 3:
            break
        t += i
    return t
print(k(40000), k(2))
"""),
    ("nested loops", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        for j in range(3):
            t += i * j
    return t
print(k(20000))
"""),
    ("math module", """
import math
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        x = i * 0.001
        t += math.sqrt(x) + math.exp(-x) + math.log(x + 1.0)
    return t
print(round(k(30000), 6))
"""),
    ("module constants", """
WINDOW = 16
SCALE = 0.125
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += (i % WINDOW) * SCALE
    return t
print(round(k(30000), 6))
"""),
    ("helper call", """
def helper(x: float) -> float:
    return x * x + 1.0
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += helper(i * 0.01)
    return t
print(round(k(20000), 6))
"""),
    ("list argument", """
def k(xs, n: int) -> float:
    t = 0.0
    for i in range(n):
        t += xs[i]
    return t
data = [i * 0.5 for i in range(5000)]
print(round(k(data, len(data)), 6))
"""),
    ("numpy array argument", """
import numpy as np
def k(xs, n: int) -> float:
    t = 0.0
    for i in range(n):
        t += xs[i] * 2.0
    return t
data = np.arange(5000, dtype=np.float64) / 7.0
print(round(k(data, len(data)), 6))
"""),
    ("writes through a buffer", """
import numpy as np
def k(xs, n: int) -> int:
    for i in range(n):
        xs[i] = xs[i] * 2.0 + 1.0
    return n
data = np.arange(2000, dtype=np.float64)
k(data, len(data))
print(round(float(data.sum()), 6), round(float(data[0]), 6), round(float(data[-1]), 6))
"""),
    ("called once, not just in a loop", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += i % 17
    return t
print(k(100000))
"""),
    ("negative numbers and unary minus", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += -abs(i - 500) * 0.5
    return t
print(round(k(20000), 6))
"""),
    ("zero iterations", """
def k(n: int) -> int:
    t = 0
    for i in range(n):
        t += 1
    return t
print(k(0), k(1), k(2))
"""),
    ("an exception still comes from the user's code", """
def k(n: int) -> float:
    t = 0.0
    for i in range(n):
        t += 1.0 / (n - i - 1)
    return t
print(k(5))
"""),
    ("integers big enough to leave 64 bits", """
def k(n: int) -> int:
    t = 1
    for i in range(n):
        t = t * 3 + 1
    return t
print(k(60))
"""),
]


# Cases where native and CPython genuinely disagree, and the disagreement is
# supposed to be caught rather than prevented. Mojo's Int is 64 bits and
# CPython's promotes, so this program *must* end up back on the interpreter —
# with the right answer, and with the variant retired rather than reused.
EXPECTED_TO_DIVERGE = {"integers big enough to leave 64 bits"}


def _run(python_exe, code: str, accelerated: bool):
    return sandbox.run(python=python_exe, code=code, timeout_ms=60000,
                       accel=accelerated)


@pytest.mark.parametrize("name, source",
                         DIFFERENTIAL, ids=[c[0] for c in DIFFERENTIAL])
def test_acceleration_never_changes_the_answer(python_exe, name, source):
    plain = _run(python_exe, source, False)
    # Twice: the first accelerated run may queue the compile, the second is the
    # one that actually dispatches into it.
    _run(python_exe, source, True)
    fast = _run(python_exe, source, True)

    assert fast["stdout"] == plain["stdout"], (
        f"{name}: accelerated {fast['stdout']!r} != CPython {plain['stdout']!r}")
    assert fast["exit_code"] == plain["exit_code"]
    failures = fast["accel"].get("verify_failures", 0)
    if name in EXPECTED_TO_DIVERGE:
        # The answer above is already CPython's. This is the guard that made it
        # so, and a run where it stopped firing would be a wrong answer.
        assert failures >= 1, "the 64-bit divergence was not caught"
    else:
        assert failures == 0, fast["accel"]


@pytest.mark.parametrize("name, source",
                         DIFFERENTIAL, ids=[c[0] for c in DIFFERENTIAL])
def test_nothing_in_the_matrix_fails_outright(python_exe, name, source):
    """A program in this corpus that errors is a corpus bug, not a finding —
    and an accelerated run that errors where CPython did not is the finding."""
    plain = _run(python_exe, source, False)
    if plain["exit_code"] == 0:
        assert _run(python_exe, source, True)["exit_code"] == 0, name
