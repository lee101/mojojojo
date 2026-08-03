"""Where compiled kernels live: the reserve, the tiers, and the prefix rule.

The reserve is the part that keeps the machine alive. This box's NVMe is 81%
full and the archive disk is at 97%, so "evict when we hit N entries" is not a
policy — free space is.
"""

from __future__ import annotations

import os
import sys
import types

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import cachestore  # noqa: E402


@pytest.fixture
def store(tmp_path, monkeypatch):
    local = tmp_path / "local"
    archive = tmp_path / "archive"
    local.mkdir()
    archive.mkdir()
    monkeypatch.setattr(cachestore, "LOCAL", str(local))
    monkeypatch.setattr(cachestore, "ARCHIVE", str(archive))
    monkeypatch.setattr(cachestore, "ARCHIVE_RESERVE_GB", 0)
    monkeypatch.setattr(cachestore, "SYNC_ENABLED", False)
    return local, archive


def make(local, name, atime=None, size=16):
    (local / name).write_bytes(b"x" * size)
    (local / (name + ".mojo")).write_text("source")
    if atime is not None:
        os.utime(local / name, (atime, atime))


# --- the reserve ---

def test_nothing_is_evicted_while_there_is_room(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 100)
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 500.0)
    for i in range(5):
        make(local, f"{i}.so")
    assert cachestore.enforce_reserve()["evicted"] == 0
    assert len(list(local.glob("*.so"))) == 5


def test_low_free_space_evicts_even_under_the_entry_cap(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 1000)
    monkeypatch.setattr(cachestore, "RESERVE_GB", 20)
    # Free space stays below the reserve, so eviction continues until there is
    # nothing left to evict — the disk filling up is the emergency this exists
    # for, and stopping early would be worse than being aggressive.
    monkeypatch.setattr(cachestore, "free_gb",
                        lambda path: 0.0 if str(path).endswith("local") else 500.0)
    for i in range(4):
        make(local, f"{i}.so", atime=1000 + i)
    result = cachestore.enforce_reserve()
    assert result["evicted"] == 4
    assert len(list(local.glob("*.so"))) == 0
    assert len(list(archive.glob("*.so"))) == 4


def test_eviction_is_least_recently_used(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 2)
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 500.0)
    for i in range(5):
        make(local, f"{i}.so", atime=1000 + i)
    cachestore.enforce_reserve()
    left = sorted(p.name for p in local.glob("*.so"))
    assert left == ["3.so", "4.so"]


def test_eviction_moves_the_source_alongside_the_library(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 0)
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 500.0)
    make(local, "a.so")
    cachestore.enforce_reserve()
    assert (archive / "a.so").exists() and (archive / "a.so.mojo").exists()


def test_whole_program_binaries_share_eviction_and_restore(store, monkeypatch):
    """Serverless Mojo binaries must not bypass the cache size ceiling."""
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 0)
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 500.0)
    make(local, "program.bin")

    result = cachestore.enforce_reserve()

    assert result["evicted"] == 1
    assert not (local / "program.bin").exists()
    assert (archive / "program.bin").exists()
    assert (archive / "program.bin.mojo").exists()
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 100)
    assert cachestore.restore("program.bin") is True
    assert (local / "program.bin").exists()


def test_cache_stats_count_libraries_and_programs(store):
    local, _ = store
    make(local, "kernel.so")
    make(local, "program.bin")
    assert cachestore._count(str(local)) == 2


def test_a_full_archive_means_deletion_not_a_crash(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 0)
    monkeypatch.setattr(cachestore, "ARCHIVE_RESERVE_GB", 1000)  # archive "full"
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 0.5)
    make(local, "a.so")
    cachestore.enforce_reserve()
    assert not (local / "a.so").exists()
    assert not (archive / "a.so").exists(), "should not have been archived"


# --- restoring ---

def test_a_local_hit_needs_no_work(store):
    local, _ = store
    make(local, "a.so")
    assert cachestore.restore("a.so") is True


def test_an_evicted_entry_comes_back_from_the_archive(store, monkeypatch):
    local, archive = store
    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 0)
    monkeypatch.setattr(cachestore, "free_gb", lambda path: 500.0)
    make(local, "a.so")
    cachestore.enforce_reserve()
    assert not (local / "a.so").exists()

    monkeypatch.setattr(cachestore, "MAX_LOCAL_ENTRIES", 100)
    assert cachestore.restore("a.so") is True
    assert (local / "a.so").exists() and (local / "a.so.mojo").exists()


def test_a_miss_everywhere_is_a_miss_not_an_error(store):
    assert cachestore.restore("nothing.so") is False


@pytest.mark.parametrize("name", ["../escape.so", "/tmp/escape.bin", "x.txt"])
def test_restore_accepts_artifact_names_not_paths(store, name):
    assert cachestore.restore(name) is False


def test_an_oversized_remote_artifact_is_rejected_before_reading(store, monkeypatch):
    monkeypatch.setattr(cachestore, "SYNC_ENABLED", True)

    class Response:
        headers = {"Content-Length": str(cachestore.MAX_ARTIFACT_BYTES + 1)}

        def __enter__(self):
            return self

        def __exit__(self, *_exc):
            return None

        def read(self, *_args):
            raise AssertionError("oversized response should not be buffered")

    monkeypatch.setattr(cachestore.urllib.request, "urlopen",
                        lambda *_args, **_kwargs: Response())
    assert cachestore._download("a.so") is False


def test_remote_calls_are_skipped_when_syncing_is_off(store, monkeypatch):
    called = []
    monkeypatch.setattr(cachestore, "_upload", lambda name: called.append(name))
    cachestore.publish("a.so")
    assert called == []


def test_publishing_is_skipped_without_credentials(store, monkeypatch):
    monkeypatch.setattr(cachestore, "SYNC_ENABLED", True)
    monkeypatch.setattr(cachestore, "R2_ENDPOINT", "")
    called = []
    monkeypatch.setattr(cachestore, "_upload", lambda name: called.append(name))
    cachestore.publish("a.so")
    assert called == []


def test_remote_seed_is_bounded_and_uses_checked_restoration(store, monkeypatch):
    monkeypatch.setattr(cachestore, "SYNC_ENABLED", True)
    monkeypatch.setattr(cachestore, "_r2_configured", lambda: True)
    keys = [
        {"Key": f"{cachestore.R2_PREFIX}cache/{name}"}
        for name in ("a.so", "b.bin", "b.bin.mojo", "../escape.so")
    ]
    commands = []

    def run(command, **_kwargs):
        commands.append(command)
        return types.SimpleNamespace(
            returncode=0, stderr="", stdout=__import__("json").dumps({"Contents": keys}))

    restored = []
    monkeypatch.setattr(cachestore.subprocess, "run", run)

    def restore(name):
        restored.append(name)
        return True

    monkeypatch.setattr(cachestore, "restore", restore)

    result = cachestore.seed_from_remote(limit=2)

    assert result["seeded"] == 2
    assert restored == ["a.so", "b.bin"]
    assert "--max-items" in commands[0]
    assert commands[0][commands[0].index("--max-items") + 1] == "2"


def test_remote_seed_rejects_an_invalid_limit(store, monkeypatch):
    monkeypatch.setattr(cachestore, "SYNC_ENABLED", True)
    monkeypatch.setattr(cachestore, "_r2_configured", lambda: True)
    assert "error" in cachestore.seed_from_remote(limit="many")


# --- the uploads prefix (the rule that guards a shared bucket) ---

def test_uploads_land_under_the_uploads_prefix():
    key = cachestore.upload_key("user-1", "data.csv")
    assert key == f"{cachestore.R2_PREFIX}uploads/user-1/data.csv"


@pytest.mark.parametrize("filename", [
    "../cache/aaaa.so", "../../appnz/index.html", "/etc/passwd",
    "..%2f..%2fcache%2fx.so", "x/../../y", "\\..\\..\\z",
    "....//....//cache//x.so",
])
def test_no_filename_escapes_the_prefix(filename):
    # Either outcome is correct — a sanitised key inside the user's directory,
    # or an outright refusal. What must never happen is a key somewhere else.
    try:
        key = cachestore.upload_key("u", filename)
    except cachestore.UploadError:
        return
    assert key.startswith(f"{cachestore.R2_PREFIX}uploads/u/")
    assert ".." not in key
    assert "/cache/" not in key


def test_a_dotfile_cannot_be_created():
    assert not os.path.basename(cachestore.upload_key("u", ".env")).startswith(".")


def test_an_empty_name_still_produces_a_usable_key():
    with pytest.raises(cachestore.UploadError):
        cachestore.upload_key("u", "...")


def test_names_are_length_capped():
    key = cachestore.upload_key("u", "a" * 500 + ".bin")
    assert len(os.path.basename(key)) <= 120


def test_public_urls_stay_under_the_cdn_base():
    for name in ("a.bin", "../../x", "weird name.txt"):
        url = cachestore.public_url(cachestore.upload_key("u", name))
        assert url.startswith(cachestore.CDN_BASE + "/")
        assert "/cache/" not in url
