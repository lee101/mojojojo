"""Programs of the shape users actually submit, labelled with what should
happen to them.

The label is the *desired* outcome, decided by hand and by measurement, not by
whatever the current code does — that is the whole point of having it. Three
values:

  "native"  — the transpiler can take this and native is genuinely faster.
  "cpython" — it cannot, or native loses. Deciding this WITHOUT paying for a
              transpile, a compile, or a timing race is what the heuristics are
              for.
  "either"  — small or ambiguous; whichever tier runs it, nobody is harmed.

`args` is a callable so each case can build fresh inputs (a kernel that writes
through its buffer must not be handed one another case already mutated).
"""

from __future__ import annotations

import array
from dataclasses import dataclass, field

try:
    import numpy as np
except ImportError:  # noqa: BLE001 - numpy cases are skipped without it
    np = None


@dataclass
class Case:
    name: str
    entry: str
    source: str
    args: object            # callable -> tuple
    label: str
    why: str = ""
    # Whether this case's cost depends on argument size (so the study can
    # report the crossover rather than one point on it).
    scales: bool = False
    tags: tuple = field(default_factory=tuple)


def _f64(n: int, seed: int = 7):
    if np is not None:
        rng = np.random.default_rng(seed)
        return rng.normal(0.0004, 0.01, n)
    return array.array("d", [((i * 37 % 1000) / 1000.0 - 0.5) for i in range(n)])


def _list_f64(n: int, seed: int = 7):
    return list(_f64(n, seed))


def _matrix(rows: int, cols: int, seed: int = 11):
    """A contiguous row-major feature matrix, which is what 2-D support takes."""
    if np is None:
        return None
    rng = np.random.default_rng(seed)
    return np.ascontiguousarray(rng.normal(0.0, 1.0, (rows, cols)))


def _labels(n: int, seed: int = 12):
    if np is None:
        return None
    rng = np.random.default_rng(seed)
    return (rng.random(n) > 0.5).astype(float)


def _zeros(n: int):
    if np is None:
        return None
    return np.zeros(n)


CASES: list[Case] = [
    # ---------------------------------------------------------- native wins
    Case(
        "monte_carlo_pi", "monte_carlo_pi",
        '''
def monte_carlo_pi(n: int) -> float:
    inside = 0
    x = 12345
    for i in range(n):
        x = (1103515245 * x + 12345) % 2147483648
        a = x / 2147483648.0
        x = (1103515245 * x + 12345) % 2147483648
        b = x / 2147483648.0
        if a * a + b * b <= 1.0:
            inside += 1
    return 4.0 * inside / n
''',
        lambda: (200_000,), "native", "scalar loop, no arguments to copy",
        tags=("scalar", "loop"),
    ),
    Case(
        "math_module_calls", "black_scholes_ish",
        '''
import math

def black_scholes_ish(n: int, s: float) -> float:
    total = 0.0
    for i in range(n):
        x = s + i * 0.0001
        total += math.exp(-0.5 * x) * math.sqrt(x) + math.log(x + 1.0)
    return total
''',
        lambda: (200_000, 1.0), "native",
        "math.* is the ordinary way to write a numeric kernel and the "
        "transpiler emits it; the filter rejected every one of them",
        tags=("scalar", "loop", "module-calls"),
    ),
    Case(
        "math_function_we_do_not_emit", "gamma_sum",
        '''
import math

def gamma_sum(n: int) -> float:
    total = 0.0
    for i in range(n):
        total += math.gamma(i * 0.001 + 1.0)
    return total
''',
        lambda: (20_000,), "cpython",
        "math.gamma has no emitted equivalent — allowing the module root must "
        "not mean allowing everything behind the dot",
        tags=("scalar", "loop", "module-calls"),
    ),
    Case(
        "rolling_sharpe_array", "rolling_sharpe",
        '''
def rolling_sharpe(returns, window: int) -> float:
    best = -1000000000.0
    for i in range(window, len(returns)):
        total = 0.0
        sq = 0.0
        for j in range(i - window, i):
            total += returns[j]
            sq += returns[j] * returns[j]
        mean = total / window
        var = sq / window - mean * mean
        if var > 0.000000000001:
            s = mean / (var ** 0.5)
            if s > best:
                best = s
    return best
''',
        lambda: (_f64(60_000), 64), "native",
        "nested loop with branches over a zero-copy buffer",
        tags=("buffer", "nested", "branchy"),
    ),
    Case(
        "sma_crossover", "sma_crossover",
        '''
def sma_crossover(prices, fast: int, slow: int) -> float:
    equity = 1.0
    position = 0.0
    for i in range(slow, len(prices)):
        f = 0.0
        for j in range(i - fast, i):
            f += prices[j]
        s = 0.0
        for j in range(i - slow, i):
            s += prices[j]
        signal = 1.0 if f / fast > s / slow else 0.0
        equity = equity * (1.0 + position * (prices[i] / prices[i - 1] - 1.0))
        position = signal
    return equity
''',
        lambda: (_f64(20_000, 3) + 1.0, 10, 30), "native",
        "the canonical agent-written backtest",
        tags=("buffer", "nested", "branchy"),
    ),
    Case(
        "dot_array", "dot",
        '''
def dot(a, b) -> float:
    total = 0.0
    for i in range(len(a)):
        total += a[i] * b[i]
    return total
''',
        lambda: (_f64(1_000_000, 1), _f64(1_000_000, 2)), "native",
        "reduction over two zero-copy buffers, vectorizable",
        scales=True, tags=("buffer", "reduction", "vector"),
    ),
    Case(
        "collatz_max", "collatz_max",
        '''
def collatz_max(n: int) -> int:
    best = 0
    for start in range(1, n):
        x = start
        steps = 0
        while x != 1:
            if x % 2 == 0:
                x = x // 2
            else:
                x = 3 * x + 1
            steps += 1
        if steps > best:
            best = steps
    return best
''',
        lambda: (20_000,), "native", "integer loop, data-dependent trip count",
        tags=("scalar", "loop", "branchy"),
    ),

    Case(
        "logistic_gradient", "logloss_grad",
        '''
import math


def logloss_grad(X, y, w, grad) -> float:
    loss = 0.0
    for i in range(X.shape[0]):
        z = 0.0
        for j in range(X.shape[1]):
            z += X[i, j] * w[j]
        p = 1.0 / (1.0 + math.exp(-z))
        if y[i] > 0.5:
            loss -= math.log(p + 1e-12)
        else:
            loss -= math.log(1.0 - p + 1e-12)
        d = p - y[i]
        for j in range(X.shape[1]):
            grad[j] += d * X[i, j]
    return loss / float(X.shape[0])
''',
        lambda: (_matrix(20_000, 32), _labels(20_000), _f64(32, 4), _zeros(32)),
        "native",
        "the shape almost all ML work has: a pass over a feature matrix with a "
        "per-row branch. Could not be expressed at all before 2-D buffers",
        tags=("matrix", "nested", "branchy", "ml"),
    ),

    # ------------------------------------------- native loses: copy dominates
    Case(
        "dot_list_big", "dot",
        '''
def dot(a, b) -> float:
    total = 0.0
    for i in range(len(a)):
        total += a[i] * b[i]
    return total
''',
        lambda: (_list_f64(1_000_000, 1), _list_f64(1_000_000, 2)), "either",
        "two million-element lists copied across the ABI for two ops per "
        "element. Measured 0.7-1.0x — an actual loss — until the needless copy "
        "*back* off read-only buffers was removed; now 2.9x. Kept in the corpus "
        "as the case that used to justify a cost model we then did not need",
        scales=True, tags=("list", "reduction", "copy-bound"),
    ),
    Case(
        "scale_list", "scale",
        '''
def scale(xs, k: float) -> float:
    total = 0.0
    for i in range(len(xs)):
        total += xs[i] * k
    return total
''',
        lambda: (_list_f64(400_000, 5), 1.5), "either",
        "one op per element over one copied list: measured 2x, where the same "
        "kernel on an array is 400x. Compiling it is not wrong, it is just "
        "almost all copy — the caller is the one holding the fix",
        scales=True, tags=("list", "reduction", "copy-bound"),
    ),

    # ------------------------------------------- transpiler cannot take these
    Case(
        "uses_global_constant", "score",
        '''
WINDOW = 64

def score(xs) -> float:
    total = 0.0
    for i in range(WINDOW):
        total += xs[i]
    return total
''',
        lambda: (_f64(1000),), "native",
        "WINDOW is a module global, which used to mean `Unsupported: unbound "
        "name` — a module constant is now folded into the emitted Mojo as its "
        "literal, so the unit's content hash changes when the constant does.",
        tags=("free-variable", "module-constant"),
    ),
    Case(
        "attribute_access", "total",
        '''
def total(rows) -> float:
    out = 0.0
    for i in range(len(rows)):
        out += rows[i].value
    return out
''',
        lambda: ([type("R", (), {"value": 1.0})() for _ in range(10)],), "cpython",
        "attribute load in the loop body: not in the numeric subset",
        tags=("attribute",),
    ),
    Case(
        "string_work", "count_words",
        '''
def count_words(text) -> int:
    n = 0
    for i in range(len(text)):
        if text[i] == " ":
            n += 1
    return n
''',
        lambda: ("the quick brown fox " * 200,), "cpython",
        "strings are not in the subset, and str has no array interface",
        tags=("string",),
    ),
    Case(
        "dict_accumulate", "histogram",
        '''
def histogram(xs, buckets: int) -> int:
    counts = {}
    for i in range(len(xs)):
        b = int(xs[i] * buckets)
        counts[b] = counts.get(b, 0) + 1
    return len(counts)
''',
        lambda: (_f64(1000),), "cpython", "dict literal and method call",
        tags=("dict", "method"),
    ),
    Case(
        "list_append", "evens",
        '''
def evens(n: int) -> int:
    out = []
    for i in range(n):
        if i % 2 == 0:
            out.append(i)
    return len(out)
''',
        lambda: (1000,), "cpython", "builds a list, calls a method",
        tags=("list-build", "method"),
    ),
    Case(
        "numpy_vectorized", "moments",
        '''
def moments(xs) -> float:
    m = xs.mean()
    s = xs.std()
    return float(m / s) if s > 0 else 0.0
''',
        lambda: (_f64(100_000),), "cpython",
        "already vectorized in numpy — mojosub competes with the interpreter, "
        "not with BLAS. Also has no loop.",
        tags=("numpy", "no-loop"),
    ),
    Case(
        "try_except", "safe_div_sum",
        '''
def safe_div_sum(xs, d: float) -> float:
    total = 0.0
    for i in range(len(xs)):
        try:
            total += xs[i] / d
        except ZeroDivisionError:
            total += 0.0
    return total
''',
        lambda: (_f64(1000), 2.0), "cpython", "exception handling",
        tags=("try",),
    ),
    Case(
        "comprehension", "sum_squares",
        '''
def sum_squares(n: int) -> int:
    return sum(i * i for i in range(n))
''',
        lambda: (10_000,), "cpython", "generator expression, no explicit loop",
        tags=("comprehension", "no-loop"),
    ),
    Case(
        "f_string_log", "trace_sum",
        '''
def trace_sum(xs) -> float:
    total = 0.0
    for i in range(len(xs)):
        total += xs[i]
        if i == 0:
            print(f"first {xs[i]}")
    return total
''',
        lambda: (_f64(100),), "cpython", "f-string and print in the body",
        tags=("fstring", "print"),
    ),
    Case(
        "calls_unlisted_helper", "process",
        '''
def helper(x):
    return sorted([x, 1.0])[0]

def process(xs) -> float:
    total = 0.0
    for i in range(len(xs)):
        total += helper(xs[i])
    return total
''',
        lambda: (_f64(1000),), "cpython",
        "calls a helper that is itself outside the subset",
        tags=("helper",),
    ),
    Case(
        "tuple_unpack", "minmax_span",
        '''
def minmax_span(xs) -> float:
    lo, hi = xs[0], xs[0]
    for i in range(len(xs)):
        if xs[i] < lo:
            lo = xs[i]
        if xs[i] > hi:
            hi = xs[i]
    return hi - lo
''',
        lambda: (_f64(10_000),), "cpython", "tuple assignment target",
        tags=("tuple",),
    ),
    Case(
        "enumerate_loop", "weighted",
        '''
def weighted(xs) -> float:
    total = 0.0
    for i, x in enumerate(xs):
        total += x * i
    return total
''',
        lambda: (_f64(10_000),), "cpython", "for over enumerate, not range",
        tags=("enumerate",),
    ),
    Case(
        "while_true_break", "converge",
        '''
def converge(x0: float, tol: float) -> float:
    x = x0
    while True:
        nx = (x + 2.0 / x) / 2.0
        if abs(nx - x) < tol:
            break
        x = nx
    return x
''',
        lambda: (2.0, 1e-12), "either",
        "in the subset, but far too short to matter either way",
        tags=("scalar", "tiny"),
    ),
    Case(
        "tiny_loop", "small_sum",
        '''
def small_sum(n: int) -> int:
    total = 0
    for i in range(n):
        total += i
    return total
''',
        lambda: (50,), "either", "50 iterations: below the dispatch overhead",
        tags=("scalar", "tiny"),
    ),
    Case(
        "no_args", "constant_work",
        '''
def constant_work() -> int:
    total = 0
    for i in range(1000):
        total += i * i
    return total
''',
        lambda: (), "either", "no arguments, so no signature to specialise",
        tags=("no-args",),
    ),
]


def by_label(label: str) -> list[Case]:
    return [c for c in CASES if c.label == label]
