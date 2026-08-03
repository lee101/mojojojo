// Tests for the parts of the browser runtime that decide where code runs.
// `node --test web/mojojojo.test.mjs` (or `bun test`). No browser needed: the
// routing decision is pure, which is the reason it is a separate function.

import { test } from "node:test";
import assert from "node:assert/strict";

import { plan, imports, createVectorProject, vectorUpsert, vectorSearch } from "./mojojojo.js";

test("finds top-level imports in both spellings", () => {
  const code = "import numpy as np\nfrom os.path import join\nimport torch.nn\n";
  assert.deepEqual(imports(code).sort(), ["numpy", "os", "torch"]);
});

test("ignores the word import inside a string", () => {
  assert.deepEqual(imports('x = "import torch"\n'), []);
});

test("plain code stays in the browser", () => {
  const d = plan("print(sum(range(10)))");
  assert.equal(d.where, "browser");
});

test("wasm-capable packages stay in the browser", () => {
  const d = plan("import numpy as np\nprint(np.arange(3))");
  assert.equal(d.where, "browser");
});

test("an import with no wasm build goes to the server, and says which", () => {
  const d = plan("import torch\nprint(torch.__version__)");
  assert.equal(d.where, "server");
  assert.match(d.why, /torch/);
});

test("network libraries go to the server — wasm has no sockets", () => {
  assert.equal(plan("import requests").where, "server");
  assert.equal(plan("import socket").where, "server");
});

test("a GPU request always goes to the server", () => {
  assert.equal(plan("print(1)", { gpu: true }).where, "server");
});

test("asking for the native path goes to the server", () => {
  assert.equal(plan("print(1)", { native: true }).where, "server");
});

test("a package outside the wasm registry goes to the server", () => {
  const d = plan("print(1)", { packages: ["polars==1.0"] });
  assert.equal(d.where, "server");
  assert.match(d.why, /polars/);
});

test("an explicit choice is honoured over the heuristic", () => {
  assert.equal(plan("import torch", { where: "browser" }).where, "browser");
  assert.equal(plan("print(1)", { where: "server" }).where, "server");
});

test("every decision carries a reason", () => {
  for (const code of ["print(1)", "import torch", "import numpy"]) {
    assert.ok(plan(code).why.length > 0, code);
  }
});

test("Mojo goes to the server, because there is no wasm Mojo", () => {
  assert.equal(plan("def main(): pass", { language: "mojo" }).where, "server");
  assert.equal(plan("print(1)", { language: "python" }).where, "browser");
  assert.equal(plan("print(1)", {}).where, "browser");
});

test("a caller who insists on the browser is not billed behind their back", () => {
  // `where: "browser"` is a cost ceiling, not a hint. Mojo cannot run there —
  // so the answer is a free failure with a reason, not a surprise charge.
  const d = plan("def main(): pass", { language: "mojo", where: "browser" });
  assert.equal(d.where, "browser");
  assert.match(d.why, /compiled on our machines/);
});

test("vector helpers preserve tenant auth and request shape", async () => {
  const calls = [];
  const oldFetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options, body: options.body ? JSON.parse(options.body) : null });
    const body = url.endsWith("/v1/vector/projects")
      ? { project: { id: "vec_1" } }
      : url.endsWith("/search") ? { matches: [], backend: "mojo-int8-exact" }
        : { ok: true, upserted: 1 };
    return { ok: true, status: 200, json: async () => body };
  };
  try {
    assert.equal((await createVectorProject("Docs", { endpoint: "https://example.test", apiKey: "key" })).id, "vec_1");
    await vectorUpsert("vec_1", [{ id: "guide", text: "Mojo" }], { endpoint: "https://example.test", apiKey: "key" });
    assert.equal((await vectorSearch("vec_1", "fast", { endpoint: "https://example.test", apiKey: "key", limit: 4 })).backend, "mojo-int8-exact");
    assert.equal(calls[0].options.headers.Authorization, "Bearer key");
    assert.equal(calls[1].body.documents[0].id, "guide");
    assert.equal(calls[2].body.limit, 4);
  } finally {
    globalThis.fetch = oldFetch;
  }
});
