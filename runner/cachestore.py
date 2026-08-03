"""Where compiled kernels live, and how they survive.

Three tiers, and each exists for a different reason:

1. **NVMe** (`/nvme0n1-disk/data/mojojojo/mojosub-cache`) — the working set for
   both kernel `.so` files and whole-program `.bin` files. It is what a run's
   private view links into, so a lookup here is a `stat`.
2. **Archive** (`/sdb-disk/mojojojo/cache`) — the same objects on the big slow
   disk. Eviction moves entries here rather than deleting them, so a kernel
   that falls out of the working set costs a copy back rather than a five
   second recompile.
3. **R2 / the CDN** (`appstatic` bucket, `mojojojo/` prefix, public at
   appstatic.app.nz) — off-box, and the only tier that survives this machine.
   A freshly rented worker seeds itself from here over HTTPS instead of
   starting cold with an empty cache, which is what makes overflow capacity
   useful within a minute rather than an hour.

The reason for the reserve is blunt: this box's NVMe is 81% full and the
archive disk is at 97%. A cache that grows until a disk is full does not
degrade, it takes the service down — so eviction is driven by *free space
remaining*, not just entry count, and it runs before every write rather than
on a timer.

Uploads from users never touch any of this. They go to `mojojojo/uploads/`,
enforced in one place (`upload_key`), because a prefix rule that is applied at
three call sites is a prefix rule with a hole in it.
"""

from __future__ import annotations

import contextlib
import itertools
import json
import os
import shutil
import subprocess
import threading
import time
import urllib.error
import urllib.request

DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
LOCAL = os.environ.get("MOJOJOJO_CACHE", os.path.join(DATA_ROOT, "mojosub-cache"))
ARCHIVE = os.environ.get("MOJOJOJO_CACHE_ARCHIVE", "/sdb-disk/mojojojo/cache")

# Keep this much of the cache's filesystem free, always. Eviction to the
# archive starts as soon as free space would fall below it.
RESERVE_GB = float(os.environ.get("MOJOJOJO_DISK_RESERVE_GB", "20"))
ARCHIVE_RESERVE_GB = float(os.environ.get("MOJOJOJO_ARCHIVE_RESERVE_GB", "50"))
MAX_LOCAL_ENTRIES = int(os.environ.get("MOJOJOJO_CACHE_MAX_ENTRIES", "4000"))
MAX_ARTIFACT_BYTES = int(os.environ.get(
    "MOJOJOJO_MAX_ARTIFACT_BYTES", str(256 * 1024 * 1024)))

# Off-box object storage. Provider-shaped names stay internal for compatibility
# with the existing deployment secrets; callers only see the branded CDN_BASE.
R2_BUCKET = os.environ.get("MOJOJOJO_OBJECT_BUCKET",
                           os.environ.get("MOJOJOJO_R2_BUCKET", "mojojojostatic"))
R2_PREFIX = os.environ.get("MOJOJOJO_OBJECT_PREFIX",
                           os.environ.get("MOJOJOJO_R2_PREFIX", ""))
# The account id is what is actually kept in the host env; the endpoint is
# derived from it when it is not set explicitly, so one missing variable does
# not silently turn off the off-box cache tier.
R2_ACCOUNT = os.environ.get("R2_ACCOUNT_ID", "")
R2_ENDPOINT = os.environ.get("MOJOJOJO_OBJECT_ENDPOINT",
                             os.environ.get("R2_ENDPOINT", "")) or (
    f"https://{R2_ACCOUNT}.r2.cloudflarestorage.com" if R2_ACCOUNT else "")
R2_KEY = os.environ.get("MOJOJOJO_OBJECT_ACCESS_KEY",
                        os.environ.get("CLOUDFLARE_R2_ACCESS_KEY_ID", ""))
R2_SECRET = os.environ.get("MOJOJOJO_OBJECT_SECRET_KEY",
                           os.environ.get("CLOUDFLARE_R2_SECRET_ACCESS_KEY", ""))
# Public read path for the same objects.
CDN_BASE = os.environ.get("MOJOJOJO_STATIC_BASE",
                          os.environ.get("MOJOJOJO_CDN_BASE",
                                         "https://mojojojostatic.mojojojo.com")).rstrip("/")
SYNC_ENABLED = os.environ.get("MOJOJOJO_CACHE_SYNC", "1") != "0"
AWS = os.environ.get("MOJOJOJO_AWS", shutil.which("aws") or "/usr/local/bin/aws")

_lock = threading.Lock()
_stats = {"evicted": 0, "restored": 0, "uploaded": 0, "downloaded": 0,
          "upload_failures": 0, "seeded": 0}
_uploading: set[str] = set()
_restore_locks: dict[str, list] = {}
_tmp_counter = itertools.count()


def _is_artifact(name: str) -> bool:
    """Compiled kernel libraries and whole-program Mojo executables."""
    return name.endswith((".so", ".bin"))


def _safe_artifact(name: str) -> bool:
    """An artifact name, never a path into or out of a cache tier."""
    return (isinstance(name, str) and name == os.path.basename(name)
            and "\x00" not in name and _is_artifact(name))


@contextlib.contextmanager
def _restore_lock(name: str):
    """Coalesce concurrent misses without retaining attacker-sized key maps."""
    with _lock:
        entry = _restore_locks.setdefault(name, [threading.Lock(), 0])
        entry[1] += 1
    lock = entry[0]
    lock.acquire()
    try:
        yield
    finally:
        lock.release()
        with _lock:
            entry[1] -= 1
            if entry[1] == 0 and _restore_locks.get(name) is entry:
                del _restore_locks[name]


def _tmp_path(root: str, name: str) -> str:
    return os.path.join(
        root, f".{name}.{os.getpid()}.{threading.get_ident()}.{next(_tmp_counter)}.tmp")


def stats() -> dict:
    with _lock:
        out = dict(_stats)
    out.update({
        "local_entries": _count(LOCAL),
        "archive_entries": _count(ARCHIVE),
        "free_gb": round(free_gb(LOCAL), 1),
        "reserve_gb": RESERVE_GB,
        "remote": bool(_r2_configured()),
        "cdn": CDN_BASE,
    })
    return out


def free_gb(path: str) -> float:
    try:
        usage = shutil.disk_usage(path if os.path.isdir(path) else os.path.dirname(path))
    except OSError:
        return 0.0
    return usage.free / (1 << 30)


def _count(path: str) -> int:
    try:
        return sum(1 for n in os.listdir(path) if _is_artifact(n))
    except OSError:
        return 0


# --- eviction, with a floor under free space ------------------------------

def enforce_reserve() -> dict:
    """Evict least-recently-used entries until the disk has room again.

    Called before writing to the cache, not on a timer: the moment that matters
    is the one just before we make the problem worse.
    """
    moved = 0
    freed = 0
    entries = _entries(LOCAL)
    over_count = max(0, len(entries) - MAX_LOCAL_ENTRIES)
    entries.sort()  # oldest access first

    for _, name in entries:
        need_space = free_gb(LOCAL) < RESERVE_GB
        if not need_space and moved >= over_count:
            break
        size = _sizeof(LOCAL, name)
        if _archive(name):
            moved += 1
            freed += size
    if moved:
        with _lock:
            _stats["evicted"] += moved
    return {"evicted": moved, "freed_mb": round(freed / (1 << 20), 1),
            "free_gb": round(free_gb(LOCAL), 1)}


def _entries(root: str) -> list[tuple[float, str]]:
    out = []
    try:
        names = os.listdir(root)
    except OSError:
        return out
    for name in names:
        if not _is_artifact(name):
            continue
        try:
            out.append((os.path.getatime(os.path.join(root, name)), name))
        except OSError:
            continue
    return out


def _sizeof(root: str, name: str) -> int:
    total = 0
    for candidate in (name, name + ".mojo"):
        try:
            total += os.path.getsize(os.path.join(root, candidate))
        except OSError:
            pass
    return total


def _archive(name: str) -> bool:
    """Move one entry to the slow disk. Deletes it if the archive is full too."""
    if free_gb(ARCHIVE) < ARCHIVE_RESERVE_GB:
        # The archive disk is the one at 97%. When it has no room, eviction
        # means deletion — the object is still in R2 if syncing is on.
        return _delete(name)
    try:
        os.makedirs(ARCHIVE, exist_ok=True)
        for candidate in (name, name + ".mojo"):
            src = os.path.join(LOCAL, candidate)
            if os.path.exists(src):
                shutil.move(src, os.path.join(ARCHIVE, candidate))
        return True
    except OSError:
        return _delete(name)


def _delete(name: str) -> bool:
    ok = False
    for candidate in (name, name + ".mojo"):
        try:
            os.remove(os.path.join(LOCAL, candidate))
            ok = True
        except OSError:
            pass
    return ok


# --- restoring a miss ------------------------------------------------------

def restore(name: str) -> bool:
    """Bring one entry back into the working set, from wherever it still is.

    Archive first (a file copy), then the CDN (an HTTPS GET, no credentials
    needed because the bucket is public for reads). Either beats recompiling.
    """
    if not _safe_artifact(name):
        return False
    with _restore_lock(name):
        if os.path.exists(os.path.join(LOCAL, name)):
            return True
        if _restore_from_archive(name):
            with _lock:
                _stats["restored"] += 1
            return True
        if _download(name):
            with _lock:
                _stats["downloaded"] += 1
            return True
        return False


def _restore_from_archive(name: str) -> bool:
    src = os.path.join(ARCHIVE, name)
    if not os.path.exists(src):
        return False
    try:
        if os.path.getsize(src) > MAX_ARTIFACT_BYTES:
            return False
    except OSError:
        return False
    try:
        enforce_reserve()
        os.makedirs(LOCAL, exist_ok=True)
        for candidate in (name, name + ".mojo"):
            path = os.path.join(ARCHIVE, candidate)
            if os.path.exists(path):
                tmp = _tmp_path(LOCAL, candidate)
                try:
                    shutil.copy2(path, tmp)
                    os.replace(tmp, os.path.join(LOCAL, candidate))
                finally:
                    try:
                        os.unlink(tmp)
                    except OSError:
                        pass
        return os.path.exists(os.path.join(LOCAL, name))
    except OSError:
        return False


def _download(name: str) -> bool:
    if not SYNC_ENABLED:
        return False
    url = f"{CDN_BASE}/cache/{name}"
    try:
        request = urllib.request.Request(url, headers={"User-Agent": "mojojojo-cache"})
        with urllib.request.urlopen(request, timeout=20) as resp:
            stated = resp.headers.get("Content-Length")
            if stated is not None and int(stated) > MAX_ARTIFACT_BYTES:
                return False
            body = resp.read(MAX_ARTIFACT_BYTES + 1)
    except (urllib.error.URLError, OSError):
        return False
    except (TypeError, ValueError, OverflowError):
        return False
    if not body or len(body) > MAX_ARTIFACT_BYTES:
        return False
    try:
        enforce_reserve()
        os.makedirs(LOCAL, exist_ok=True)
        tmp = _tmp_path(LOCAL, name)
        try:
            with open(tmp, "xb") as fh:
                fh.write(body)
            os.replace(tmp, os.path.join(LOCAL, name))
        finally:
            try:
                os.unlink(tmp)
            except OSError:
                pass
        return True
    except OSError:
        return False


# --- pushing new work off the box ------------------------------------------

def publish(name: str) -> None:
    """Upload a newly compiled entry to R2, in the background.

    Fire and forget by design: the compile already succeeded and the local copy
    is what this run needs. The upload only matters to the *next* machine.
    """
    if not (_safe_artifact(name) and SYNC_ENABLED and _r2_configured()):
        return
    with _lock:
        if name in _uploading:
            return
        _uploading.add(name)
    threading.Thread(target=_upload, args=(name,), daemon=True,
                     name=f"publish:{name[:12]}").start()


def _upload(name: str) -> None:
    try:
        for candidate in (name, name + ".mojo"):
            path = os.path.join(LOCAL, candidate)
            if not os.path.exists(path):
                continue
            key = f"{R2_PREFIX}cache/{candidate}"
            result = subprocess.run(
                [AWS, "s3", "cp", path, f"s3://{R2_BUCKET}/{key}",
                 "--endpoint-url", R2_ENDPOINT, "--only-show-errors",
                 "--cache-control", "public, max-age=31536000, immutable"],
                capture_output=True, text=True, timeout=120,
                env=_aws_env())
            if result.returncode != 0:
                with _lock:
                    _stats["upload_failures"] += 1
                return
        with _lock:
            _stats["uploaded"] += 1
    except (OSError, subprocess.SubprocessError):
        with _lock:
            _stats["upload_failures"] += 1
    finally:
        with _lock:
            _uploading.discard(name)


def seed_from_remote(limit: int = 200) -> dict:
    """Pull the shared cache down onto a machine that has none.

    This is what a rented worker runs at startup: without it, overflow capacity
    spends its first minutes recompiling kernels this network has had for
    weeks. Bounded, and failure is survivable — an empty cache is correct, just
    slower.
    """
    if not (SYNC_ENABLED and _r2_configured()):
        return {"seeded": 0, "reason": "no remote configured"}
    try:
        limit = max(0, min(int(limit), MAX_LOCAL_ENTRIES))
    except (TypeError, ValueError, OverflowError):
        return {"seeded": 0, "error": "invalid seed limit"}
    if limit == 0:
        return {"seeded": 0}
    os.makedirs(LOCAL, exist_ok=True)
    prefix = f"{R2_PREFIX}cache/"
    try:
        result = subprocess.run(
            [AWS, "s3api", "list-objects-v2", "--bucket", R2_BUCKET,
             "--prefix", prefix, "--max-items", str(limit),
             "--endpoint-url", R2_ENDPOINT, "--output", "json"],
            capture_output=True, text=True, timeout=120, env=_aws_env())
    except (OSError, subprocess.SubprocessError) as exc:
        return {"seeded": 0, "error": str(exc)[:200]}
    if result.returncode != 0:
        return {"seeded": 0, "error": (result.stderr or "")[:200]}
    try:
        listing = json.loads(result.stdout or "{}")
    except (TypeError, ValueError):
        return {"seeded": 0, "error": "invalid remote listing"}
    keys = listing.get("Contents", []) if isinstance(listing, dict) else []
    names = []
    for item in keys:
        key = item.get("Key", "") if isinstance(item, dict) else ""
        name = key[len(prefix):] if key.startswith(prefix) else ""
        if _safe_artifact(name):
            names.append(name)
    # `restore` applies the same name and response-size checks as an ordinary
    # cache miss. In particular, startup no longer lets `aws s3 sync` copy an
    # unbounded or unexpected object directly into the executable cache.
    count = sum(1 for name in names[:limit] if restore(name))
    with _lock:
        _stats["seeded"] += count
    enforce_reserve()
    return {"seeded": count}


def _r2_configured() -> bool:
    return bool(R2_ENDPOINT and R2_KEY and R2_SECRET and os.path.exists(AWS))


def _aws_env() -> dict:
    return dict(os.environ,
                AWS_ACCESS_KEY_ID=R2_KEY,
                AWS_SECRET_ACCESS_KEY=R2_SECRET,
                AWS_DEFAULT_REGION="auto",
                AWS_EC2_METADATA_DISABLED="true")


# --- the upload prefix rule ------------------------------------------------

class UploadError(ValueError):
    """A key that would have escaped the uploads prefix."""


def upload_key(user_id: str, filename: str) -> str:
    """The one place a user-supplied name becomes an object key.

    Users write to `mojojojo/uploads/<user>/<name>` and nowhere else. The cache
    prefix, the site, and every other tenant of this bucket are one directory
    traversal away, so the name is sanitised rather than trusted: no separators,
    no dot segments, no leading dots, length-capped, and the result is asserted
    to still start with the uploads prefix before it is returned.
    """
    user = "".join(c for c in str(user_id) if c.isalnum() or c in "-_")[:64]
    if not user:
        raise UploadError("no user")
    base = os.path.basename(str(filename).replace("\\", "/")).strip()
    safe = "".join(c for c in base if c.isalnum() or c in "-_.")[:120]
    while safe.startswith("."):
        safe = safe[1:]
    if not safe or safe in (".", ".."):
        raise UploadError("invalid filename")

    key = f"{R2_PREFIX}uploads/{user}/{safe}"
    prefix = f"{R2_PREFIX}uploads/"
    if not key.startswith(prefix) or ".." in key:
        raise UploadError("refusing to write outside the uploads prefix")
    return key


def public_url(key: str) -> str:
    """CDN URL for an object key we produced."""
    return f"{CDN_BASE}/{key[len(R2_PREFIX):]}" if key.startswith(R2_PREFIX) \
        else f"{CDN_BASE}/{key}"
