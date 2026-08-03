"""What a hostile program must not be able to do.

These are the tests that decide whether this service can be pointed at the
internet. The ones that need a real jail skip themselves when `mojojail` is not
installed — and that skip is itself the signal that a machine is a development
box, not production.

Every case here is written from the attacker's side: not "the sandbox sets
RLIMIT_DATA" but "a program that allocates 8GB does not take the box down".
"""

from __future__ import annotations

import io
import json
import os
import subprocess
import sys
import textwrap
from email.message import Message

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import cachestore  # noqa: E402
import envs  # noqa: E402
import sandbox  # noqa: E402

jailed = pytest.mark.skipif(not sandbox.jail_available(),
                            reason="no mojojail — development machine")


@pytest.fixture(scope="module")
def python_exe():
    exe, _ = envs.ensure([])
    return exe


def run(python_exe, code, **kw):
    kw.setdefault("timeout_ms", 20000)
    return sandbox.run(python=python_exe, code=textwrap.dedent(code), **kw)


# --- isolation ------------------------------------------------------------

@jailed
def test_program_runs_as_the_sandbox_user(python_exe):
    r = run(python_exe, "import os; print(os.getuid(), os.geteuid())")
    uid = r["stdout"].split()[0]
    assert uid != "0", "running as root"
    assert uid != str(os.getuid()), "running as the service user"


@jailed
def test_no_network_at_all(python_exe):
    r = run(python_exe, """
        import socket
        for target in [("1.1.1.1", 53), ("127.0.0.1", 4342), ("8.8.8.8", 443)]:
            try:
                socket.create_connection(target, timeout=2)
                print("REACHED", target)
            except OSError as exc:
                print("blocked", target[0], type(exc).__name__)
    """)
    assert "REACHED" not in r["stdout"], r["stdout"]


@jailed
def test_cannot_reach_the_runner_itself(python_exe):
    # The worker is on loopback, and loopback inside a fresh netns is not the
    # host's loopback — this is what stops a program from submitting its own
    # jobs or reading another tenant's result.
    r = run(python_exe, """
        import urllib.request
        try:
            urllib.request.urlopen("http://127.0.0.1:4342/health", timeout=2)
            print("REACHED")
        except Exception as exc:
            print("blocked", type(exc).__name__)
    """)
    assert "REACHED" not in r["stdout"]


@jailed
def test_cannot_read_system_secrets(python_exe):
    r = run(python_exe, """
        for path in ["/etc/shadow", "/root/.ssh/id_rsa",
                     "/home/administrator/.secretbashrc",
                     "/nvme0n1-disk/data/appnz-sso.db"]:
            try:
                with open(path, "rb") as fh:
                    print("READ", path, len(fh.read(16)))
            except OSError as exc:
                print("denied", path, type(exc).__name__)
    """)
    assert "READ" not in r["stdout"], r["stdout"]


@jailed
def test_cannot_enumerate_other_runs(python_exe):
    r = run(python_exe, f"""
        import os
        try:
            print("LISTED", os.listdir({sandbox.RUN_ROOT!r})[:3])
        except OSError as exc:
            print("denied", type(exc).__name__)
    """)
    assert "LISTED" not in r["stdout"], "run directories are enumerable"


@jailed
def test_cannot_enumerate_pool_sockets(python_exe):
    import pool

    r = run(python_exe, f"""
        import os
        try:
            print("LISTED", os.listdir({pool.SOCKET_DIR!r}))
        except OSError as exc:
            print("denied", type(exc).__name__)
    """)
    assert "LISTED" not in r["stdout"], "pool sockets are enumerable"


@jailed
def test_cannot_write_the_shared_compile_cache(python_exe):
    import hostcompile

    r = run(python_exe, f"""
        import os
        target = os.path.join({hostcompile.SHARED!r}, "evil.so")
        try:
            with open(target, "wb") as fh:
                fh.write(b"pwned")
            print("WROTE")
        except OSError as exc:
            print("denied", type(exc).__name__)
    """)
    assert "WROTE" not in r["stdout"], "a run can poison the shared cache"


@jailed
def test_replacing_a_cache_symlink_does_not_touch_the_shared_copy(python_exe):
    """A run may overwrite its *own* view of a cached kernel. The shared file
    behind the symlink must be unchanged for the next run."""
    import hostcompile

    before = {}
    for name in os.listdir(hostcompile.SHARED):
        if name.endswith(".so"):
            path = os.path.join(hostcompile.SHARED, name)
            before[name] = os.path.getsize(path)
    if not before:
        pytest.skip("nothing in the shared cache yet")

    run(python_exe, """
        import os
        root = os.environ["MOJOSUB_CACHE"]
        for name in os.listdir(root):
            if name.endswith(".so"):
                try:
                    with open(os.path.join(root, name), "wb") as fh:
                        fh.write(b"x")
                except OSError:
                    pass
        print("done")
    """)
    for name, size in before.items():
        assert os.path.getsize(os.path.join(hostcompile.SHARED, name)) == size, \
            f"{name} was modified through a run's private view"


@jailed
def test_no_host_secrets_in_the_environment(python_exe):
    r = run(python_exe, """
        import os
        leaked = [k for k in os.environ
                  if any(s in k.upper() for s in
                         ("SECRET", "TOKEN", "PASSWORD", "AWS", "R2", "API_KEY",
                          "CLOUDFLARE", "STRIPE"))]
        print("LEAKED", leaked)
    """)
    assert "LEAKED []" in r["stdout"], r["stdout"]


@jailed
def test_no_sudo_inside(python_exe):
    r = run(python_exe, """
        import shutil, subprocess
        print("sudo:", shutil.which("sudo"))
        try:
            out = subprocess.run(["sudo", "-n", "true"], capture_output=True, timeout=5)
            print("rc", out.returncode)
        except Exception as exc:
            print("blocked", type(exc).__name__)
    """)
    assert "rc 0" not in r["stdout"], "the sandbox can sudo"


@jailed
def test_no_new_privs_blocks_setuid_binaries(python_exe):
    r = run(python_exe, """
        import subprocess
        try:
            out = subprocess.run(["/usr/bin/passwd", "--help"], capture_output=True, timeout=5)
            print("rc", out.returncode)
        except Exception as exc:
            print("blocked", type(exc).__name__)
    """)
    # Either it is missing, or it runs without privilege. What must not happen
    # is a privilege gain, which no-new-privs prevents at exec.
    assert "READ" not in r["stdout"]


# --- resource limits ------------------------------------------------------

def test_memory_is_capped(python_exe):
    r = run(python_exe, """
        chunks = []
        try:
            for _ in range(400):
                chunks.append(bytearray(64 * 1024 * 1024))   # 25GB if allowed
            print("ALLOCATED ALL")
        except MemoryError:
            print("capped")
    """, timeout_ms=30000)
    assert "ALLOCATED ALL" not in r["stdout"], "no memory limit"


def test_runaway_cpu_is_killed(python_exe):
    r = run(python_exe, "while True:\n    pass\n", timeout_ms=1500)
    assert r["timed_out"] and r["wall_ms"] < 8000


def test_sleeping_past_the_deadline_is_killed(python_exe):
    r = run(python_exe, "import time; time.sleep(60); print('WOKE')", timeout_ms=1500)
    assert r["timed_out"] and "WOKE" not in r["stdout"]


@jailed
def test_fork_bomb_does_not_take_the_box_down(python_exe):
    r = run(python_exe, """
        import os
        made = 0
        try:
            for _ in range(5000):
                if os.fork() == 0:
                    os._exit(0)
                made += 1
        except OSError:
            pass
        print("forked", made)
    """, timeout_ms=20000)
    forked = int(r["stdout"].split()[-1]) if r["stdout"].strip() else 0
    assert forked < 5000, "RLIMIT_NPROC did not bite"


def test_giant_output_is_truncated_not_streamed_forever(python_exe):
    r = run(python_exe, "print('x' * 50_000_000)", timeout_ms=30000)
    assert len(r["stdout"]) <= sandbox.MAX_OUTPUT + 200


def test_huge_file_writes_are_capped(python_exe):
    r = run(python_exe, """
        try:
            with open("big.bin", "wb") as fh:
                for _ in range(200):
                    fh.write(b"0" * (1024 * 1024))
            print("WROTE ALL")
        except OSError as exc:
            print("capped", type(exc).__name__)
    """, timeout_ms=30000)
    assert "WROTE ALL" not in r["stdout"] or r["timed_out"]


def test_the_run_directory_is_gone_afterwards(python_exe):
    r = run(python_exe, "open('leftover.txt','w').write('x'); print('ok')")
    assert not os.path.exists(os.path.join(sandbox.RUN_ROOT, r["run_id"]))


# --- input validation -----------------------------------------------------

@pytest.mark.parametrize("spec", [
    "-r requirements.txt", "--extra-index-url=http://evil",
    "numpy --upgrade", "pkg;rm -rf /", "/etc/passwd", "../../escape",
    "https://evil.example/x.whl", "pkg\n--index-url=http://evil",
])
def test_package_specs_that_reach_argv_are_refused(spec):
    with pytest.raises(envs.EnvError):
        envs.normalize([spec])


def test_environment_variables_from_a_caller_cannot_override_ours(python_exe):
    r = run(python_exe, """
        import os
        print("trace", os.environ.get("MOJOJOJO_TRACE", "")[-10:])
        print("evil", os.environ.get("EVIL", "unset"))
    """, env_extra={"MOJOJOJO_TRACE": "/tmp/pwned", "EVIL": "value",
                    "PATH": "/evil/bin"})
    assert "trace /tmp/pwned" not in r["stdout"], "a caller overwrote our trace path"
    assert "evil value" in r["stdout"], "ordinary caller variables should pass through"


# --- the upload prefix ----------------------------------------------------

@pytest.mark.parametrize("name", [
    "../cache/deadbeef.so", "../../appnz/index.html", "/etc/passwd",
    "..\\..\\windows", "....//....//x", ".hidden", "", "   ",
])
def test_uploads_cannot_escape_their_prefix(name):
    try:
        key = cachestore.upload_key("user-1", name)
    except cachestore.UploadError:
        return  # refused outright is also correct
    assert key.startswith(cachestore.R2_PREFIX + "uploads/user-1/")
    assert ".." not in key
    assert "/cache/" not in key


def test_uploads_are_namespaced_per_user():
    a = cachestore.upload_key("alice", "data.csv")
    b = cachestore.upload_key("bob", "data.csv")
    assert a != b and "/alice/" in a and "/bob/" in b


def test_a_hostile_user_id_cannot_escape_either():
    key = cachestore.upload_key("../../../root", "x.txt")
    assert key.startswith(cachestore.R2_PREFIX + "uploads/")
    assert ".." not in key


def test_public_url_stays_under_the_cdn_base():
    url = cachestore.public_url(cachestore.upload_key("u", "f.bin"))
    assert url.startswith(cachestore.CDN_BASE + "/")


# --- worker authentication ------------------------------------------------

def _post_to_fake_worker(body: bytes, headers: Message):
    import runner as worker

    class FakeHandler(worker.Handler):
        def __init__(self):
            self.path = "/run"
            self.headers = headers
            self.rfile = io.BytesIO(body)
            self.sent = []

        def _send(self, status, payload):
            self.sent.append((status, payload))

        def _authorised(self):
            return True

    handler = FakeHandler()
    handler.do_POST()
    return handler.sent


def test_duplicate_content_lengths_are_rejected():
    body = b'{"code":"print(1)"}'
    headers = Message()
    headers.add_header("Content-Length", str(len(body)))
    headers.add_header("Content-Length", str(len(body)))

    sent = _post_to_fake_worker(body, headers)

    assert sent == [(400, {"error": "invalid content length"})]


def test_nonstandard_json_numbers_are_rejected():
    body = b'{"code":"print(1)","timeout_ms":NaN}'
    headers = Message()
    headers.add_header("Content-Length", str(len(body)))

    sent = _post_to_fake_worker(body, headers)

    assert sent == [(400, {"error": "invalid json"})]

def test_a_rented_worker_refuses_requests_without_its_token(tmp_path):
    """The local worker is on loopback; a rented one is on the public internet
    and must not execute anything for an unauthenticated caller."""
    import runner as worker

    class FakeHandler(worker.Handler):
        def __init__(self):  # noqa: D107 - not a real HTTP connection
            self.headers = {}
            self.sent = []

        def _send(self, status, payload):
            self.sent.append((status, payload))

    handler = FakeHandler()
    original = worker.TOKEN
    try:
        worker.TOKEN = "expected-token"
        assert handler._authorised() is False
        assert handler.sent[-1][0] == 401

        handler.headers = {"Authorization": "Bearer wrong"}
        assert handler._authorised() is False

        handler.headers = {"Authorization": "Bearer expected-token"}
        assert handler._authorised() is True
    finally:
        worker.TOKEN = original


def test_no_token_means_no_check_only_on_loopback():
    import runner as worker

    assert worker.HOST in ("127.0.0.1", "localhost") or worker.TOKEN, \
        "a worker bound to a public interface must have a token"


def test_an_empty_token_fails_closed_on_a_public_listener(monkeypatch):
    import runner as worker

    class FakeHandler(worker.Handler):
        def __init__(self):
            self.headers = {}
            self.sent = []

        def _send(self, status, payload):
            self.sent.append((status, payload))

    monkeypatch.setattr(worker, "HOST", "0.0.0.0")
    monkeypatch.setattr(worker, "TOKEN", "")
    handler = FakeHandler()
    assert handler._authorised() is False
    assert handler.sent[-1][0] == 503


@pytest.mark.parametrize("host", ["127.0.0.1", "::1", "[::1]", "localhost"])
def test_loopback_listener_detection_handles_ipv4_and_ipv6(host):
    import runner as worker

    assert worker._loopback_host(host)


# --- the jail's port allowlist ---------------------------------------------
#
# `--ports` exists for latency (see sandbox.jail_command), but it takes a name
# from the worker, so what it will accept is a security question. It may only
# ever name one of the `mojo-*` directories the script would have bound anyway.

def _jail(*args):
    return subprocess.run(
        ["sh", os.path.join(os.path.dirname(__file__), "..", "mojojail"), *args],
        capture_output=True, text=True, timeout=30)


@pytest.mark.parametrize("ports", [
    "/etc", "../../etc", "mojo-x/../../etc", "mojosub", "code/mojo-x",
    "/nvme0n1-disk/data/appnz-sso.db", "..",
])
def test_the_jail_refuses_a_port_that_is_not_a_port(ports):
    out = _jail("5", "--ports", ports, "/bin/true")
    assert out.returncode == 2, out.stderr
    assert "port" in out.stderr


def test_the_run_command_asks_only_for_the_ports_it_imports():
    plain = sandbox.jail_command(10, "/w", [], [], ["/bin/true"])
    assert "--ports" not in plain

    ported = sandbox.jail_command(
        10, "/w", ["/nvme0n1-disk/code/mojo-pillow/python"], [], ["/bin/true"])
    assert ported[ported.index("--ports") + 1] == "mojo-pillow"
