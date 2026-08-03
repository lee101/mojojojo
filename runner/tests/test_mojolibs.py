"""The ported Mojo libraries, and the rules about substituting them.

The risk here is not performance, it is silence: a port covers a subset of its
original's API, so aliasing without being asked turns a speedup into an
AttributeError in someone else's code. These tests are mostly about that line
staying where it is.
"""

from __future__ import annotations

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import mojolibs  # noqa: E402


def test_the_registry_is_internally_consistent():
    seen_repos, seen_packages = set(), set()
    for port in mojolibs.PORTS:
        assert port.repo.startswith("mojo-"), port.repo
        assert port.repo not in seen_repos, f"{port.repo} listed twice"
        assert port.package not in seen_packages, f"{port.package} listed twice"
        assert port.imports, port.repo
        seen_repos.add(port.repo)
        seen_packages.add(port.package)


def test_every_registered_port_exists_on_disk():
    """A registry entry for a repo that is not there would put a nonexistent
    directory on a run's PYTHONPATH, which is silent until it is not."""
    missing = [p.repo for p in mojolibs.PORTS if not os.path.isdir(p.path)]
    assert missing == [], f"registry names repos that do not exist: {missing}"


def test_import_names_map_to_one_port_each():
    for name, port in mojolibs.BY_IMPORT.items():
        assert name in port.imports


def test_nothing_is_offered_for_an_unported_import(tmp_path, monkeypatch):
    monkeypatch.setattr(mojolibs, "WISHLIST", str(tmp_path / "wish.jsonl"))
    result = mojolibs.prepare(["some_library_nobody_ported"])
    assert result["used"] == [] and result["paths"] == []
    assert result["missing"] == ["some_library_nobody_ported"]


def test_unported_imports_are_recorded_for_the_factory(tmp_path, monkeypatch):
    path = tmp_path / "wish.jsonl"
    monkeypatch.setattr(mojolibs, "WISHLIST", str(path))
    monkeypatch.setattr(mojolibs, "DATA_ROOT", str(tmp_path))
    mojolibs.prepare(["totally_unported"])
    assert "totally_unported" in path.read_text()


def test_aliases_are_off_unless_asked_for():
    result = mojolibs.prepare(["PIL"], enable_aliases=False)
    assert result["aliases"] == {}, "a port was substituted without being asked"


def test_only_drop_in_ports_are_ever_aliased():
    for port in mojolibs.PORTS:
        if port.drop_in:
            continue
        result = mojolibs.prepare(list(port.imports), enable_aliases=True)
        assert result["aliases"] == {}, \
            f"{port.repo} is not drop-in but was offered as an alias"


def test_an_unbuilt_port_is_not_put_on_the_path(monkeypatch):
    """A port whose kernels are not compiled would try to build them at import
    time — inside a sandbox that has no compiler."""
    monkeypatch.setattr(mojolibs, "is_built", lambda port: False)
    monkeypatch.setattr(mojolibs, "_build_async", lambda port: None)
    result = mojolibs.prepare(["PIL"], enable_aliases=True)
    assert result["paths"] == [] and result["aliases"] == {}
    assert "mojo-pillow" in result["building"]


def test_a_built_port_is_offered_with_its_python_directory(monkeypatch):
    monkeypatch.setattr(mojolibs, "is_built", lambda port: True)
    result = mojolibs.prepare(["PIL"], enable_aliases=True)
    assert result["used"] == ["mojopillow"]
    assert result["paths"] == [mojolibs.BY_IMPORT["PIL"].python_dir]
    assert result["aliases"] == {"PIL": "mojopillow"}


def test_the_whole_thing_can_be_switched_off(monkeypatch):
    monkeypatch.setattr(mojolibs, "ENABLED", False)
    result = mojolibs.prepare(["PIL"], enable_aliases=True)
    assert result == {"paths": [], "aliases": {}, "used": [], "building": [],
                      "missing": []}


def test_paths_are_deduplicated_when_two_imports_share_a_port(monkeypatch):
    monkeypatch.setattr(mojolibs, "is_built", lambda port: True)
    result = mojolibs.prepare(["PIL", "pillow"], enable_aliases=False)
    assert len(result["paths"]) == 1


@pytest.mark.skipif(not os.path.isdir("/nvme0n1-disk/code/mojo-pillow"),
                    reason="ports are not checked out next to this repo")
def test_a_real_port_imports_and_computes():
    """End to end against the actual kernels, because a registry that lists a
    package which cannot be imported is worse than no registry."""
    port = mojolibs.BY_IMPORT["PIL"]
    if not mojolibs.is_built(port):
        pytest.skip("mojo-pillow has not been built on this machine yet")
    sys.path.insert(0, port.python_dir)
    import numpy as np
    from mojopillow import Image

    img = Image.fromarray(np.zeros((16, 16, 3), dtype=np.uint8))
    assert img.size == (16, 16)
    assert img.resize((8, 8)).size == (8, 8)


def test_no_port_can_shadow_a_real_library():
    """A port's package lives under `mojo`, so adding it to a run's PYTHONPATH
    cannot silently replace a library the caller thought they were importing.

    Two ports on disk fail this — `mojo-surprise` and `mojo-rank-bm25` call their
    packages `surprise` and `rank_bm25` — which is why they are not registered.
    The check exists because the registry is fed by a port factory, and the next
    one like them will arrive without anybody reading the diff.
    """
    assert mojolibs.REJECTED == (), mojolibs.REJECTED
    for port in mojolibs.PORTS:
        assert port.package.startswith("mojo"), port
        # A port may legitimately stand for itself — `mojo-embed` is not a port
        # *of* anything, it is its own library — and that is safe for the same
        # reason: the name is namespaced, so there is nothing to shadow.
        for name in port.imports:
            assert name == port.package or not name.startswith("mojo"), port


def test_the_registry_covers_the_ml_stack():
    """The point of registering these is that an import triggers the build.

    An unregistered port is dead weight: `prepare` wishlists the import as
    unported even though the port is sitting on disk, so nothing ever builds it
    and no run ever gets it.
    """
    for name in ("shap", "optuna", "polars", "duckdb", "umap", "hdbscan",
                 "river", "tslearn", "statsforecast", "prophet", "xgboost",
                 "lightgbm", "sklearn", "numpy", "pandas", "cvxpy", "h5py"):
        assert name in mojolibs.BY_IMPORT, f"{name} has no port registered"


def test_an_unbuilt_registered_port_is_wishlisted_as_building_not_missing(monkeypatch):
    """The distinction matters: `missing` feeds the port factory, so a port that
    exists and merely needs compiling must not look like one nobody has written."""
    monkeypatch.setattr(mojolibs, "is_built", lambda port: False)
    monkeypatch.setattr(mojolibs, "_build_async", lambda port: None)
    out = mojolibs.prepare(["shap", "definitely_not_a_library"])
    assert "mojo-shap" in out["building"]
    assert out["missing"] == ["definitely_not_a_library"]
    assert out["paths"] == []
