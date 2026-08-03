import { test } from "node:test";
import assert from "node:assert/strict";

import {
  capable, costUSD, priceUSD, marginUSD, route, shouldFailOver,
  EDGE_MIN_MS, EDGE_MAX_MS, imports,
} from "./route.js";

const FLEET = { edge: { up: true }, home: { up: true, queued: 0 }, gpu: { up: true } };

// --- what each tier can take -------------------------------------------

test("a stdlib-only program can run anywhere", () => {
  const { can } = capable("import math\nprint(math.sqrt(2))");
  assert.deepEqual(can, { browser: true, edge: true, home: true, rented: true });
});

test("numpy is fine in the browser and impossible at the edge", () => {
  // Pyodide can fetch a wheel; a Worker isolate has no time to.
  const { can, why } = capable("import numpy as np\nprint(np.arange(3))");
  assert.equal(can.browser, true);
  assert.equal(can.edge, false);
  assert.match(why.edge, /numpy/);
});

test("torch is a server program however it is asked for", () => {
  const { can } = capable("import torch\nprint(torch.cuda.is_available())");
  assert.equal(can.browser, false);
  assert.equal(can.edge, false);
  assert.equal(can.home, true);
});

test("Mojo and GPU close both wasm tiers", () => {
  assert.equal(capable("def main(): pass", { language: "mojo" }).can.edge, false);
  assert.equal(capable("print(1)", { gpu: true }).can.browser, false);
});

// --- the money ----------------------------------------------------------

test("the edge charges a fixed fee to serve at all", () => {
  // Which is what makes short programs the awkward case: at 1ms the fee is
  // most of the bill, and by 100ms the CPU has overtaken it.
  const oneMs = costUSD("edge", 1);
  const hundred = costUSD("edge", 100);
  assert.ok(oneMs > 0);
  assert.ok(costUSD("edge", 0) > 0.9 * oneMs, "at 1ms the bill is nearly all fee");
  assert.ok(hundred - oneMs > oneMs, "by 100ms the CPU dominates");
});

test("a short edge run does not pay for itself, and we know where the line is", () => {
  assert.ok(EDGE_MIN_MS > 0);
  assert.ok(marginUSD("edge", EDGE_MIN_MS - 1) < 0,
    "below the line an edge run should be a loss");
  assert.ok(marginUSD("edge", EDGE_MIN_MS + 50) > 0,
    "above the line it should pay");
});

test("our own box earns more per millisecond than any tier we rent", () => {
  const rates = { msPerCredit: 10000, msPerCreditGPU: 2000, creditUSD: 0.001 };
  assert.ok(marginUSD("home", 1000, rates) > marginUSD("edge", 1000, rates));
  assert.ok(marginUSD("home", 1000, rates) > marginUSD("rented", 1000, rates));
});

test("a GPU second is priced above what a GPU second costs", () => {
  const rates = { msPerCreditGPU: 2000, creditUSD: 0.001 };
  assert.ok(marginUSD("gpu", 60_000, rates) > 0);
  assert.ok(priceUSD("gpu", 1000, rates) > priceUSD("home", 1000, rates));
});

// --- placement ----------------------------------------------------------

test("free and local beats everything when the browser can do it", () => {
  const d = route("print(1)", {}, FLEET);
  assert.equal(d.where, "browser");
  assert.deepEqual(d.chain.slice(0, 2), ["browser", "edge"]);
});

test("a program the browser cannot take goes to the edge before our box", () => {
  const d = route("print(1)", { where: undefined }, FLEET);
  assert.ok(d.chain.indexOf("edge") < d.chain.indexOf("home"));
});

test("a program we have seen take a long time skips the edge", () => {
  const d = route("print(1)", {}, FLEET, { ms: EDGE_MAX_MS + 500, seen: 3 });
  assert.equal(d.chain.includes("edge"), false);
  assert.ok(d.chain.includes("home"));
});

test("a program with a compiled kernel goes home, where the kernel is", () => {
  const d = route("def k(n: int) -> int:\n    return n", {}, FLEET,
                  { ms: 40, native: true, seen: 9 });
  assert.equal(d.chain.includes("edge"), false);
  assert.match(d.reasons.home, /compiled kernel/);
});

test("the edge is skipped entirely when it is down", () => {
  const d = route("print(1)", {}, { ...FLEET, edge: { up: false } });
  assert.equal(d.chain.includes("edge"), false);
});

test("a GPU run never falls back to a CPU tier", () => {
  const d = route("import torch", { gpu: true }, FLEET);
  assert.deepEqual(d.chain, ["home", "rented"]);
  assert.equal(d.chain.includes("edge"), false);
  assert.equal(d.chain.includes("browser"), false);
});

test("a queue at home promotes rented capacity above idle overflow", () => {
  const busy = { ...FLEET, home: { up: true, queued: 5 }, rented: { up: true } };
  const d = route("import numpy as np", {}, busy);
  assert.ok(d.chain.indexOf("rented") >= 0);
  assert.match(d.reasons.rented, /queueing/);
});

test("an explicit choice is honoured when the tier can take it", () => {
  const d = route("print(1)", { where: "server" }, FLEET);
  assert.equal(d.where, "home");
  assert.equal(d.why, "requested");
});

test("an impossible explicit choice is obeyed, with a warning", () => {
  // `where: "browser"` is a caller saying "never bill me". Moving the run to a
  // tier that charges would be a worse answer than the failure they asked for,
  // so the choice stands and the reason is reported.
  const d = route("import torch", { where: "browser" }, FLEET);
  assert.equal(d.where, "browser");
  assert.match(d.refused, /browser/);
  assert.match(d.why, /torch/);
});

test("a program that left the browser says why it had to", () => {
  const d = route("import torch\nprint(1)", {}, FLEET);
  assert.equal(d.where, "home");
  assert.match(d.why, /torch/);
  assert.match(d.local_refused, /wasm/);
});

test("an estimate turns into a price the caller can see before running", () => {
  const rates = { msPerCredit: 10000, creditUSD: 0.001 };
  // A program only our box can take, so the price is a billed price rather
  // than the browser's zero.
  const d = route("import torch\nprint(1)", { rates }, FLEET,
                  { ms: 250, seen: 4 });
  assert.equal(d.where, "home");
  assert.equal(d.estimate_ms, 250);
  assert.ok(d.price_usd > 0);
  assert.ok(d.cost_usd >= 0);
  assert.ok(d.price_usd > d.cost_usd, "our own box must earn on a billed run");
});

test("a browser run is free to quote as well as to run", () => {
  const d = route("print(1)", { rates: { msPerCredit: 10000 } }, FLEET,
                  { ms: 12, seen: 2 });
  assert.equal(d.where, "browser");
  assert.equal(d.price_usd, 0);
  assert.equal(d.cost_usd, 0);
});

test("with no fleet and no history it still decides", () => {
  const d = route("print(1)");
  assert.ok(d.where);
  assert.ok(d.chain.length >= 1);
});

// --- failing over -------------------------------------------------------

test("a program that raised is not retried somewhere else", () => {
  assert.equal(shouldFailOver(
    { ok: false, exit_code: 1, stderr: "ValueError: boom" }), false);
});

test("a timeout is the program's own, not a tier failure", () => {
  assert.equal(shouldFailOver(
    { ok: false, timed_out: true, stderr: "time limit exceeded" }), false);
});

test("a tier that refused the work hands it on", () => {
  assert.equal(shouldFailOver({ ok: false, retry_elsewhere: true }), true);
});

test("running out of GPU memory moves the run, it does not fail it", () => {
  assert.equal(shouldFailOver({
    ok: false, exit_code: 1,
    stderr: "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 GiB",
  }), true);
});

test("no credits is an answer, not a reason to try another machine", () => {
  assert.equal(shouldFailOver(null, { status: 402 }), false);
  assert.equal(shouldFailOver(null, { status: 401 }), false);
  assert.equal(shouldFailOver(null, { status: 500 }), true);
  assert.equal(shouldFailOver(null, { status: 0, message: "network" }), true);
});

test("imports are found wherever they are written", () => {
  assert.deepEqual(imports("import a\nfrom b.c import d\n  import e").sort(),
                   ["a", "b", "e"]);
});
