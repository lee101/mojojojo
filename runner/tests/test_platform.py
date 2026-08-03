"""Tests for the parts that make this a platform rather than a subprocess:
auto-install, the host-side compile cache, and the warm pool.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
import types

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import autoinstall  # noqa: E402
import hostcompile  # noqa: E402
import pool  # noqa: E402


# --- auto-install ---

def test_finds_imports_at_any_depth():
    code = ("import pandas as pd\n"
            "def f():\n"
            "    import requests\n"
            "    from bs4 import BeautifulSoup\n"
            "    return requests, BeautifulSoup\n")
    assert set(autoinstall.top_level_imports(code)) == {"pandas", "requests", "bs4"}


def test_relative_imports_are_not_packages():
    assert autoinstall.top_level_imports("from . import sibling\nfrom .mod import x\n") == []


def test_stdlib_is_never_installed():
    wanted = autoinstall.detect("import json, os, sys, asyncio, sqlite3\n")
    assert wanted == []


def test_module_names_that_differ_from_the_distribution():
    wanted = autoinstall.detect("import cv2\nimport yaml\nfrom PIL import Image\n")
    assert set(wanted) == {"opencv-python-headless", "pyyaml", "pillow"}


def test_our_own_modules_are_never_installed():
    assert autoinstall.detect("import mojosub\nimport accel\n") == []


def test_detection_is_capped():
    code = "\n".join(f"import pkg{i}" for i in range(50))
    assert len(autoinstall.detect(code)) <= autoinstall.MAX_AUTO


def test_broken_syntax_asks_for_nothing():
    assert autoinstall.detect("def (:\n") == []


def test_already_present_packages_do_not_rebuild_the_environment(tmp_path):
    site = tmp_path / "lib" / "python3.13" / "site-packages"
    site.mkdir(parents=True)
    (site / "numpy").mkdir()
    fake_python = str(tmp_path / "bin" / "python")
    os.makedirs(os.path.dirname(fake_python), exist_ok=True)
    open(fake_python, "w").close()
    autoinstall._site_cache.clear()
    assert autoinstall.detect("import numpy\n", fake_python) == []


# --- the compile cache boundary ---

PROGRAM = ("def kernel(n: int) -> float:\n"
           "    t = 0.0\n"
           "    for i in range(n):\n"
           "        t += i\n"
           "    return t\n"
           "print(kernel(10))\n")
OTHER = PROGRAM.replace("kernel", "other_kernel").replace("t += i", "t += i * 2")


def seeded_shared(tmp_path, monkeypatch, program=PROGRAM):
    """A shared cache holding one compiled unit for `program`."""
    shared = tmp_path / "shared"
    (shared / "sigs").mkdir(parents=True)
    (shared / "index").mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    key = hostcompile.source_keys(program)[0]
    (shared / "sigs" / f"{key}.json").write_text(json.dumps({"Int": {"ret": "Float64"}}))
    (shared / "a1b2c3d4e5f60718293a4b5c6d7e8f90.so").write_bytes(b"\x7fELF-pretend")
    (shared / "index" / f"{key}.json").write_text(json.dumps({"units": ["a1b2c3d4e5f60718293a4b5c6d7e8f90"]}))
    return shared, key


def test_a_run_is_seeded_with_its_own_kernels(tmp_path, monkeypatch):
    shared, key = seeded_shared(tmp_path, monkeypatch)
    private = tmp_path / "run" / "cache"

    linked = hostcompile.seed(str(private), PROGRAM)

    assert linked == 1
    assert (private / "a1b2c3d4e5f60718293a4b5c6d7e8f90.so").exists()
    assert (private / "sigs" / f"{key}.json").exists()


def test_per_run_cache_seeding_obeys_the_entry_ceiling(tmp_path, monkeypatch):
    shared, key = seeded_shared(tmp_path, monkeypatch)
    second = "b1b2c3d4e5f60718293a4b5c6d7e8f90"
    (shared / f"{second}.so").write_bytes(b"\x7fELF-pretend")
    (shared / "index" / f"{key}.json").write_text(json.dumps({
        "units": ["a1b2c3d4e5f60718293a4b5c6d7e8f90", second],
    }))
    monkeypatch.setattr(hostcompile, "MAX_ENTRIES", 1)
    private = tmp_path / "run" / "cache"

    assert hostcompile.seed(str(private), PROGRAM) == 1
    assert len(list(private.glob("*.so"))) == 1


def test_a_run_is_not_seeded_with_anybody_elses(tmp_path, monkeypatch):
    """The whole cache used to be linked into every run, which handed each
    tenant the compiled form of every other tenant's kernels."""
    shared, _ = seeded_shared(tmp_path, monkeypatch)
    private = tmp_path / "run" / "cache"

    linked = hostcompile.seed(str(private), OTHER)

    assert linked == 0
    assert not (private / "a1b2c3d4e5f60718293a4b5c6d7e8f90.so").exists()
    assert list((private / "sigs").iterdir()) == []


def test_an_unknown_program_gets_an_empty_cache(tmp_path, monkeypatch):
    seeded_shared(tmp_path, monkeypatch)
    private = tmp_path / "run" / "cache"
    assert hostcompile.seed(str(private), "print('hello')") == 0


def test_a_run_writing_its_view_leaves_the_shared_copy_alone(tmp_path, monkeypatch):
    shared, key = seeded_shared(tmp_path, monkeypatch)
    private = tmp_path / "run" / "cache"
    hostcompile.seed(str(private), PROGRAM)

    # mojosub writes through a temp file and renames, which replaces the run's
    # own directory entry rather than the shared inode.
    tmp = private / "sigs" / f"{key}.json.tmp"
    tmp.write_text(json.dumps({"Int": {"ret": "Int", "verified": True}}))
    os.replace(tmp, private / "sigs" / f"{key}.json")

    kept = json.loads((shared / "sigs" / f"{key}.json").read_text())
    assert kept["Int"]["ret"] == "Float64"
    assert "verified" not in kept


def test_promotion_keeps_return_types_and_drops_verdicts(tmp_path, monkeypatch):
    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))

    private = tmp_path / "run" / "cache"
    (private / "sigs").mkdir(parents=True)
    (private / "sigs" / "unit.json").write_text(json.dumps({
        "Int": {"ret": "Float64", "verified": True, "raced": True,
                "speedup": 99.0, "retired": "nonsense"},
    }))

    promoted = hostcompile.promote(str(private), str(tmp_path / "requests"))
    assert promoted["sigs"] == 1
    kept = json.loads((shared / "sigs" / "unit.json").read_text())
    assert kept == {"Int": {"ret": "Float64"}}, \
        "a sandbox must not be able to tell another run a variant is verified"


def test_promotion_ignores_a_seed_the_run_never_touched(tmp_path, monkeypatch):
    seeded_shared(tmp_path, monkeypatch)
    private = tmp_path / "run" / "cache"
    hostcompile.seed(str(private), PROGRAM)
    result = hostcompile.promote(str(private), str(tmp_path / "requests"))
    assert result["sigs"] == 0


def test_a_junk_return_type_is_not_promoted(tmp_path, monkeypatch):
    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    private = tmp_path / "run" / "cache"
    (private / "sigs").mkdir(parents=True)
    (private / "sigs" / "unit.json").write_text(json.dumps({
        "Int": {"ret": "Float64; import os"},
    }))
    assert hostcompile.promote(str(private), str(tmp_path / "req"))["sigs"] == 0


def test_a_run_cannot_promote_a_signature_for_another_program(tmp_path,
                                                               monkeypatch):
    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    victim = hostcompile.source_keys(OTHER)[0]
    private = tmp_path / "run" / "cache"
    (private / "sigs").mkdir(parents=True)
    (private / "sigs" / f"{victim}.json").write_text(json.dumps({
        "Int": {"ret": "Float64"},
    }))

    result = hostcompile.promote(
        str(private), str(tmp_path / "requests"), PROGRAM)

    assert result["sigs"] == 0
    assert not (shared / "sigs" / f"{victim}.json").exists()


def test_concurrent_unit_index_updates_do_not_lose_an_entry(tmp_path,
                                                             monkeypatch):
    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    original = hostcompile._units_for

    def slow_read(key):
        current = original(key)
        time.sleep(0.03)
        return current

    monkeypatch.setattr(hostcompile, "_units_for", slow_read)
    ready = threading.Event()

    def remember(unit):
        ready.wait()
        hostcompile._remember_units(["abcd1234"], [unit])

    threads = [threading.Thread(target=remember, args=(unit,))
               for unit in ("a" * 32, "b" * 32)]
    for thread in threads:
        thread.start()
    ready.set()
    for thread in threads:
        thread.join(5)

    assert not any(thread.is_alive() for thread in threads)
    assert sorted(original("abcd1234")) == ["a" * 32, "b" * 32]


def test_pruning_keeps_the_cache_bounded(tmp_path, monkeypatch):
    """Eviction is by least-recently-used, and evicted entries move to the
    archive rather than being deleted — a copy back beats a recompile."""
    import cachestore

    shared = tmp_path / "shared"
    archive = tmp_path / "archive"
    shared.mkdir()
    archive.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    monkeypatch.setattr(cachestore, "LOCAL", str(shared))
    monkeypatch.setattr(cachestore, "ARCHIVE", str(archive))
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 3)
    monkeypatch.setattr(cachestore, "ARCHIVE_RESERVE_GB", 0)
    for i in range(6):
        (shared / f"{i}.so").write_bytes(b"x")
        (shared / f"{i}.so.mojo").write_text("source")
        os.utime(shared / f"{i}.so", (1000 + i, 1000 + i))

    hostcompile._prune()

    left = sorted(n for n in os.listdir(shared) if n.endswith(".so"))
    assert len(left) == 3
    assert left == ["3.so", "4.so", "5.so"], "evicted the wrong (newest) entries"
    assert sorted(n for n in os.listdir(archive) if n.endswith(".so")) == \
        ["0.so", "1.so", "2.so"], "evicted entries should be archived, not deleted"


# --- the warm pool ---

def test_pool_is_skipped_when_the_jail_is_missing(monkeypatch):
    monkeypatch.setattr(pool, "JAIL", "/nonexistent/mojojail")
    assert pool.available() is False
    assert pool.acquire("/usr/bin/python3") is None


def test_pool_can_be_switched_off(monkeypatch):
    monkeypatch.setattr(pool, "ENABLED", False)
    assert pool.available() is False


class _FakeProc:
    def __init__(self, alive=True):
        self._alive = alive

    def poll(self):
        return None if self._alive else 1

    def terminate(self):
        self._alive = False

    def wait(self, timeout=None):
        return 0

    def kill(self):
        self._alive = False


def test_a_dead_slot_is_never_handed_out(tmp_path, monkeypatch):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "_starting", {})
    # A miss now starts replacements; this test is about what it hands out.
    monkeypatch.setattr(pool, "_spawn_async", lambda python: None)
    sock = tmp_path / "s.sock"
    sock.write_text("")
    dead = pool.Slot("py", _FakeProc(alive=False), str(sock))
    pool._pools["py"] = [dead]
    monkeypatch.setattr(pool, "JAIL", __file__)  # exists, so available() is True
    assert pool.acquire("py") is None


def test_a_stale_slot_is_retired_on_release(tmp_path, monkeypatch):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "MAX_JOBS", 1)
    monkeypatch.setattr(pool, "_spawn_async", lambda python: None)
    sock = tmp_path / "s.sock"
    sock.write_text("")
    slot = pool.Slot("py", _FakeProc(), str(sock))
    slot.jobs = 5
    pool.release(slot)
    assert not pool._pools.get("py")


@pytest.mark.parametrize("healthy", [True, False])
def test_release_returns_healthy_slots_only(tmp_path, monkeypatch, healthy):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "_spawn_async", lambda python: None)
    sock = tmp_path / "s.sock"
    sock.write_text("")
    slot = pool.Slot("py", _FakeProc(), str(sock))
    pool.release(slot, healthy=healthy)
    assert bool(pool._pools.get("py")) is healthy


def test_a_rebuilt_unit_reaches_the_program_whose_signature_we_already_knew(
        tmp_path, monkeypatch):
    """The steady state: a known program needs a *new* library.

    A signature is keyed on the Python source and survives a toolchain change.
    A library is keyed on the Mojo source, the compiler version and the host's
    instruction set, and does not. So a Mojo upgrade — or a change to what goes
    into the library key — leaves a program whose signature is already known
    needing a fresh compile.

    Indexing the compiled unit against "signatures this run taught us" records
    nothing in that case, because the run taught us nothing new. The unit is
    then compiled, correct, and unreachable: `seed` hands the next run nothing,
    it queues the same compile again, and acceleration is silently off for that
    program for ever. Nothing errors and nothing is slow enough to notice.
    """
    shared = tmp_path / "shared"
    (shared / "sigs").mkdir(parents=True)
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    key = hostcompile.source_keys(PROGRAM)[0]
    # The signature is already known, exactly as it would be after any earlier
    # run of this program. There is nothing new for promotion to learn.
    (shared / "sigs" / f"{key}.json").write_text(
        json.dumps({"Int": {"ret": "Float64"}}))

    private = tmp_path / "run" / "cache"
    requests = tmp_path / "run" / "requests"
    requests.mkdir(parents=True)
    hostcompile.seed(str(private), PROGRAM)
    (requests / "unit.mojo").write_text("def f(): pass\n")

    built = []
    # Only the compiler is faked; source_key has to stay the real one, since the
    # whole point is that promote and seed agree about the program's keys.
    real_cache = hostcompile._mojosub().cache

    class FakeCache:
        source_key = staticmethod(real_cache.source_key)

        @staticmethod
        def compile_unit(source, opt=3, root=None):
            path = os.path.join(root, "beef" * 8 + ".so")
            with open(path, "wb") as fh:
                fh.write(b"\x7fELF-pretend")
            built.append(path)
            return path

    monkeypatch.setattr(hostcompile, "_mojosub",
                        lambda: types.SimpleNamespace(cache=FakeCache))
    monkeypatch.setattr(hostcompile.cachestore, "publish", lambda name: None)
    monkeypatch.setattr(hostcompile.cachestore, "enforce_reserve", lambda: None)

    result = hostcompile.promote(str(private), str(requests), PROGRAM)

    assert result["compiled"] == 1, result
    assert result["sigs"] == 0, "nothing new was learned, which is the point"
    # The unit still has to be findable from the program that asked for it.
    assert "beef" * 8 in hostcompile._units_for(key), (
        "a rebuilt unit was not indexed against its program, so the next run "
        "cannot be seeded with it")
    fresh = tmp_path / "run2" / "cache"
    assert hostcompile.seed(str(fresh), PROGRAM) == 1
    assert (fresh / ("beef" * 8 + ".so")).exists()


def test_units_are_still_indexed_when_no_program_is_given(tmp_path, monkeypatch):
    """The old behaviour is the fallback, not a regression: without the program
    text, newly promoted signatures are still the best available key."""
    shared = tmp_path / "shared"
    (shared / "sigs").mkdir(parents=True)
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    key = hostcompile.source_keys(PROGRAM)[0]

    private = tmp_path / "run" / "cache"
    (private / "sigs").mkdir(parents=True)
    (private / "sigs" / f"{key}.json").write_text(
        json.dumps({"Int": {"ret": "Float64"}}))
    requests = tmp_path / "run" / "requests"
    requests.mkdir(parents=True)
    (requests / "unit.mojo").write_text("def f(): pass\n")

    real_cache = hostcompile._mojosub().cache

    class FakeCache:
        source_key = staticmethod(real_cache.source_key)

        @staticmethod
        def compile_unit(source, opt=3, root=None):
            path = os.path.join(root, "cafe" * 8 + ".so")
            with open(path, "wb") as fh:
                fh.write(b"\x7fELF-pretend")
            return path

    monkeypatch.setattr(hostcompile, "_mojosub",
                        lambda: types.SimpleNamespace(cache=FakeCache))
    monkeypatch.setattr(hostcompile.cachestore, "publish", lambda name: None)
    monkeypatch.setattr(hostcompile.cachestore, "enforce_reserve", lambda: None)

    result = hostcompile.promote(str(private), str(requests))
    assert result["sigs"] == 1
    assert "cafe" * 8 in hostcompile._units_for(key)


# --- the warm pool refills itself ---
#
# It did not, and that was the single largest source of latency in the service:
# slots aged out after thirty minutes, `acquire` retired them without starting
# replacements, and every run from then on paid a cold start it did not have to.

def test_an_empty_pool_starts_refilling_itself(monkeypatch):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "_starting", {})
    monkeypatch.setattr(pool, "JAIL", __file__)
    started = []
    monkeypatch.setattr(pool, "_spawn_async", started.append)

    assert pool.acquire("py") is None
    assert started == ["py"] * pool.PER_ENV


def test_retiring_a_dead_slot_replaces_it(tmp_path, monkeypatch):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "_starting", {})
    monkeypatch.setattr(pool, "JAIL", __file__)
    started = []
    monkeypatch.setattr(pool, "_spawn_async", started.append)
    sock = tmp_path / "s.sock"
    sock.write_text("")
    pool._pools["py"] = [pool.Slot("py", _FakeProc(alive=False), str(sock))]

    assert pool.acquire("py") is None
    assert started, "a slot that could not be handed out must be replaced"


def test_slots_already_on_their_way_are_not_started_twice(monkeypatch):
    monkeypatch.setattr(pool, "_pools", {})
    monkeypatch.setattr(pool, "_starting", {"py": pool.PER_ENV})
    monkeypatch.setattr(pool, "JAIL", __file__)
    started = []
    monkeypatch.setattr(pool, "_spawn_async", started.append)

    pool._top_up("py")
    assert started == []


def test_sockets_from_a_previous_process_are_cleared(tmp_path, monkeypatch):
    """A killed slot does not unlink its socket, so the directory grew by one
    per slot per restart until it held hundreds of dead names."""
    monkeypatch.setattr(pool, "SOCKET_DIR", str(tmp_path))
    (tmp_path / "old.sock").write_text("")
    (tmp_path / "keep.txt").write_text("")

    pool._clear_stale_sockets()
    assert not (tmp_path / "old.sock").exists()
    assert (tmp_path / "keep.txt").exists()


# --- compiling while the run is still running ---

def test_the_watcher_answers_a_request_with_a_library(tmp_path, monkeypatch):
    """`mojo_stub` waits for this file, and loading it is what lets a kernel go
    native part-way through its own first run instead of only on the next one."""
    requests = tmp_path / "requests"
    requests.mkdir()
    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))

    class FakeCache:
        @staticmethod
        def compile_unit(source, opt=3, root=None):
            path = os.path.join(root, "feed" * 8 + ".so")
            with open(path, "wb") as fh:
                fh.write(b"\x7fELF-pretend")
            return path

    monkeypatch.setattr(hostcompile, "_mojosub",
                        lambda: types.SimpleNamespace(cache=FakeCache))
    monkeypatch.setattr(hostcompile.cachestore, "publish", lambda name: None)
    monkeypatch.setattr(hostcompile.cachestore, "enforce_reserve", lambda: None)

    with hostcompile.Watcher(str(requests), interval=0.005) as watcher:
        (requests / "unit.mojo").write_text("fn main(): pass\n")
        _wait_for(lambda: (requests / "unit.so").exists())
    watcher.join(timeout=5)
    assert (requests / "unit.so").read_bytes() == b"\x7fELF-pretend"


def test_a_unit_that_will_not_build_says_so_instead_of_stalling(tmp_path, monkeypatch):
    """Without the marker the run's compile thread waits out its whole budget
    for a library that is never coming."""
    requests = tmp_path / "requests"
    requests.mkdir()

    def explode(*_a, **_k):
        raise RuntimeError("nope")

    monkeypatch.setattr(hostcompile, "_mojosub",
                        lambda: types.SimpleNamespace(
                            cache=types.SimpleNamespace(compile_unit=explode)))
    monkeypatch.setattr(hostcompile.cachestore, "enforce_reserve", lambda: None)

    with hostcompile.Watcher(str(requests), interval=0.005) as watcher:
        (requests / "unit.mojo").write_text("not mojo\n")
        _wait_for(lambda: (requests / "unit.err").exists())
    watcher.join(timeout=5)
    assert not (requests / "unit.so").exists()


def test_concurrent_requests_compile_one_unit_and_release_the_key(tmp_path, monkeypatch):
    """Unique programs must not grow the lock table, and duplicate programs
    must not spend two compiler slots on the same output."""
    import threading
    import time

    shared = tmp_path / "shared"
    shared.mkdir()
    monkeypatch.setattr(hostcompile, "SHARED", str(shared))
    entered = threading.Event()
    finish = threading.Event()
    calls = []

    class FakeCache:
        @staticmethod
        def key_for(_source, _opt):
            return "abcd" * 8

        @staticmethod
        def compile_unit(source, opt=3, root=None):
            calls.append(source)
            entered.set()
            assert finish.wait(5)
            path = os.path.join(root, "abcd" * 8 + ".so")
            with open(path, "wb") as fh:
                fh.write(b"library")
            return path

    monkeypatch.setattr(hostcompile, "_mojosub",
                        lambda: types.SimpleNamespace(cache=FakeCache))
    monkeypatch.setattr(hostcompile.cachestore, "enforce_reserve", lambda: None)
    results = []
    first = threading.Thread(target=lambda: results.append(
        hostcompile._compile_unit("same source")))
    second = threading.Thread(target=lambda: results.append(
        hostcompile._compile_unit("same source")))
    first.start()
    assert entered.wait(5)
    second.start()
    time.sleep(0.02)
    finish.set()
    first.join(5)
    second.join(5)

    assert calls == ["same source"]
    assert sorted(built for _, built, _ in results) == [False, True]
    assert hostcompile._unit_locks == {}


def test_a_full_compile_queue_fails_without_leaking_a_waiter(monkeypatch):
    class FullSemaphore:
        @staticmethod
        def acquire(**_kwargs):
            return False

    monkeypatch.setattr(hostcompile, "_compile_slots", FullSemaphore())
    before = hostcompile._compile_rejected
    with pytest.raises(hostcompile.CompileBusy):
        with hostcompile.compiler_slot():
            raise AssertionError("a full queue cannot grant a slot")
    assert hostcompile._compile_waiting == 0
    assert hostcompile._compile_rejected == before + 1


def _wait_for(predicate, seconds: float = 5.0) -> None:
    import time as _time

    deadline = _time.perf_counter() + seconds
    while _time.perf_counter() < deadline:
        if predicate():
            return
        _time.sleep(0.005)
    raise AssertionError("timed out waiting for the watcher")


# --- the compiler stub waits for the host ---

def _run_stub(requests, out_path, wait_ms, timeout=20):
    import subprocess

    env = dict(os.environ,
               MOJOJOJO_COMPILE_REQUESTS=str(requests),
               MOJOJOJO_COMPILE_WAIT_MS=str(wait_ms))
    src = os.path.join(str(requests), "in.mojo")
    with open(src, "w") as fh:
        fh.write("fn main(): pass\n")
    stub = os.path.join(os.path.dirname(__file__), "..", "mojo_stub.py")
    return subprocess.run(
        [sys.executable, stub, "build", "--emit", "shared-lib",
         "--optimization-level=3", src, "-o", str(out_path)],
        capture_output=True, text=True, env=env, timeout=timeout)


def _answer(requests, payload=b"\x7fELF-pretend"):
    """Play the host: fill whatever request appears."""
    def worker():
        deadline = __import__("time").perf_counter() + 10
        while __import__("time").perf_counter() < deadline:
            for name in os.listdir(requests):
                if name.endswith(".mojo") and name != "in.mojo":
                    with open(os.path.join(requests, name[:-5] + ".so"), "wb") as fh:
                        fh.write(payload)
                    return
            __import__("time").sleep(0.005)

    import threading

    thread = threading.Thread(target=worker, daemon=True)
    thread.start()
    return thread


def test_the_stub_loads_a_library_the_host_builds_during_the_run(tmp_path):
    """This is what makes a kernel's *first* run go native part-way through
    instead of running interpreted end to end while the compiler works."""
    requests = tmp_path / "requests"
    requests.mkdir()
    out = tmp_path / "unit.so"
    _answer(str(requests))
    result = _run_stub(requests, out, 8000)
    assert result.returncode == 0, result.stderr
    assert out.read_bytes() == b"\x7fELF-pretend"


def test_the_stub_stops_waiting_when_the_program_is_over(tmp_path):
    """Past the end of the user's code there is no call left for the library to
    serve, and the caller would be sitting through a compile for nothing."""
    requests = tmp_path / "requests"
    requests.mkdir()
    (requests / "stop").write_text("")
    out = tmp_path / "unit.so"
    started = __import__("time").perf_counter()
    result = _run_stub(requests, out, 8000)
    assert result.returncode == 1
    assert __import__("time").perf_counter() - started < 4
    assert not out.exists()


def test_the_stub_still_queues_when_it_cannot_wait(tmp_path):
    requests = tmp_path / "requests"
    requests.mkdir()
    result = _run_stub(requests, tmp_path / "unit.so", 0)
    assert result.returncode == 1
    queued = [n for n in os.listdir(requests)
              if n.endswith(".mojo") and n != "in.mojo"]
    assert len(queued) == 1, "the host must still get the request"
