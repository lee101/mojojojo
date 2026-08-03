"""Auto-install: what a program imports is what gets installed.

This is the feature most likely to fail quietly — a wrong distribution name
installs *something*, the import still fails, and the caller is left reading a
traceback about a package they never asked for. So the tests are mostly about
the mapping being right and the failure modes being loud.
"""

from __future__ import annotations

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import autoinstall  # noqa: E402
import envs  # noqa: E402


# --- what counts as an import ---

@pytest.mark.parametrize("code,expected", [
    ("import pandas", ["pandas"]),
    ("import pandas as pd", ["pandas"]),
    ("import pandas, numpy", ["pandas", "numpy"]),
    ("from pandas import DataFrame", ["pandas"]),
    ("from pandas.io import json", ["pandas"]),
    ("import a.b.c", ["a"]),
    ("if True:\n    import polars", ["polars"]),
    ("try:\n    import ujson\nexcept ImportError:\n    ujson = None", ["ujson"]),
    ("async def f():\n    import aiohttp", ["aiohttp"]),
    ("class K:\n    import attr", ["attr"]),
])
def test_imports_are_found_wherever_they_are(code, expected):
    assert sorted(autoinstall.top_level_imports(code)) == sorted(expected)


@pytest.mark.parametrize("code", [
    'x = "import torch"',
    "# import torch",
    "'''\nimport torch\n'''",
    "importlib = 1",
    "from . import local",
    "from .. import parent",
])
def test_things_that_only_look_like_imports_are_ignored(code):
    assert autoinstall.top_level_imports(code) == []


def test_duplicate_imports_are_requested_once():
    code = "import pandas\nimport pandas as pd\nfrom pandas import Series\n"
    assert autoinstall.top_level_imports(code) == ["pandas"]


def test_import_order_is_preserved():
    code = "import zzz\nimport aaa\nimport mmm\n"
    assert autoinstall.top_level_imports(code) == ["zzz", "aaa", "mmm"]


# --- module name to distribution ---

@pytest.mark.parametrize("module,dist", [
    ("cv2", "opencv-python-headless"),
    ("PIL", "pillow"),
    ("sklearn", "scikit-learn"),
    ("skimage", "scikit-image"),
    ("yaml", "pyyaml"),
    ("bs4", "beautifulsoup4"),
    ("dateutil", "python-dateutil"),
    ("dotenv", "python-dotenv"),
    ("jwt", "pyjwt"),
    ("Crypto", "pycryptodome"),
    ("psycopg2", "psycopg2-binary"),
    ("docx", "python-docx"),
    ("fitz", "pymupdf"),
    ("zmq", "pyzmq"),
])
def test_known_module_names_map_to_the_right_distribution(module, dist):
    assert autoinstall.detect(f"import {module}\n") == [dist]


def test_unknown_modules_pass_through_with_underscores_normalised():
    # PyPI names use hyphens; `import my_pkg` is `my-pkg` on the index.
    assert autoinstall.detect("import my_special_pkg\n") == ["my-special-pkg"]


def test_the_mapping_table_is_self_consistent():
    for module, dist in autoinstall.KNOWN.items():
        assert dist and dist == dist.strip(), module
        assert " " not in dist, f"{module} maps to something with a space"
        # Every mapped distribution must survive the spec validator, or the
        # mapping would produce an environment that can never be built.
        assert envs.normalize([dist])


@pytest.mark.parametrize("blocked", sorted(autoinstall.BLOCKED))
def test_platform_modules_are_never_installed(blocked):
    assert autoinstall.detect(f"import {blocked}\n") == []


def test_local_mojo_ports_are_not_mistaken_for_pypi_packages():
    code = "import mojo_qrcode\nimport qrcode\n"
    assert autoinstall.detect(code) == ["qrcode"]


def test_stdlib_never_appears_even_when_shadowed_by_a_pypi_name():
    # `json`, `types`, `email` all exist on PyPI as unrelated packages.
    for module in ("json", "types", "email", "logging", "select", "queue"):
        assert autoinstall.detect(f"import {module}\n") == [], module


# --- combining with what the caller asked for ---

def test_explicit_and_detected_packages_merge_without_duplicates():
    specs = envs.normalize(["pandas"] + autoinstall.detect("import pandas\n"))
    assert specs.count("pandas") == 1


def test_a_pinned_explicit_version_is_not_duplicated_by_detection():
    # The caller pinned it; detection adds the bare name. Both reach uv, which
    # would fail on a conflict — so this documents the shape that must not
    # regress into "pandas==2.2.* and pandas".
    specs = envs.normalize(["pandas==2.2.*"] + autoinstall.detect("import pandas\n"))
    assert "pandas==2.2.*" in specs
    assert len([s for s in specs if s.startswith("pandas")]) <= 2


@pytest.mark.parametrize("spec", [
    "numpy", "numpy==2.1.0", "numpy>=2", "numpy<3", "numpy~=2.1",
    "numpy!=2.0.0", "uvicorn[standard]", "ray[default]==2.9.0",
])
def test_valid_specs_survive_normalisation(spec):
    assert spec in envs.normalize([spec])


def test_detection_is_bounded_so_one_program_cannot_build_a_universe():
    code = "\n".join(f"import pkg_{i}" for i in range(200))
    assert len(autoinstall.detect(code)) == autoinstall.MAX_AUTO


def test_detection_never_raises_on_hostile_input():
    for code in ("import " + "a" * 10000, "\x00import x", "import 123",
                 "from  import x", "import *", ""):
        assert isinstance(autoinstall.detect(code), list)


# --- end to end, against a real environment ---

@pytest.mark.slow
def test_a_detected_package_is_actually_installed_and_importable():
    """The whole point, verified once against uv rather than mocked."""
    import sandbox

    python, info = envs.ensure(autoinstall.detect("import pytz\nprint(pytz.utc)\n"))
    assert "pytz" in info["packages"]
    result = sandbox.run(python=python, code="import pytz; print(pytz.utc)",
                         timeout_ms=30000)
    assert result["ok"] and "UTC" in result["stdout"]
