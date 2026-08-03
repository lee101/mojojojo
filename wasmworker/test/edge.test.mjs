/**
 * The edge tier, tested against the real runtime.
 *
 * `wrangler dev --local` runs the same workerd that Cloudflare runs, so these
 * are not mocks: the Python below is executed by Pyodide inside an isolate,
 * exactly as it would be in a datacentre. Booting it takes the better part of a
 * minute, so one server serves the whole file.
 *
 * Skips itself when wrangler is not installed, the same way the browser pass
 * does — a checkout without node_modules should still be able to run the suite.
 */

import { test, before, after, describe } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = dirname(HERE);
const WRANGLER = join(ROOT, "node_modules", ".bin", "wrangler");
const PORT = Number(process.env.EDGE_TEST_PORT || 8853);
const BASE = `http://127.0.0.1:${PORT}`;
const BOOT_TIMEOUT_MS = 120_000;

let server = null;

async function reachable() {
  // Checks *what* answered, not just that something did: this box runs other
  // services, and a stranger on the port produced nineteen confusing failures
  // instead of one clear one.
  try {
    const r = await fetch(`${BASE}/health`, { signal: AbortSignal.timeout(2000) });
    if (!r.ok) return false;
    const body = await r.json();
    if (body.where !== "edge") {
      throw new Error(`something else is already listening on ${PORT}`);
    }
    return true;
  } catch (err) {
    if (String(err.message).includes("already listening")) throw err;
    return false;
  }
}

// The control plane's own credential. Set in .dev.vars for these tests, and a
// `wrangler secret` in production; without it the worker is open to anyone who
// finds the URL, which is our Cloudflare bill.
const SHARED_SECRET = "test-edge-secret";

async function run(body, timeoutMs = 60_000) {
  const r = await fetch(`${BASE}/run`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${SHARED_SECRET}`,
    },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(timeoutMs),
  });
  return { status: r.status, body: await r.json() };
}

before(async () => {
  if (!existsSync(WRANGLER)) return;
  server = spawn(WRANGLER, ["dev", "--port", String(PORT), "--local"], {
    cwd: ROOT, stdio: "ignore", detached: true,
  });
  const deadline = Date.now() + BOOT_TIMEOUT_MS;
  while (Date.now() < deadline) {
    if (await reachable()) return;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error("the edge worker did not come up");
});

after(() => {
  if (server) try { process.kill(-server.pid, "SIGKILL"); } catch { /* gone */ }
});

const skip = !existsSync(WRANGLER)
  ? { skip: "wrangler is not installed (npm i in wasmworker/)" }
  : {};

describe("the edge tier", skip, () => {
  test("runs a program and reports it like every other tier", async () => {
    const { body } = await run({ code: "print(sum(range(100)))" });
    assert.equal(body.ok, true);
    assert.equal(body.stdout, "4950\n");
    assert.equal(body.where, "edge");
    assert.equal(body.exit_code, 0);
    // Billing is the program's own milliseconds — there is no compiler and no
    // warm phase here, so wall and billable are the same number.
    assert.ok(body.billable_ms > 0);
    assert.ok(body.billable_ms <= body.wall_ms + 0.001);
  });

  test("stdin, argv and exit codes all arrive", async () => {
    const { body } = await run({
      code: "import sys\nprint(sys.stdin.read().strip(), sys.argv[1])\nsys.exit(3)",
      stdin: "from-stdin",
      args: ["from-argv"],
    });
    assert.equal(body.stdout.trim(), "from-stdin from-argv");
    assert.equal(body.exit_code, 3);
    assert.equal(body.ok, false);
  });

  test("a traceback is the user's, not ours", async () => {
    const { body } = await run({ code: "x = 1\nraise ValueError('boom')" });
    assert.match(body.stderr, /ValueError: boom/);
    assert.match(body.stderr, /line 2/);
    assert.equal(body.ok, false);
  });

  test("a syntax error is reported, not crashed", async () => {
    const { body } = await run({ code: "def (:" });
    assert.match(body.stderr, /SyntaxError/);
    assert.equal(body.ok, false);
  });

  test("a runaway loop is stopped and says so", async () => {
    const { body } = await run({ code: "while True:\n    pass", timeout_ms: 1500 });
    assert.equal(body.timed_out, true);
    assert.equal(body.exit_code, 124);
    assert.match(body.stderr, /time limit exceeded/);
    // Our own deadline, not the platform's 30s backstop.
    assert.ok(body.wall_ms < 6000, `took ${body.wall_ms}ms`);
  });

  test("output is capped rather than returned whole", async () => {
    const { body } = await run({ code: "print('x' * 2_000_000)" });
    assert.ok(body.stdout.length < 300_000);
    assert.match(body.stdout, /truncated/);
  });

  // --- the isolate is the sandbox, so the doors out of it are what matter ---

  for (const module of ["js", "pyodide", "ctypes", "socket", "subprocess"]) {
    test(`\`import ${module}\` cannot reach the host`, async () => {
      const { body } = await run({ code: `import ${module}` });
      assert.equal(body.ok, false);
      assert.match(body.stderr, /not available at the edge/);
    });
  }

  test("the host is still closed on a second request to the same isolate", async () => {
    // Isolates are reused. A door opened by one caller stays open for the next
    // unless the block is installed once and left there.
    await run({ code: "print(1)" });
    const { body } = await run({ code: "from js import fetch" });
    assert.equal(body.ok, false);
    assert.match(body.stderr, /not available at the edge/);
  });

  test("a program cannot see our environment", async () => {
    const { body } = await run({
      code: "import os\nprint(sorted(k for k in os.environ if 'TOKEN' in k.upper()))",
    });
    assert.equal(body.stdout.trim(), "[]");
  });

  test("the standard library that has no host access still works", async () => {
    const { body } = await run({
      code: "import json, math, urllib.parse\n"
        + "print(json.dumps({'a': math.floor(2.7)}), urllib.parse.quote('a b'))",
    });
    assert.equal(body.stdout.trim(), '{"a": 2} a%20b');
  });

  // --- what the edge refuses, so the router can send it elsewhere ---

  for (const [name, body_] of [
    ["a GPU", { code: "print(1)", gpu: true }],
    ["Mojo", { code: "def main(): pass", language: "mojo" }],
    ["packages", { code: "import numpy", packages: ["numpy"] }],
  ]) {
    test(`${name} is refused with a retry-elsewhere, not attempted`, async () => {
      const { status, body } = await run(body_);
      assert.equal(status, 409);
      assert.equal(body.retry_elsewhere, true);
    });
  }

  test("an empty program is a 400", async () => {
    const { status } = await run({ code: "   " });
    assert.equal(status, 400);
  });

  test("health says what this tier is", async () => {
    const r = await fetch(`${BASE}/health`);
    const body = await r.json();
    assert.equal(body.ok, true);
    assert.equal(body.where, "edge");
    assert.equal(body.runtime, "pyodide");
  });
});

// --- tickets: how a browser reaches the edge without a detour ------------

import { createHmac } from "node:crypto";

const SECRET = SHARED_SECRET;

function ticketFor(claims) {
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const sig = createHmac("sha256", SECRET).update(payload).digest("hex");
  return `${payload}.${sig}`;
}

async function runWithHeaders(body, headers) {
  const r = await fetch(`${BASE}/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(60_000),
  });
  return { status: r.status, body: await r.json() };
}

describe("tickets", { ...skip, concurrency: false }, () => {
  // These need the shared secret set, which `dev.sh`/the test runner does via
  // .dev.vars. Without it the worker is open and the ticket path is not
  // exercised — better skipped than passing vacuously.
  const configured = existsSync(join(ROOT, ".dev.vars"));
  const maybe = configured ? {} : { skip: "no .dev.vars with EDGE_TOKEN" };

  test("a valid ticket is admitted and comes back with a receipt", maybe, async () => {
    const claims = {
      u: "user-1", x: Math.floor(Date.now() / 1000) + 120, m: 5000, n: "nonce-1",
    };
    const { status, body } = await runWithHeaders(
      { code: "print(6*7)" }, { "X-Mojojojo-Ticket": ticketFor(claims) });
    assert.equal(status, 200);
    assert.equal(body.stdout, "42\n");
    assert.equal(body.ticket, "nonce-1");
    // The receipt is over the nonce and the milliseconds, so the caller cannot
    // report a smaller bill than they used.
    const want = createHmac("sha256", SECRET)
      .update(`nonce-1:${body.billable_ms.toFixed(3)}`).digest("hex");
    assert.equal(body.receipt, want);
  });

  test("an expired ticket is refused", maybe, async () => {
    const claims = {
      u: "user-1", x: Math.floor(Date.now() / 1000) - 5, m: 5000, n: "old",
    };
    const { status } = await runWithHeaders(
      { code: "print(1)" }, { "X-Mojojojo-Ticket": ticketFor(claims) });
    assert.equal(status, 401);
  });

  test("a forged ticket is refused", maybe, async () => {
    const payload = Buffer.from(JSON.stringify({
      u: "user-1", x: Math.floor(Date.now() / 1000) + 120, m: 5000, n: "forged",
    })).toString("base64url");
    const { status } = await runWithHeaders(
      { code: "print(1)" }, { "X-Mojojojo-Ticket": `${payload}.deadbeef` });
    assert.equal(status, 401);
  });

  test("no ticket and no shared secret is refused", maybe, async () => {
    const { status } = await runWithHeaders({ code: "print(1)" }, {});
    assert.equal(status, 401);
  });

  test("the ticket's budget caps the run, whatever the request asked for", maybe, async () => {
    const claims = {
      u: "user-1", x: Math.floor(Date.now() / 1000) + 120, m: 800, n: "small",
    };
    const { body } = await runWithHeaders(
      { code: "while True:\n    pass", timeout_ms: 30000 },
      { "X-Mojojojo-Ticket": ticketFor(claims) });
    assert.equal(body.timed_out, true);
    assert.ok(body.wall_ms < 4000, `ran for ${body.wall_ms}ms on an 800ms ticket`);
  });
});
