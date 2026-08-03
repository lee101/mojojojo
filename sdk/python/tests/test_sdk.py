"""SDK tests. No network: a local HTTP server stands in for the service, which
is also how the request shape gets pinned — it is somebody else's API and a
silently dropped field costs the caller money.
"""

from __future__ import annotations

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from mojojojo import (AuthError, Client, InsufficientCredits,  # noqa: E402
                      MojojojoError, precompile, remote)

RECEIVED: dict = {}
REPLY: dict = {}
STATUS = {"code": 200}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _respond(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        RECEIVED.clear()
        raw = self.rfile.read(length)
        if self.headers.get("Content-Type") == "application/json":
            RECEIVED.update(json.loads(raw))
        else:
            RECEIVED["_body"] = raw
        RECEIVED["_path"] = self.path
        RECEIVED["_project"] = self.headers.get("X-Project-ID", "")
        RECEIVED["_auth"] = self.headers.get("Authorization", "")
        RECEIVED["_agent"] = self.headers.get("User-Agent", "")
        self._respond(REPLY, STATUS["code"])

    def do_GET(self):
        RECEIVED.clear()
        RECEIVED["_path"] = self.path
        RECEIVED["_auth"] = self.headers.get("Authorization", "")
        self._respond(REPLY, STATUS["code"])

    def do_PUT(self):
        self.do_POST()

    def do_DELETE(self):
        self.do_POST()


@pytest.fixture(scope="module")
def server():
    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{httpd.server_port}"
    httpd.shutdown()


@pytest.fixture
def client(server):
    STATUS["code"] = 200
    REPLY.clear()
    REPLY.update({"ok": True, "stdout": "45\n", "wall_ms": 120.0,
                  "billable_ms": 90.0, "warm_start": True,
                  "accel": {"native_calls": 3, "compile_ms": 0},
                  "env": {"packages": ["numpy", "pandas"], "auto": ["pandas"]},
                  "charge": {"billed_ms": 90.0, "credits": 0, "owed_ms": 90.0,
                             "balance": 999, "ms_per_credit": 10000}})
    return Client(api_key="mj_live_test", endpoint=server)


def test_run_sends_what_the_service_expects(client):
    client.run("print(1)", packages=["numpy"], gpu=True, timeout_ms=5000)
    assert RECEIVED["code"] == "print(1)"
    assert RECEIVED["packages"] == ["numpy"]
    assert RECEIVED["gpu"] is True
    assert RECEIVED["timeout_ms"] == 5000
    assert RECEIVED["auto_install"] is True
    assert RECEIVED["_auth"] == "Bearer mj_live_test"
    assert "mojojojo-python" in RECEIVED["_agent"]


def test_indented_code_is_dedented(client):
    client.run("""
        for i in range(3):
            print(i)
    """)
    assert RECEIVED["code"].lstrip("\n").startswith("for i in range(3):")


def test_result_exposes_cost_and_acceleration(client):
    r = client.run("print(1)")
    assert r.ok and r.stdout == "45\n"
    assert r.charge.credits == 0 and r.charge.billed_ms == 90.0
    assert r.charge.usd == pytest.approx(90.0 / 10000 * 0.001)
    assert r.accel.native_calls == 3
    assert r.auto_installed == ["pandas"]
    assert r.warm_start is True


def test_precompile_is_an_explicit_representative_run(client):
    REPLY["accelerated"] = ["backtest"]
    REPLY["accel"] = {"compiled": 1, "native_calls": 0, "compile_ms": 4300}
    prepared = client.precompile("print(backtest(prices))")
    assert prepared.ready is True
    assert prepared.kernels == ["backtest"]
    assert "prepared" in prepared.message
    assert prepared.result.charge.billed_ms == 90.0
    assert RECEIVED["source"] == "sdk-python:precompile"


def test_top_level_precompile_accepts_an_explicit_client(client):
    prepared = precompile("print(1)", client=client)
    assert prepared.ready is True


def test_precompile_cli_reports_the_stage(client, monkeypatch, capsys):
    from mojojojo import cli

    monkeypatch.setattr(cli, "Client", lambda: client)
    assert cli.main(["precompile", "-c", "print(1)", "--json"]) == 0
    output = json.loads(capsys.readouterr().out)
    assert output["ready"] is True
    assert output["run"]["ok"] is True
    assert RECEIVED["source"] == "sdk-python:cli:precompile"


def test_check_raises_on_a_failed_program(client):
    REPLY.update({"ok": False, "exit_code": 1, "stderr": "boom"})
    with pytest.raises(MojojojoError) as exc:
        client.run("raise SystemExit(1)", check=True)
    assert "boom" in str(exc.value)


def test_auth_error_points_at_the_key(client):
    STATUS["code"] = 401
    REPLY.clear()
    REPLY.update({"error": "invalid or revoked API key"})
    with pytest.raises(AuthError) as exc:
        client.run("print(1)")
    assert "MOJOJOJO_API_KEY" in str(exc.value)


def test_out_of_credits_is_its_own_exception(client):
    STATUS["code"] = 402
    REPLY.clear()
    REPLY.update({"error": "out of credits"})
    with pytest.raises(InsufficientCredits):
        client.run("print(1)")


def test_unreachable_endpoint_says_so():
    client = Client(api_key="k", endpoint="http://127.0.0.1:1")
    with pytest.raises(MojojojoError) as exc:
        client.run("print(1)")
    assert "could not reach" in str(exc.value)


# --- @remote ---

def test_remote_ships_source_and_returns_the_value(client):
    import base64

    payload = base64.b64encode(json.dumps({"answer": 42}).encode()).decode()
    REPLY.update({"ok": True,
                  "stdout": "working\n__mojojojo_value__:" + payload + "\n"})

    @remote(packages=["numpy"], client=client)
    def compute(a, b):
        return {"answer": a * b}

    assert compute(6, 7) == {"answer": 42}
    assert "def compute(a, b):" in RECEIVED["code"]
    assert "@remote" not in RECEIVED["code"], "our decorator must not travel"
    assert RECEIVED["packages"] == ["numpy"]
    assert '"args": [6, 7]' in RECEIVED["code"] or "[6, 7]" in RECEIVED["code"]


def test_remote_keeps_the_local_function(client):
    @remote(client=client)
    def double(x):
        return x * 2

    assert double.local(21) == 42


def test_remote_program_output_excludes_the_value_marker(client):
    import base64

    payload = base64.b64encode(json.dumps(1).encode()).decode()
    REPLY.update({"ok": True, "stdout": "hello\n__mojojojo_value__:" + payload + "\n"})

    @remote(client=client)
    def f(x):
        return x

    f(1)
    # The marker line is consumed; what the program printed is untouched.
    result = client.run("print('hello')")
    assert result.stdout == "hello\n"
    assert result.value == 1


def test_credits_reports_the_balance_and_what_it_buys(client):
    REPLY.clear()
    REPLY.update({"signed_in": True, "credits": 3780,
                  "balance": {"free": 1380, "plan": 0, "paid": 2400, "usd": 3.78},
                  "buys": {"cpu_ms": 37800000, "gpu_ms": 7560000},
                  "owed_ms": 119.7, "total_ms": 119.7, "ms_per_credit": 10000,
                  "welcome_granted": True, "spent_credits": 16,
                  "ledger": [{"at": "2026-07-26T19:45:00Z", "delta": -14,
                              "reason": "exec:gpu:28000ms", "app": "mojojojo"}],
                  "top_up": "https://mojojojo.cc/credits"})
    data = client.credits()
    assert data["credits"] == 3780
    # The number a long job actually needs: how many ms it can still pay for.
    assert data["buys"]["cpu_ms"] == data["credits"] * data["ms_per_credit"]
    assert data["top_up"].startswith("http")


def test_credits_cli_prints_a_balance_and_where_to_top_up(client, capsys):
    from mojojojo import cli

    REPLY.clear()
    REPLY.update({"signed_in": True, "credits": 1000,
                  "balance": {"free": 1000, "plan": 0, "paid": 0, "usd": 1.0},
                  "buys": {"cpu_ms": 10000000, "gpu_ms": 2000000},
                  "owed_ms": 0, "total_ms": 0, "ledger": [],
                  "top_up": "https://mojojojo.cc/credits"})
    assert cli._credits(client, as_json=False) == 0
    out = capsys.readouterr().out
    assert "1000 credits" in out
    assert "mojojojo.cc/credits" in out


def test_credits_cli_tells_a_stranger_how_to_sign_up(client, capsys):
    REPLY.clear()
    REPLY.update({"signed_in": False, "signup": "https://mojojojo.cc/auth/signup",
                  "welcome_credits": 1000})
    from mojojojo import cli

    assert cli._credits(client, as_json=False) == 2
    assert "mojojojo.cc/auth/signup" in capsys.readouterr().err


# --- Mojo programs ---------------------------------------------------------

def test_a_mojo_run_says_so_and_reports_its_build(client):
    """The build is waited for and never billed, so it is reported separately
    from the milliseconds the program itself ran."""
    REPLY.update({"language": "mojo", "build_ms": 5300.0, "stdout": "3.14\n"})
    result = client.run("def main(): pass", language="mojo")

    assert RECEIVED["language"] == "mojo"
    assert result.language == "mojo"
    assert result.build_ms == 5300.0


def test_python_is_the_default_language(client):
    result = client.run("print(45)")
    assert RECEIVED["language"] == "python"
    assert result.language == "python"


def test_multi_file_mojo_project_is_preserved(client):
    files = {"main.mojo": "from helper import answer\ndef main(): print(answer())\n",
             "helper.mojo": "def answer() -> Int: return 42\n"}
    client.run(files=files, entrypoint="main.mojo", language="mojo")
    assert RECEIVED["code"] == ""
    assert RECEIVED["files"] == files
    assert RECEIVED["entrypoint"] == "main.mojo"


def test_project_creation_and_raw_zip_deploy(client):
    REPLY.clear()
    REPLY.update({"project": {"id": "p1", "name": "Fractal"}})
    assert client.create_project("Fractal")["id"] == "p1"
    assert RECEIVED["runtime"] == "static"

    REPLY.clear()
    REPLY.update({"deployment": {"id": "d1", "url": "https://static.example/site"}})
    deployed = client.deploy("p1", b"PK\x03\x04test")
    assert deployed["id"] == "d1"
    assert RECEIVED["_path"] == "/v1/deployments"
    assert RECEIVED["_project"] == "p1"
    assert RECEIVED["_body"].startswith(b"PK")


def test_studio_workspace_publish_and_submit(client):
    REPLY.clear()
    REPLY.update({"ok": True, "file_count": 2})
    client.save_project_files("p1", {"index.html": "<h1>ok</h1>",
                                     "mojojojo.json": '{"runtime":"app"}'})
    assert RECEIVED["_path"] == "/v1/projects/p1/files"
    assert RECEIVED["files"]["index.html"] == "<h1>ok</h1>"

    REPLY.clear()
    REPLY.update({"deployment": {"id": "d2", "url": "https://example/p/p1/"}})
    assert client.publish_project("p1")["id"] == "d2"
    assert RECEIVED["_path"] == "/v1/projects/p1/publish"

    REPLY.clear()
    REPLY.update({"submission": {"id": "s1", "status": "pending"}})
    assert client.submit_project("p1")["status"] == "pending"
    assert RECEIVED["_path"] == "/v1/projects/p1/submit"


def test_vector_project_upsert_and_search(client):
    REPLY.clear()
    REPLY.update({"project": {"id": "vec_1", "name": "Docs", "tier": "exact"}})
    project = client.create_vector_project("Docs")
    assert project["id"] == "vec_1"
    assert RECEIVED["_path"] == "/v1/vector/projects"

    REPLY.clear()
    REPLY.update({"ok": True, "vector_count": 1, "upserted": 1})
    client.vector_upsert("vec_1", [{"id": "guide", "text": "Mojo guide"}])
    assert RECEIVED["_path"] == "/v1/vector/projects/vec_1/upsert"
    assert RECEIVED["documents"][0]["id"] == "guide"

    REPLY.clear()
    REPLY.update({"ok": True, "matches": [{"id": "guide", "score": 0.9}],
                  "backend": "mojo-int8-exact", "latency_ms": 0.7})
    found = client.vector_search("vec_1", "fast code", limit=5)
    assert found["matches"][0]["id"] == "guide"
    assert RECEIVED["limit"] == 5


def test_managed_data_crud_and_vector_link(client):
    REPLY.clear()
    REPLY.update({"database": {"id": "db_1", "name": "Customers",
                               "vector_project_id": "vec_1"}})
    database = client.create_database("Customers", vector_project_id="vec_1")
    assert database["id"] == "db_1"
    assert RECEIVED["_path"] == "/v1/data/namespaces"
    assert RECEIVED["vector_project_id"] == "vec_1"

    REPLY.clear()
    REPLY.update({"ok": True, "vector": {"status": "ready"}})
    client.data_upsert("db_1", "customer:123", {"plan": "starter"},
                       search_text="Ada uses Mojo")
    assert RECEIVED["_path"] == "/v1/data/namespaces/db_1/records"
    assert RECEIVED["value"] == {"plan": "starter"}

    REPLY.clear()
    REPLY.update({"records": [{"key": "customer:123", "value": {"plan": "starter"}}]})
    assert client.data_records("db_1")[0]["key"] == "customer:123"
    assert RECEIVED["_path"] == "/v1/data/namespaces/db_1/records"

    REPLY.clear()
    REPLY.update({"ok": True, "deleted": "customer:123"})
    client.data_delete("db_1", "customer:123")
    assert RECEIVED["key"] == "customer:123"
