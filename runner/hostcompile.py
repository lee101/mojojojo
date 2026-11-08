"""The compile cache, and the only thing allowed to write to it.

Runs read the cache; they never write it. A run gets a private directory full
of symlinks into the shared cache, so a lookup is a hit and a write lands in
the run's own directory and dies with it. Afterwards the host promotes what the
run learned:

- a queued Mojo unit is **compiled here**, by us, from the source whose hash
  names it — so a shared library can only ever be what its content address
  says it is;
- a learned return type is copied across, and nothing else. Verification
  verdicts stay behind, so every run re-checks the first native result against
  CPython itself. A tenant can therefore never mark another tenant's kernel
  "verified".

That is the whole trust boundary. It is deliberately small.
"""

from __future__ import annotations

import contextlib
import itertools
import json
import os
import shutil
import sys
import threading
import time

import cachestore

DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
SHARED = os.environ.get("MOJOJOJO_CACHE", os.path.join(DATA_ROOT, "mojosub-cache"))
MOJOSUB_PATH = os.environ.get("MOJOJOJO_MOJOSUB", "/nvme0n1-disk/code/mojosub")
# Bounds the per-run seeding cost: every entry costs one symlink at run start.
MAX_ENTRIES = max(0, int(os.environ.get("MOJOJOJO_CACHE_MAX_ENTRIES", "4000")))
_HEX = __import__("re").compile(r"^[0-9a-f]{8,64}$")
# Mojo builds are CPU- and memory-heavy. Letting every request start one at
# once makes the machine slower for all tenants and gives unique source strings
# an easy resource-exhaustion path. A small shared pool covers both JIT units
# and whole-program builds (mojoprog uses compiler_slot too).
COMPILE_CONCURRENCY = max(1, int(os.environ.get(
    "MOJOJOJO_COMPILE_CONCURRENCY",
    str(max(1, min(4, (os.cpu_count() or 4) // 8))))))
MAX_COMPILE_QUEUE = max(1, int(os.environ.get(
    "MOJOJOJO_COMPILE_QUEUE", str(COMPILE_CONCURRENCY * 8))))
COMPILE_QUEUE_TIMEOUT_S = max(0.1, float(os.environ.get(
    "MOJOJOJO_COMPILE_QUEUE_TIMEOUT_S", "30")))

_mojo_bin: str | None = None
_tmp_counter = itertools.count()
_lock = threading.Lock()
_compile_slots = threading.BoundedSemaphore(COMPILE_CONCURRENCY)
_compile_active = 0
_compile_waiting = 0
_compile_rejected = 0
_stats = {"compiled": 0, "failed": 0, "compile_seconds": 0.0, "promoted_sigs": 0}


class CompileBusy(RuntimeError):
    """The bounded compiler queue is full; callers should fall back or retry."""


def stats() -> dict:
    with _lock:
        out = dict(_stats)
        out["compile_active"] = _compile_active
        out["compile_waiting"] = _compile_waiting
        out["compile_concurrency"] = COMPILE_CONCURRENCY
        out["compile_queue_limit"] = MAX_COMPILE_QUEUE
        out["compile_queue_timeout_s"] = COMPILE_QUEUE_TIMEOUT_S
        out["compile_rejected"] = _compile_rejected
    out["entries"] = _count_entries()
    out["store"] = cachestore.stats()
    return out


@contextlib.contextmanager
def compiler_slot():
    """Bound all host compiler processes with one observable semaphore."""
    global _compile_active, _compile_waiting, _compile_rejected
    with _lock:
        if _compile_active + _compile_waiting >= \
                COMPILE_CONCURRENCY + MAX_COMPILE_QUEUE:
            _compile_rejected += 1
            raise CompileBusy("host compiler queue is full; retry shortly")
        _compile_waiting += 1
    if not _compile_slots.acquire(timeout=COMPILE_QUEUE_TIMEOUT_S):
        with _lock:
            _compile_waiting -= 1
            _compile_rejected += 1
        raise CompileBusy("host compiler queue wait expired; retry shortly")
    with _lock:
        _compile_waiting -= 1
        _compile_active += 1
    try:
        yield
    finally:
        with _lock:
            _compile_active -= 1
        _compile_slots.release()


def _count_entries() -> int:
    try:
        return sum(1 for n in os.listdir(SHARED) if n.endswith(".so"))
    except OSError:
        return 0


_COMPILE_ENV_KEEP = ("PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "LD_LIBRARY_PATH",
                     "MODULAR_HOME", "MOJO_PYTHON_LIBRARY", "CUDA_HOME")


def compile_env() -> dict:
    """Minimal environment for compiling tenant source: no credentials.

    An allowlist, not a denylist: the host environment holds R2 keys, DB URLs
    and service tokens, none of which `mojo build` needs.
    """
    env = {k: os.environ[k] for k in _COMPILE_ENV_KEEP if os.environ.get(k)}
    env.setdefault("PATH", "/usr/local/bin:/usr/bin:/bin")
    return env


def mojo_binary() -> str:
    """Absolute path to the real Mojo compiler on this host.

    Resolved here rather than left to mojosub because the runner's PATH is
    systemd's, which does not include a pixi environment.
    """
    global _mojo_bin
    if _mojo_bin is not None:
        return _mojo_bin
    candidates = [
        os.environ.get("MOJOSUB_MOJO", ""),
        os.path.join(MOJOSUB_PATH, ".pixi", "envs", "default", "bin", "mojo"),
        shutil.which("mojo") or "",
    ]
    _mojo_bin = next((c for c in candidates if c and os.path.exists(c)), "")
    return _mojo_bin


def modular_home() -> str:
    """MODULAR_HOME matching the resolved compiler: <env>/share/max.

    An inherited MODULAR_HOME from an unrelated Modular install points at a
    different toolchain, and the mismatch surfaces as "unable to locate module
    'std'" — so the derived path wins over the environment.
    """
    mojo = mojo_binary()
    if mojo:
        candidate = os.path.join(os.path.dirname(os.path.dirname(mojo)), "share", "max")
        if os.path.isdir(candidate):
            return candidate
    return os.environ.get("MODULAR_HOME", "")


def _mojosub():
    if MOJOSUB_PATH not in sys.path:
        sys.path.insert(0, MOJOSUB_PATH)
    if mojo_binary():
        os.environ["MOJOSUB_MOJO"] = mojo_binary()
        os.environ["MODULAR_HOME"] = modular_home()
    # The host compiles into the shared cache and nowhere else.
    os.environ["MOJOSUB_CACHE"] = SHARED
    import mojosub  # noqa: PLC0415 - imported lazily so the runner starts without it

    mojosub.cache.DEFAULT_ROOT = SHARED
    return mojosub


def mojo_version(default: str = "") -> str:
    try:
        return _mojosub().mojo_version()
    except Exception:  # noqa: BLE001
        return default


def restore(name: str) -> bool:
    """Bring one evicted entry back before a run looks for it."""
    return cachestore.restore(name)


def source_keys(program: str) -> list[str]:
    """The cache keys this program's own functions will look themselves up by.

    mojosub keys a learned signature by the unparsed module its function came
    from, with decorators stripped. Recomputing that here — from the program
    text the caller just sent — is what lets a run be seeded with its own
    kernels and nothing else.

    That module is now the *whole* program: mojosub keeps sibling functions, so
    that a kernel calling an annotated helper can be emitted at all. This has to
    agree exactly, and when it did not the symptom was silent — every run wrote
    a signature under one key and looked it up under another, so nothing was
    ever seeded and every run recompiled from scratch. The per-function keys are
    still returned behind it, so units cached under the old scheme stay
    reachable.
    """
    import ast

    try:
        tree = _stripped(program)
    except SyntaxError:
        return []
    cache = _mojosub().cache
    keys = [cache.source_key(ast.unparse(tree))]
    for name in [n.name for n in tree.body if isinstance(n, ast.FunctionDef)]:
        # Reuse the parsed tree. This runs while seeding every request, and the
        # previous implementation reparsed the whole submission once per
        # function (quadratic work for generated or notebook-style programs).
        module = ast.Module(
            body=[
                node for node in tree.body
                if not isinstance(node, ast.FunctionDef) or node.name == name
            ],
            type_ignores=tree.type_ignores,
        )
        key = cache.source_key(ast.unparse(module))
        if key not in keys:
            keys.append(key)
    return keys


def _stripped(program: str):
    """The program as mojosub sees it: decorators off, everything else kept."""
    import ast

    module = ast.parse(program)
    for node in module.body:
        if isinstance(node, ast.FunctionDef):
            node.decorator_list = []
    return module


def seed(private: str, program: str = "") -> int:
    """Give a run the compiled kernels of *its own* program, and nothing else.

    The obvious implementation — link the whole shared cache into every run —
    hands every tenant the compiled form of every other tenant's kernels. So
    the seed is keyed on the program's own source: its signature files are
    copied, and the units recorded against them are hardlinked in. A run that
    has never been seen before gets an empty cache, which is correct.

    Hardlinks rather than symlinks, so the shared directory does not have to be
    visible inside the jail at all.
    """
    os.makedirs(os.path.join(private, "sigs"), mode=0o777, exist_ok=True)
    os.chmod(private, 0o777)
    os.chmod(os.path.join(private, "sigs"), 0o777)
    if not program:
        return 0

    linked = 0
    for key in source_keys(program):
        sig = os.path.join(SHARED, "sigs", key + ".json")
        if os.path.exists(sig):
            _link(sig, os.path.join(private, "sigs", key + ".json"))
        for unit in _units_for(key):
            if linked >= MAX_ENTRIES:
                return linked
            for candidate in (unit + ".so",):
                source = os.path.join(SHARED, candidate)
                if not os.path.exists(source):
                    # Evicted to the archive or the CDN; bring it back.
                    if not cachestore.restore(candidate):
                        continue
                _link(source, os.path.join(private, candidate))
                linked += 1
    return linked


def _tmp_tag() -> str:
    """A name no other writer of this file can be using."""
    return f"{os.getpid()}.{threading.get_ident()}.{next(_tmp_counter)}"


def _index_path(key: str) -> str:
    return os.path.join(SHARED, "index", key + ".json")


def _units_for(key: str) -> list[str]:
    try:
        with open(_index_path(key), "r", encoding="utf-8") as fh:
            table = json.load(fh)
        units = table.get("units", []) if isinstance(table, dict) else []
        return [u for u in units[:MAX_ENTRIES]
                if isinstance(u, str) and _HEX.fullmatch(u)]
    except (OSError, ValueError, TypeError):
        return []


def _remember_units(keys, units) -> None:
    """Record which compiled units belong to which program signature.

    Without this the only way to find a run's own kernels would be to hand it
    the whole cache and let it look — which is exactly the exposure this
    avoids.
    """
    valid_units = {unit for unit in units
                   if isinstance(unit, str) and _HEX.fullmatch(unit)}
    if not valid_units:
        return
    for key in keys:
        # This is a read-modify-write, so unique temp names alone are not
        # sufficient: two finishers can both read the old index and whichever
        # replaces it last silently loses the other one's unit.
        with _lock:
            existing = set(_units_for(key))
            merged = sorted(existing | valid_units)[:MAX_ENTRIES]
            if merged == sorted(existing):
                continue
            os.makedirs(os.path.dirname(_index_path(key)), exist_ok=True)
            tmp = _index_path(key) + f".{_tmp_tag()}.tmp"
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump({"units": merged}, fh)
            os.replace(tmp, _index_path(key))


_unit_locks: dict[str, list] = {}


@contextlib.contextmanager
def _unit_lock(source: str):
    """One compiler per unit, however many runs want it.

    Two people running the same example at the same time each queue the same
    build, and `compile_unit` has no interlock: both compile it, seconds of CPU
    each, to produce the same file. The second one waits here and finds the
    first one's output already in the cache.
    """
    try:
        key = _mojosub().cache.key_for(source, 3)
    except Exception:  # noqa: BLE001 - falling back to no interlock is fine
        with threading.Lock():
            yield
        return
    with _lock:
        entry = _unit_locks.setdefault(key, [threading.Lock(), 0])
        entry[1] += 1
    lock = entry[0]
    lock.acquire()
    try:
        yield
    finally:
        lock.release()
        with _lock:
            entry[1] -= 1
            if entry[1] == 0 and _unit_locks.get(key) is entry:
                del _unit_locks[key]


def _compile_unit(source: str) -> tuple[str, bool, float]:
    """Compile one unit, deduplicated and bounded; report a real cache miss."""
    with _unit_lock(source):
        cache = _mojosub().cache
        expected = ""
        try:
            expected = os.path.join(SHARED, cache.key_for(source, 3) + ".so")
        except Exception:  # noqa: BLE001 - test doubles and older mojosub
            pass
        if expected and os.path.exists(expected):
            return expected, False, 0.0
        # Reserve space before the expensive build, rather than discovering a
        # full disk when the compiler tries to rename its output.
        cachestore.enforce_reserve()
        with compiler_slot():
            # A whole-program build may have been ahead of us in the compiler
            # queue; check again once this request owns a slot.
            if expected and os.path.exists(expected):
                return expected, False, 0.0
            started = time.perf_counter()
            lib = cache.compile_unit(source, opt=3, root=SHARED)
            return lib, True, time.perf_counter() - started


class Watcher:
    """Compiles a run's requests *while it is still running*.

    `promote` exists because a compile is seconds and belongs on our clock, not
    the caller's. That reasoning is right about the billing and wrong about the
    latency: mojosub asks for the build on a background thread after the first
    call, so a build that finishes before the program does is one the same run
    can use — and a kernel called eight times went eight times interpreted
    purely because nobody was listening.

    So the host listens here, compiles into the shared cache exactly as
    promotion would, and drops the library beside the request for `mojo_stub`
    to pick up. Nothing about the trust boundary moves: the compiler is still
    on the host, still fed the source whose hash names the unit, and the run
    still cannot write to the shared cache.
    """

    def __init__(self, requests: str, interval: float = 0.02) -> None:
        self.requests = requests
        self.interval = interval
        self.compiled = 0
        self.failed = 0
        self.seconds = 0.0
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def __enter__(self) -> "Watcher":
        self._thread = threading.Thread(target=self._loop, daemon=True,
                                        name="compile-watch")
        self._thread.start()
        return self

    def __exit__(self, *_exc) -> None:
        self._stop.set()

    def join(self, timeout: float = 900.0) -> None:
        """Wait for an in-flight compile to finish.

        Called by promotion, off the caller's clock, and not before: promoting
        while the watcher is still building the same unit means two compilers
        on the same source, which is seconds of CPU to arrive at a file that
        already existed.
        """
        if self._thread:
            self._thread.join(timeout=timeout)

    def _loop(self) -> None:
        done: set[str] = set()
        while not self._stop.is_set():
            for name in _listdir(self.requests):
                if not name.endswith(".mojo") or name in done:
                    continue
                done.add(name)
                self._fill(name)
            self._stop.wait(self.interval)

    def _fill(self, name: str) -> None:
        base = name[: -len(".mojo")]
        path = os.path.join(self.requests, name)
        try:
            with open(path, "r", encoding="utf-8") as fh:
                source = fh.read()
        except OSError:
            return
        started = time.perf_counter()
        try:
            lib, built, compile_seconds = _compile_unit(source)
            if built:
                cachestore.publish(os.path.basename(lib))
            # Copied rather than hardlinked: the run's directory is writable by
            # the sandbox user and the shared cache inode must not be.
            tmp = os.path.join(self.requests, base + ".so.part")
            shutil.copyfile(lib, tmp)
            os.chmod(tmp, 0o644)
            os.replace(tmp, os.path.join(self.requests, base + ".so"))
            if built:
                self.compiled += 1
                with _lock:
                    _stats["compiled"] += 1
                    _stats["compile_seconds"] += compile_seconds
        except CompileBusy:
            # This run remains on CPython. A later run can ask again; queue
            # pressure is not evidence that this source failed to compile.
            try:
                open(os.path.join(self.requests, base + ".err"), "w").close()
            except OSError:
                pass
        except Exception:  # noqa: BLE001 - a kernel that will not build is normal
            self.failed += 1
            with _lock:
                _stats["failed"] += 1
            # Tells the waiting stub to stop waiting; without it the run's
            # compile thread sits out its whole budget for nothing.
            try:
                open(os.path.join(self.requests, base + ".err"), "w").close()
            except OSError:
                pass
        finally:
            self.seconds += time.perf_counter() - started


def promote(private: str, requests: str, program: str = "") -> dict:
    """Compile what the run asked for and keep what it learned.

    Called off the request path — the caller has already been answered and
    billed — because a compile is seconds and it buys the *next* caller's
    speed, not this one's.

    `program` is the source the run submitted, and it is what the compiled units
    are indexed against. It has to be, rather than "whichever signatures this
    run happened to teach us": a signature is keyed on the Python source and
    survives a toolchain change, while a library is keyed on the Mojo source,
    the compiler version and the host's instruction set, and does not. So the
    steady state is a program whose signature we already know needing a *new*
    library — and indexing off newly-learned signatures records nothing for it,
    leaving the unit compiled, correct, and unreachable. `seed()` then hands the
    run nothing, it queues the same compile again, and acceleration is silently
    off for that program for ever.

    Using the same `source_keys(program)` that `seed()` reads is what keeps the
    two ends of that from drifting.
    """
    result = {"compiled": 0, "failed": 0, "deferred": 0, "sigs": 0}
    os.makedirs(SHARED, exist_ok=True)
    compiled_units: list[str] = []

    for name in _listdir(requests):
        if not name.endswith(".mojo"):
            continue
        path = os.path.join(requests, name)
        try:
            with open(path, "r", encoding="utf-8") as fh:
                source = fh.read()
        except OSError:
            continue
        started = time.perf_counter()
        try:
            lib, built, compile_seconds = _compile_unit(source)
            # Off-box copy, in the background. It is what lets a rented worker
            # start warm instead of recompiling this network's whole history.
            if built:
                cachestore.publish(os.path.basename(lib))
            compiled_units.append(os.path.basename(lib)[:-3])  # drop .so
            if built:
                result["compiled"] += 1
                with _lock:
                    _stats["compiled"] += 1
                    _stats["compile_seconds"] += compile_seconds
        except CompileBusy:
            result["deferred"] += 1
        except Exception:  # noqa: BLE001 - a kernel that will not build is normal
            result["failed"] += 1
            with _lock:
                _stats["failed"] += 1

    program_keys = source_keys(program) if program else []
    promoted_keys = _promote_signatures(
        private, allowed=set(program_keys) if program else None)
    result["sigs"] = len(promoted_keys)
    if compiled_units:
        # Every key this program looks itself up by, not just the ones it taught
        # us something new about. Falls back to the promoted keys when the caller
        # did not pass the source, which is the old behaviour and better than
        # nothing.
        keys = program_keys if program else promoted_keys
        _remember_units(keys or promoted_keys, compiled_units)
    _prune()
    return result


def _promote_signatures(private: str,
                        allowed: set[str] | None = None) -> list[str]:
    """Copy across learned return types, and only those.

    `verified`, `raced`, `speedup` and `retired` are verdicts about whether a
    compiled variant is correct and fast. They were reached inside a sandbox by
    code we do not trust, so they stop here — every run re-verifies for itself,
    which costs one interpreted call and removes the entire class of attack
    where one tenant tells another tenant's run that a wrong answer is fine.
    """
    src_dir = os.path.join(private, "sigs")
    dst_dir = os.path.join(SHARED, "sigs")
    promoted: list[str] = []
    for name in _listdir(src_dir):
        src = os.path.join(src_dir, name)
        if os.path.islink(src) or not name.endswith(".json"):
            continue  # untouched seed, nothing new to learn
        key_name = name[:-len(".json")]
        if allowed is not None and key_name not in allowed:
            # The sandbox can write its private cache directory. It may only
            # teach us about signatures derived from the source this request
            # submitted, never choose another program's cache key itself.
            continue
        if _same_file(src, os.path.join(dst_dir, name)):
            continue  # a hardlinked seed the run did not replace
        try:
            with open(src, "r", encoding="utf-8") as fh:
                table = json.load(fh)
        except (OSError, ValueError):
            continue
        if not isinstance(table, dict):
            continue
        clean = {}
        for key, entry in itertools.islice(table.items(), MAX_ENTRIES):
            ret = entry.get("ret") if isinstance(entry, dict) else entry
            if isinstance(key, str) and isinstance(ret, str) and ret.isidentifier():
                clean[key] = {"ret": ret}
        if not clean:
            continue
        os.makedirs(dst_dir, exist_ok=True)
        dst = os.path.join(dst_dir, name)
        # As with the unit index, preserve both sets of learned signatures
        # when two runs of the same source finish together.
        with _lock:
            merged = {}
            try:
                with open(dst, "r", encoding="utf-8") as fh:
                    merged = json.load(fh)
            except (OSError, ValueError):
                pass
            if not isinstance(merged, dict):
                merged = {}
            before = json.dumps(merged, sort_keys=True)
            merged.update({k: v for k, v in clean.items() if k not in merged})
            if json.dumps(merged, sort_keys=True) == before:
                continue
            tmp = dst + f".{_tmp_tag()}.tmp"
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(merged, fh)
            os.replace(tmp, dst)
            promoted.append(key_name)
    with _lock:
        _stats["promoted_sigs"] += len(promoted)
    return promoted


def _same_file(a: str, b: str) -> bool:
    try:
        return os.path.samefile(a, b)
    except OSError:
        return False


def _prune() -> None:
    """Keep the working set bounded and the disk above its reserve.

    Entries are moved to the archive disk rather than deleted (see
    cachestore), because a copy back is milliseconds and a recompile is
    seconds. Every entry also costs one symlink at the start of every run, so
    an unbounded cache is a slow one even with infinite disk.
    """
    cachestore.enforce_reserve()


def _listdir(path: str) -> list[str]:
    try:
        return os.listdir(path)
    except OSError:
        return []


def _link(target: str, link: str) -> None:
    """Hardlink if we can, copy if we cannot.

    A hardlink means the run reads the file without the shared directory being
    reachable from inside the jail, and a write by the run replaces its own
    directory entry (mojosub writes through a temp file and renames) rather
    than the shared inode.
    """
    try:
        os.link(target, link)
    except FileExistsError:
        pass
    except OSError:
        try:
            shutil.copyfile(target, link)
        except OSError:
            pass
