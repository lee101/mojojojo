"""Keep warm interpreters around, one pool per environment.

A pool slot is a `forkserver.py` running inside the jail with the network
namespace and the unprivileged uid already applied. Handing it a job is a
socket write and a `fork()`; starting a run from cold is `sudo` + `unshare` +
`setpriv` + interpreter startup + numpy. Measured on this box, that is ~10ms
against ~90ms.

Everything here is best effort by design: if a slot cannot be started, has
died, is busy, or answers something unexpected, the caller falls back to the
cold path and the run still happens. A performance optimisation that can fail
closed is not one.

Slots are recycled after a while — a long-lived process accumulates whatever
the last program leaked into the page cache, and a fresh one costs 300ms of
background work nobody is waiting for.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import threading
import time
import uuid

RUNNER_DIR = os.path.dirname(os.path.abspath(__file__))
DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
SOCKET_DIR = os.path.join(DATA_ROOT, "pool")
JAIL = os.environ.get("MOJOJOJO_JAIL", "/usr/local/bin/mojojail")

ENABLED = os.environ.get("MOJOJOJO_POOL", "1") != "0"
# Slots per environment. Most traffic uses the base environment, so this is
# mostly "how many concurrent runs skip cold start".
PER_ENV = int(os.environ.get("MOJOJOJO_POOL_SLOTS", "4"))
MAX_JOBS = int(os.environ.get("MOJOJOJO_POOL_MAX_JOBS", "200"))
MAX_AGE_S = float(os.environ.get("MOJOJOJO_POOL_MAX_AGE_S", "1800"))
START_TIMEOUT_S = float(os.environ.get("MOJOJOJO_POOL_START_TIMEOUT_S", "30"))
# The jail's own kill deadline for a slot. Slots are recycled long before it.
SLOT_TTL_S = int(os.environ.get("MOJOJOJO_POOL_TTL_S", "7200"))

# How often the keeper checks that every pool is full. Well under MAX_AGE_S, so
# a slot that ages out is replaced before anyone asks for it.
KEEPER_INTERVAL_S = float(os.environ.get("MOJOJOJO_POOL_KEEPER_S", "20"))

_lock = threading.Lock()
_pools: dict[str, list["Slot"]] = {}
# Slots on their way up, so a burst of misses does not start one fleet each.
_starting: dict[str, int] = {}
_keeper: threading.Thread | None = None
_stats = {"hits": 0, "misses": 0, "starts": 0, "start_failures": 0, "recycled": 0}


class Slot:
    """One warm interpreter. Owned by whoever holds it; never shared."""

    def __init__(self, python: str, proc: subprocess.Popen, path: str) -> None:
        self.python = python
        self.proc = proc
        self.path = path
        self.jobs = 0
        self.born = time.time()

    def alive(self) -> bool:
        return self.proc.poll() is None and os.path.exists(self.path)

    def stale(self) -> bool:
        return self.jobs >= MAX_JOBS or (time.time() - self.born) > MAX_AGE_S

    def close(self) -> None:
        try:
            self.proc.terminate()
            self.proc.wait(timeout=5)
        except Exception:  # noqa: BLE001
            try:
                self.proc.kill()
            except Exception:  # noqa: BLE001
                pass
        # Killing our `sudo` child is not killing the slot: PAM opens a session
        # for root, which moves the jailed process into its own cgroup and out
        # of ours, so the interpreter inside carries on running as `mojorun`
        # until its two-hour deadline. Every restart leaked four of them, and
        # since RLIMIT_NPROC is per-uid and shared by every tenant, the pile-up
        # eventually stopped runs from starting threads at all — which surfaced
        # as acceleration quietly switching off.
        reap_slot(self.path)
        try:
            os.unlink(self.path)
        except OSError:
            pass


def stats() -> dict:
    with _lock:
        out = dict(_stats)
        out["warm"] = {k: len(v) for k, v in _pools.items() if v}
        out["starting"] = sum(_starting.values())
    return out


def available() -> bool:
    return ENABLED and os.path.exists(JAIL)


def acquire(python: str) -> Slot | None:
    """A warm slot for this interpreter, or None to take the cold path."""
    if not available():
        return None
    dead: list[Slot] = []
    try:
        with _lock:
            slots = _pools.setdefault(python, [])
            while slots:
                slot = slots.pop()
                if slot.alive() and not slot.stale():
                    _stats["hits"] += 1
                    return slot
                _stats["recycled"] += 1
                dead.append(slot)
            _stats["misses"] += 1
        return None
    finally:
        # Both of these off the caller's clock. Closing a slot waits on a
        # process, and a miss that does not replace what it consumed drains the
        # pool to empty and leaves it there — which is what happened: slots
        # aged out at 30 minutes, nothing refilled them, and every run since
        # paid cold start.
        for slot in dead:
            threading.Thread(target=slot.close, daemon=True).start()
        _top_up(python)


def release(slot: Slot, healthy: bool = True) -> None:
    """Give a slot back, or retire it."""
    if not healthy or not slot.alive() or slot.stale():
        threading.Thread(target=slot.close, daemon=True).start()
        _top_up(slot.python)
        return
    with _lock:
        slots = _pools.setdefault(slot.python, [])
        if len(slots) >= PER_ENV:
            slot.close()
            return
        slots.append(slot)


def warm(python: str, count: int | None = None) -> None:
    """Fill this environment's pool in the background, and keep it filled."""
    if not available():
        return
    _clear_stale_sockets()
    _top_up(python, count)
    _start_keeper()


def _top_up(python: str, count: int | None = None) -> None:
    """Spawn whatever this environment is short of, without blocking.

    Counts slots already on their way as well as slots in hand: a burst of
    misses would otherwise start PER_ENV replacements per miss.
    """
    count = PER_ENV if count is None else count
    with _lock:
        have = len(_pools.get(python, [])) + _starting.get(python, 0)
        short = max(0, count - have)
        _starting[python] = _starting.get(python, 0) + short
    for _ in range(short):
        _spawn_async(python)


def _start_keeper() -> None:
    """Refill in the background, so an idle pool is full before the next run.

    `acquire` topping up covers the busy case; this covers the case that
    actually bit — slots ageing out while nothing was running, and the first
    request after a quiet hour paying for all of it.
    """
    global _keeper
    with _lock:
        if _keeper is not None and _keeper.is_alive():
            return
        _keeper = threading.Thread(target=_keep, daemon=True, name="pool-keeper")
        _keeper.start()


def _keep() -> None:
    while True:
        time.sleep(KEEPER_INTERVAL_S)
        try:
            with _lock:
                pythons = list(_pools)
                retired = []
                for python, slots in _pools.items():
                    live = []
                    for slot in slots:
                        (live if slot.alive() and not slot.stale()
                         else retired).append(slot)
                    _pools[python] = live
                    _stats["recycled"] += len(slots) - len(live)
            for slot in retired:
                slot.close()
            for python in pythons:
                _top_up(python)
        except Exception:  # noqa: BLE001 - a warm pool is never worth a crash
            pass


def reap_slot(path: str) -> None:
    """Kill the jailed interpreter behind a slot socket, by socket name.

    The slot is PID 1 of its own namespace, so this takes its children with it.
    We cannot signal it directly — it runs as the sandbox user — which is what
    `mojojail --reap-slot` is for.
    """
    name = os.path.basename(path).split(".")[0]
    if not name or not os.path.exists(JAIL):
        return
    try:
        subprocess.run(["sudo", "-n", JAIL, "--reap-slot", name],
                       capture_output=True, timeout=10, check=False)
    except Exception:  # noqa: BLE001 - reaping is best effort
        pass


def _clear_stale_sockets() -> None:
    """Kill and remove slots left behind by a previous worker process.

    Unlinking the socket file was not enough: the process behind it survives
    our exit (see Slot.close) and holds threads against a per-uid limit every
    tenant shares. Nothing here can be live — this runs before any slot of ours
    exists, and a slot from an older process is unreachable anyway.
    """
    try:
        names = os.listdir(SOCKET_DIR)
    except OSError:
        return
    for name in names:
        if not name.endswith(".sock"):
            continue
        path = os.path.join(SOCKET_DIR, name)
        reap_slot(path)
        try:
            os.unlink(path)
        except OSError:
            pass


def _spawn_async(python: str) -> None:
    threading.Thread(target=lambda: _spawn(python), daemon=True).start()


def _spawn(python: str) -> Slot | None:
    try:
        return _start(python)
    finally:
        with _lock:
            _starting[python] = max(0, _starting.get(python, 0) - 1)


def _start(python: str) -> Slot | None:
    # 0733, not 0777: the sandbox user must be able to create its socket here
    # and traverse to it, but must not be able to *list* the directory — the
    # only thing protecting one run's socket from another is that its name is
    # a random 32 hex digits.
    os.makedirs(SOCKET_DIR, mode=0o733, exist_ok=True)
    os.chmod(SOCKET_DIR, 0o733)
    path = os.path.join(SOCKET_DIR, uuid.uuid4().hex + ".sock")
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": SOCKET_DIR,
        "TMPDIR": SOCKET_DIR,
        "PYTHONPATH": os.pathsep.join(
            [RUNNER_DIR, os.environ.get("MOJOJOJO_MOJOSUB",
                                        "/nvme0n1-disk/code/mojosub")]),
        "PYTHONUNBUFFERED": "1",
        "OPENBLAS_NUM_THREADS": "1",
        "OMP_NUM_THREADS": "1",
        "MKL_NUM_THREADS": "1",
    }
    # `--ports all`: a slot establishes its mounts long before it knows which
    # run it will serve, so it has to be able to serve any of them. It pays
    # that cost once, in the background, off everybody's latency path.
    cmd = ["sudo", "-n", JAIL, str(SLOT_TTL_S), "--ports", "all",
           "/usr/bin/env", "-i",
           *[f"{k}={v}" for k, v in env.items()],
           python, os.path.join(RUNNER_DIR, "forkserver.py"), path]
    try:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True)
    except OSError:
        with _lock:
            _stats["start_failures"] += 1
        return None

    deadline = time.time() + START_TIMEOUT_S
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        if os.path.exists(path):
            slot = Slot(python, proc, path)
            with _lock:
                _stats["starts"] += 1
                slots = _pools.setdefault(python, [])
                if len(slots) < PER_ENV:
                    slots.append(slot)
                    return slot
            slot.close()
            return None
        time.sleep(0.02)

    with _lock:
        _stats["start_failures"] += 1
    try:
        proc.kill()
    except Exception:  # noqa: BLE001
        pass
    # Said out loud: a pool that quietly fails to start slots looks exactly
    # like a pool nobody is using, and the difference is every run's latency.
    try:
        _, err = proc.communicate(timeout=2)
        if err:
            print("mojojojo-pool: slot failed to start: %s" % err.strip()[-500:],
                  flush=True)
    except Exception:  # noqa: BLE001
        pass
    return None


def submit(slot: Slot, job: dict, timeout_s: float) -> dict | None:
    """Run one job on a warm slot.

    Returns None only while it is still safe to fall back — that is, before the
    slot has confirmed a forked child. Once a pid comes back the program is
    running, and retrying it on the cold path would execute the caller's code
    twice; from that point every outcome, including a broken connection, is
    reported as a result rather than as "did not happen".
    """
    started_pid = 0
    try:
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(10)
        conn.connect(slot.path)
        conn.sendall(json.dumps(job).encode() + b"\n")
        reader = conn.makefile("r")
        first = reader.readline()
        if not first:
            return None
        started = json.loads(first)
        if "pid" not in started:
            return None
        started_pid = started["pid"]

        conn.settimeout(timeout_s)
        try:
            line = reader.readline()
        except socket.timeout:
            # The child outlived even the outer deadline: kill it through the
            # jail (it runs as the sandbox user, so we cannot signal it) and
            # retire the slot, which may be wedged.
            _reap(slot, started_pid)
            return {"exit_code": 137, "timed_out": True, "unhealthy": True}
        if not line:
            # The slot died mid-run. The program may well have completed and
            # written its output; the trace file decides, not a re-run.
            return {"exit_code": 1, "unhealthy": True, "lost": True}
        done = json.loads(line)
        slot.jobs += 1
        return done
    except (OSError, ValueError) as exc:
        if started_pid:
            return {"exit_code": 1, "unhealthy": True, "lost": True,
                    "error": str(exc)}
        return None
    finally:
        try:
            conn.close()
        except Exception:  # noqa: BLE001
            pass


def _reap(slot: "Slot", pid: int) -> None:
    """Kill a run that outlived even the outer deadline.

    The pid the slot reports is its *namespace* pid — the slot is PID 1 of its
    own PID namespace — so it means nothing on the host. Killing the slot by
    its socket name takes the child with it, which is why a timeout here also
    retires the slot.
    """
    name = os.path.basename(slot.path).split(".")[0]
    subprocess.run(["sudo", "-n", JAIL, "--reap-slot", name],
                   capture_output=True, timeout=10, check=False)


def shutdown() -> None:
    with _lock:
        for slots in _pools.values():
            for slot in slots:
                slot.close()
        _pools.clear()
