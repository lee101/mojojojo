"""Internal HTTP protocol for portable mojojojo vector workers."""

from __future__ import annotations

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from .engine import VectorEngine


MAX_BODY = 8 << 20


class Handler(BaseHTTPRequestHandler):
    server_version = "mojojojo-vector/1"

    @property
    def engine(self) -> VectorEngine:
        return self.server.engine  # type: ignore[attr-defined]

    def _send(self, status: int, value: object) -> None:
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self) -> bool:
        expected = self.server.token  # type: ignore[attr-defined]
        return not expected or self.headers.get("Authorization") == "Bearer " + expected

    def _body(self) -> dict:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > MAX_BODY:
            raise ValueError("request body is empty or too large")
        value = json.loads(self.rfile.read(length))
        if not isinstance(value, dict):
            raise ValueError("json body must be an object")
        return value

    def _collection_route(self) -> tuple[str, str] | None:
        parts = urlparse(self.path).path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "collections":
            return parts[1], parts[2]
        return None

    def do_GET(self) -> None:  # noqa: N802
        if not self._authorized():
            self._send(401, {"ok": False, "error": "unauthorized"})
        elif self.path == "/health":
            self._send(200, self.engine.health())
        else:
            self._send(404, {"ok": False, "error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if not self._authorized():
            self._send(401, {"ok": False, "error": "unauthorized"})
            return
        try:
            if self.path == "/benchmark":
                body = self._body()
                self._send(200, self.engine.benchmark(int(body.get("vectors", 10_000)), int(body.get("iterations", 10))))
                return
            route = self._collection_route()
            if route is None:
                self._send(404, {"ok": False, "error": "not found"})
                return
            collection_id, operation = route
            body = self._body()
            if operation == "upsert":
                result = self.engine.upsert(collection_id, body.get("documents") or [])
            elif operation == "search":
                result = self.engine.search(collection_id, body.get("query", ""), int(body.get("limit", 10)))
            else:
                self._send(404, {"ok": False, "error": "not found"})
                return
            self._send(200, {"ok": True, **result})
        except (ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
            self._send(400, {"ok": False, "error": str(error)})
        except Exception as error:  # the control plane needs a useful backend failure
            self._send(500, {"ok": False, "error": str(error)})

    def do_DELETE(self) -> None:  # noqa: N802
        if not self._authorized():
            self._send(401, {"ok": False, "error": "unauthorized"})
            return
        route = self._collection_route()
        if route is None or route[1] != "index":
            self._send(404, {"ok": False, "error": "not found"})
            return
        try:
            self.engine.delete(route[0])
            self._send(200, {"ok": True})
        except (ValueError, OSError) as error:
            self._send(400, {"ok": False, "error": str(error)})

    def log_message(self, message: str, *args: object) -> None:
        print("vector-worker", self.address_string(), message % args, flush=True)


def build_engine() -> VectorEngine:
    root = Path(__file__).resolve().parents[1]
    sibling = root.parent
    return VectorEngine(
        os.getenv("VECTOR_DATA_DIR", str(root / ".vector-data")),
        os.getenv("VECTOR_MODEL_PATH", str(sibling / "gobed/model/modelint8_512dim.safetensors")),
        os.getenv("VECTOR_TOKENIZER_PATH", str(sibling / "gobed/model/tokenizer.json")),
        os.getenv("MOJO_EMBED_LIBRARY", str(sibling / "mojo-embed/build/libmojo_embed.so")),
        os.getenv("MOJO_RUNTIME_DIR", str(sibling / "mojosub/.pixi/envs/default/lib")),
    )


def main() -> None:
    address = os.getenv("VECTOR_LISTEN", "127.0.0.1")
    port = int(os.getenv("VECTOR_PORT", "4344"))
    server = ThreadingHTTPServer((address, port), Handler)
    server.engine = build_engine()  # type: ignore[attr-defined]
    server.token = os.getenv("VECTOR_WORKER_TOKEN", "")  # type: ignore[attr-defined]
    print(f"mojojojo vector worker on {address}:{port} ({server.engine.kernel.name})", flush=True)  # type: ignore[attr-defined]
    server.serve_forever()


if __name__ == "__main__":
    main()
