/*!
 * route.js — decide which machine should run a program, and what it will cost.
 *
 * There are four places a program can run, and they are not interchangeable:
 *
 *   browser  free to the caller and free to us. No packages beyond Pyodide's,
 *            ~2s to boot the first time, and it is the caller's own CPU.
 *   edge     a nearby edge runtime. CPython in wasm, no compiler, no packages, no
 *            GPU, no threads. We pay per request and per CPU-millisecond.
 *   home     our own box. Warm pools, the Mojo compile cache (a kernel that is
 *            already compiled runs 6-40x faster than CPython), every package on
 *            PyPI, and a GPU. Marginal cost to us is electricity.
 *   rented   overflow capacity, available when the primary pool is queueing or
 *            the program needs a GPU it cannot currently provide.
 *
 * The decision is a pure function of the program, the caller's request, a
 * snapshot of the fleet, and — the part that makes it more than a guess — what
 * this exact program did last time it ran. `/v1/plan` returns that estimate,
 * so "how long will this take and what will it cost" is answered before the
 * caller spends anything.
 *
 * This module is deliberately dependency-free and side-effect-free: the same
 * function runs in the browser, in the notebook, and (mirrored) in the Go tier,
 * and a placement decision that cannot be unit-tested is a placement decision
 * nobody can reason about.
 */

// What each tier costs *us*, in USD. These are the numbers we are billed by
// somebody else; the price we charge is in the Go config and is checked
// against these by `marginFor` below.
export const COST = {
  // Edge request and CPU-millisecond costs. The request fee is the one that matters — it is the same
  // whether the program runs for one millisecond or a thousand.
  edgeRequestUSD: 0.30e-6,
  edgeMsUSD: 0.02e-6,
  // Our own box: bought, not rented. What a millisecond costs is power and
  // amortisation, and at 72 cores it is a rounding error next to a request fee.
  homeMsUSD: 0.0000000042,
  // A rented CPU box, per millisecond of one core (~$0.03/core-hour).
  rentedMsUSD: 0.0000000083,
  // Overflow GPU capacity, per millisecond.
  gpuMsUSD: 0.00000019,
};

// Latency to *reach* a tier, before the program starts. Measured medians, not
// guesses: the browser figure is Pyodide's boot amortised over a session, the
// edge figure is a PoP round trip, home is a trans-Pacific round trip plus a
// warm fork.
export const REACH_MS = {
  browser: 0,
  edge: 25,
  home: 180,
  rented: 200,
};

// Packages Pyodide ships or can fetch itself. The browser can load these; the
// edge cannot, because a Worker isolate has no time to download a wheel.
export const WASM_PACKAGES = new Set([
  "numpy", "scipy", "pandas", "matplotlib", "scikit-learn", "sklearn",
  "sympy", "networkx", "pillow", "pil", "regex", "pyyaml", "yaml",
  "beautifulsoup4", "bs4", "lxml", "statsmodels", "pytz", "python-dateutil",
  "attrs", "certifi", "charset-normalizer", "idna", "packaging", "six",
  "typing-extensions", "cloudpickle", "msgpack", "pyparsing", "cycler",
  "kiwisolver", "fonttools", "joblib", "threadpoolctl", "decorator",
]);

// Imports that mean "this needs a real machine".
export const NEEDS_SERVER = [
  "torch", "tensorflow", "jax", "cupy", "numba", "triton", "transformers",
  "diffusers", "polars", "duckdb", "pyarrow", "psycopg", "asyncpg", "requests",
  "httpx", "aiohttp", "urllib3", "socket", "subprocess", "multiprocessing",
  "ctypes", "mojosub",
];

// The edge has the standard library and nothing else, so anything imported
// that is not stdlib sends the program elsewhere. Listing the stdlib is not
// practical; listing what a small program actually reaches for is.
const EDGE_STDLIB = new Set([
  "abc", "argparse", "array", "ast", "base64", "binascii", "bisect", "calendar",
  "cmath", "collections", "colorsys", "contextlib", "copy", "csv", "dataclasses",
  "datetime", "decimal", "difflib", "enum", "fnmatch", "fractions", "functools",
  "gzip", "hashlib", "heapq", "hmac", "html", "io", "itertools", "json", "math",
  "operator", "os", "pathlib", "pickle", "pprint", "queue", "random", "re",
  "secrets", "statistics", "string", "struct", "sys", "textwrap", "time",
  "timeit", "typing", "unicodedata", "unittest", "urllib", "uuid", "zlib",
]);

const IMPORT_RE = /^\s*(?:import\s+([A-Za-z_][\w.]*)|from\s+([A-Za-z_][\w.]*)\s+import)/gm;

/** Top-level module names imported by a program. */
export function imports(code) {
  const found = new Set();
  for (const m of String(code).matchAll(IMPORT_RE)) {
    const name = (m[1] || m[2] || "").split(".")[0];
    if (name) found.add(name);
  }
  return [...found];
}

/**
 * Which tiers *can* run this at all. Capability only — no preferences, no
 * prices. Everything downstream is an ordering problem over this set.
 */
export function capable(code, opts = {}) {
  const used = imports(code);
  const language = opts.language || "python";
  const packages = (opts.packages || []).map((p) =>
    String(p).split(/[=<>~!\[]/)[0].trim().toLowerCase());

  const out = { browser: true, edge: true, home: true, rented: true };
  const why = {};

  if (language !== "python") {
    out.browser = out.edge = false;
    why.browser = why.edge = `${language} is compiled on our machines`;
  }
  if (opts.gpu) {
    out.browser = out.edge = false;
    why.browser = why.edge = "a GPU was asked for";
  }
  if (opts.native) {
    out.browser = out.edge = false;
    why.browser = why.edge = "native compilation was asked for";
  }
  if (typeof WebAssembly === "undefined") {
    out.browser = false;
    why.browser = "no WebAssembly in this environment";
  }

  const blocked = used.filter((m) => NEEDS_SERVER.includes(m));
  if (blocked.length) {
    out.browser = out.edge = false;
    why.browser = why.edge = `imports ${blocked.join(", ")} — no wasm build`;
  }

  const missingInBrowser = packages.filter((p) => !WASM_PACKAGES.has(p));
  if (missingInBrowser.length) {
    out.browser = false;
    why.browser = `packages ${missingInBrowser.join(", ")} are not in the wasm registry`;
  }

  // The edge ships the standard library and nothing else. A wheel download in
  // a Worker isolate costs more than the program ever will.
  const nonStdlib = used.filter((m) => !EDGE_STDLIB.has(m));
  if (nonStdlib.length || packages.length) {
    out.edge = false;
    why.edge = `${(nonStdlib.length ? nonStdlib : packages).join(", ")} is not in the edge runtime`;
  }
  return { can: out, why };
}

/** What a run of `ms` milliseconds costs *us* on a tier, in USD. */
export function costUSD(where, ms) {
  switch (where) {
    case "browser": return 0;
    case "edge": return COST.edgeRequestUSD + ms * COST.edgeMsUSD;
    case "home": return ms * COST.homeMsUSD;
    case "rented": return ms * COST.rentedMsUSD;
    case "gpu": return ms * COST.gpuMsUSD;
    default: return 0;
  }
}

/**
 * What we charge for a run of `ms` on a tier, in USD, given the credit rates
 * the service is configured with.
 */
export function priceUSD(where, ms, rates = {}) {
  const msPerCredit = where === "gpu"
    ? (rates.msPerCreditGPU || 2000)
    : (rates.msPerCredit || 10000);
  const creditUSD = rates.creditUSD || 0.001;
  if (where === "browser") return 0;
  return (ms / msPerCredit) * creditUSD;
}

/**
 * Margin on a run: what we charge less what it costs us.
 *
 * Reported rather than asserted, because it is genuinely negative in one case
 * and pretending otherwise would be the expensive kind of wrong: at the edge,
 * The edge per-*request* fee is fixed whatever the program does, so a
 * two-millisecond program costs us more than it earns. That is a fine trade for
 * latency on a free-tier call and a bad one to build a business on, which is
 * why `EDGE_MIN_MS` exists.
 */
export function marginUSD(where, ms, rates = {}) {
  return priceUSD(where, ms, rates) - costUSD(where, ms);
}

// Below this, an edge run cannot pay for its own request fee, so the tier is
// only worth using for the latency — which is exactly what the free tier and
// the browser fallback are for.
export const EDGE_MIN_MS = Math.ceil(
  COST.edgeRequestUSD / ((0.001 / 10000) - COST.edgeMsUSD));

// Above this, the edge stops being the cheap option: a long program burns
// CPU-milliseconds we are billed for, on hardware we do not own, while the box
// we already paid for sits idle. It is also where the compile cache starts to
// win by more than the round trip costs.
export const EDGE_MAX_MS = Number(200);

/**
 * Decide where a program should run, with the fallback chain behind it.
 *
 * `fleet` is a snapshot from /v1/plan: {edge: {up}, home: {up, queued}, gpu:
 * {up, queued, oom}}. `estimate` is what this program did last time:
 * {ms, native, seen}. Both are optional — with neither, this degrades to the
 * same static decision the library made before there was a fleet at all.
 *
 * Returns {where, why, chain, estimate_ms, cost_usd, price_usd}. `chain` is
 * every tier that could take it, in order, so a caller that fails over does
 * not have to re-decide.
 */
export function route(code, opts = {}, fleet = {}, estimate = null) {
  const { can, why } = capable(code, opts);
  const forced = opts.where;
  const estMs = estimate && estimate.ms > 0 ? estimate.ms : null;
  const native = Boolean(estimate && estimate.native);

  const up = (tier, dflt = true) =>
    fleet[tier] === undefined ? dflt : Boolean(fleet[tier].up);
  const queued = (tier) => (fleet[tier] && fleet[tier].queued) || 0;

  const candidates = [];
  const add = (where, reason, order) =>
    candidates.push({ where, why: reason, order });

  if (opts.gpu) {
    // A GPU run has one home and one fallback, and the fallback is a rented
    // GPU rather than a CPU: silently running a CUDA program on a CPU is not
    // a fallback, it is a different answer.
    if (up("gpu")) add("home", "GPU on our box", 0);
    add("rented", "rented GPU capacity", 1);
    const chain = candidates.sort((a, b) => a.order - b.order);
    return decision(chain, estMs, opts, "gpu");
  }

  if (forced === "browser" || forced === "edge" || forced === "home" ||
      forced === "server") {
    const where = forced === "server" ? "home" : forced;
    const out = decision([{ where, why: "requested" }], estMs, opts, where);
    if (can[where] === false) {
      // Obeyed anyway, and said so. `where: "browser"` is how a caller says
      // "never bill me for this", so quietly moving the run somewhere that
      // charges would be a worse answer than the failure they asked for.
      out.refused = `${where}: ${why[where] || "this tier cannot run it"}`;
      out.why = `requested, though ${why[where] || "this tier cannot run it"}`;
    }
    return out;
  }

  // 1. The caller's own machine, when it can: free for them and for us.
  if (can.browser) {
    add("browser", "runs locally, free", 0);
  }

  // 2. The edge, for the small and the far-away. A program we have seen take
  //    longer than EDGE_MAX_MS is not a small program, and one that goes native
  //    on our box is 6-40x faster there than any wasm CPython can be.
  if (can.edge && up("edge", false)) {
    if (native) {
      why.edge = "the compiled kernel for this program lives on our box";
    } else if (estMs !== null && estMs > EDGE_MAX_MS) {
      why.edge = `about ${Math.round(estMs)}ms of work — our box is cheaper above ${EDGE_MAX_MS}ms`;
    } else {
      add("edge", estMs === null
        ? "small enough for the nearest PoP"
        : `about ${Math.round(estMs)}ms, nearest PoP is ${REACH_MS.home - REACH_MS.edge}ms closer`, 1);
    }
  }

  // 3. Our box. Always able, and the only tier that has the compile cache.
  if (can.home) {
    add("home", native
      ? "the compiled kernel for this program is already here"
      : (queued("home") > 0 ? "our box, once a slot frees" : "our box"), 2);
  }

  // 4. Rented capacity, but only as a fallback for a queue.
  if (queued("home") > 0 && up("rented", false)) {
    add("rented", "our box is queueing", 3);
  } else {
    add("rented", "overflow capacity", 4);
  }

  const chain = candidates.sort((a, b) => a.order - b.order);
  const out = decision(chain, estMs, opts, chain[0] ? chain[0].where : "home");
  // Why this is *not* running on the caller's own machine is the thing they
  // most want to know, because it is the difference between free and billed.
  if (out.where !== "browser" && why.browser) {
    out.why = why.browser;
    out.local_refused = why.browser;
  }
  return out;
}

function decision(chain, estMs, opts, where) {
  const first = chain[0] || { where, why: "nowhere else to go" };
  const ms = estMs === null ? null : estMs;
  const rates = opts.rates || {};
  const tier = opts.gpu ? "gpu" : first.where;
  return {
    where: first.where,
    why: first.why,
    chain: chain.map((c) => c.where),
    reasons: Object.fromEntries(chain.map((c) => [c.where, c.why])),
    estimate_ms: ms,
    // Both sides of the trade, so a caller can see what a placement is for.
    cost_usd: ms === null ? null : costUSD(tier, ms),
    price_usd: ms === null ? null : priceUSD(tier, ms, rates),
    reach_ms: REACH_MS[first.where] ?? null,
  };
}

/**
 * Whether a failed attempt on one tier should be retried on the next.
 *
 * The rule is that we eat our own failures and bill the caller's. A tier that
 * refused the work, fell over, or ran out of memory is our problem and the run
 * moves; a program that raised, exited non-zero or timed out has been run
 * correctly and moving it would just run it twice.
 */
export function shouldFailOver(result, error) {
  if (error) {
    // 402 and 401 are answers, not failures — retrying elsewhere would spend
    // the caller's money to get the same answer.
    if (error.status === 401 || error.status === 402 || error.status === 400) {
      return false;
    }
    return true;
  }
  if (!result) return true;
  if (result.retry_elsewhere) return true;
  if (result.ok) return false;
  const text = `${result.stderr || ""} ${result.error || ""}`;
  // Out of memory on a device is the canonical "right program, wrong machine".
  return /out of memory|CUDA error|cudaError|failed to allocate|no CUDA-capable|MemoryError/i
    .test(text);
}
