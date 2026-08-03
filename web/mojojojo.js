/*!
 * mojojojo.js — run Python from the browser, on the cheapest machine that can
 * actually run it.
 *
 *   import { run } from "https://mojojojo.cc/mojojojo.js"
 *   const r = await run("print(sum(range(10)))")
 *   r.stdout   // "45\n"
 *   r.where    // "browser"  — cost you nothing
 *
 * The rule is simple: try the caller's own machine first (Pyodide, CPython
 * compiled to WebAssembly, free and private), and go to mojojojo.cc only
 * when the browser genuinely cannot do the job:
 *
 *   - the code asks for a GPU
 *   - it imports a package with no wasm wheel
 *   - it wants the Mojo-compiled native path, which wasm has no equivalent for
 *   - Pyodide is unavailable or the run blew the browser's limits
 *
 * Everything about that decision is inspectable: `r.where` says which machine
 * ran it and `r.why` says why, so nobody is surprised by a bill.
 *
 * MIT licensed. No dependencies, no build step, no telemetry.
 */

import { route, shouldFailOver } from "./route.js";

const DEFAULT_ENDPOINT = "https://mojojojo.cc";
const PYODIDE_CDN = "https://cdn.jsdelivr.net/pyodide/v0.28.3/full/pyodide.mjs";

// Packages Pyodide ships or can install itself. Anything outside this needs a
// real machine. The list is deliberately conservative: being wrong in this
// direction costs a retry, being wrong the other way costs a broken run.
const WASM_PACKAGES = new Set([
  "numpy", "scipy", "pandas", "matplotlib", "scikit-learn", "sklearn",
  "sympy", "networkx", "pillow", "pil", "regex", "pyyaml", "yaml",
  "beautifulsoup4", "bs4", "lxml", "statsmodels", "pytz", "python-dateutil",
  "attrs", "certifi", "charset-normalizer", "idna", "packaging", "six",
  "typing-extensions", "cloudpickle", "msgpack", "pyparsing", "cycler",
  "kiwisolver", "fonttools", "joblib", "threadpoolctl", "decorator",
]);

// Imports that mean "this needs a real machine", checked before we bother
// booting a 10MB runtime.
const NEEDS_SERVER = [
  "torch", "tensorflow", "jax", "cupy", "numba", "triton", "transformers",
  "diffusers", "polars", "duckdb", "pyarrow", "psycopg", "asyncpg", "requests",
  "httpx", "aiohttp", "urllib3", "socket", "subprocess", "multiprocessing",
  "ctypes", "mojosub",
];

const IMPORT_RE = /^\s*(?:import\s+([A-Za-z_][\w.]*)|from\s+([A-Za-z_][\w.]*)\s+import)/gm;

/** Top-level module names imported by a program. */
export function imports(code) {
  const found = new Set();
  for (const m of code.matchAll(IMPORT_RE)) {
    const name = (m[1] || m[2] || "").split(".")[0];
    if (name) found.add(name);
  }
  return [...found];
}

/**
 * Decide where a program should run, without running it.
 * Returns {where: "browser"|"server", why: string}.
 */
export function plan(code, opts = {}, fleet = undefined, estimate = undefined) {
  // Delegates to route.js so there is exactly one placement decision in the
  // codebase. Callers of this library have always got {where, why} back, and
  // still do; `where` collapses to "browser" or "server" because that is the
  // only distinction a caller without a fleet snapshot can act on.
  const decision = route(code, opts, fleet, estimate);
  const where = decision.where === "browser" ? "browser" : "server";
  return { where, why: decision.why, chain: decision.chain,
           estimate_ms: decision.estimate_ms ?? null };
}

let pyodidePromise = null;

async function bootPyodide(indexURL) {
  if (!pyodidePromise) {
    pyodidePromise = (async () => {
      const { loadPyodide } = await import(indexURL || PYODIDE_CDN);
      return loadPyodide();
    })().catch((err) => {
      pyodidePromise = null; // a failed boot must not poison later attempts
      throw err;
    });
  }
  return pyodidePromise;
}

/** Run in this browser. Throws if the runtime cannot be brought up. */
export async function runLocal(code, opts = {}) {
  const started = performance.now();
  const py = await bootPyodide(opts.pyodideURL);
  if (opts.language && opts.language !== "python") {
    throw new Error(`mojojojo: the browser runtime only runs Python, not ${opts.language}`);
  }
  const packages = imports(code).filter((m) => WASM_PACKAGES.has(m.toLowerCase()));
  if (packages.length) await py.loadPackage(packages);

  let stdout = "";
  let stderr = "";
  py.setStdout({ batched: (s) => { stdout += s + "\n"; } });
  py.setStderr({ batched: (s) => { stderr += s + "\n"; } });

  let ok = true;
  try {
    await py.runPythonAsync(code);
  } catch (err) {
    ok = false;
    stderr += String(err && err.message ? err.message : err);
  }
  const wall = performance.now() - started;
  return {
    ok, stdout, stderr,
    exit_code: ok ? 0 : 1,
    wall_ms: Math.round(wall * 1000) / 1000,
    billable_ms: 0,
    charge: { billed_ms: 0, credits: 0 },
    where: "browser",
    why: "ran locally",
    accel: { native_calls: 0, compiled: 0 },
  };
}

/**
 * Ask the service where this program should run and what it will cost.
 *
 * Two things only the server knows: what the fleet looks like right now, and
 * what this exact program did last time anybody ran it. Both are optional —
 * every failure here falls back to deciding without them, because a planning
 * request that goes wrong must not stop a run from happening.
 */
export async function fetchPlan(code, opts = {}) {
  const endpoint = (opts.endpoint || DEFAULT_ENDPOINT).replace(/\/$/, "");
  const headers = { "Content-Type": "application/json" };
  if (opts.apiKey) headers["Authorization"] = "Bearer " + opts.apiKey;
  try {
    const resp = await fetch(endpoint + "/v1/plan", {
      method: "POST",
      headers,
      credentials: opts.apiKey ? "omit" : "include",
      body: JSON.stringify({
        code, gpu: !!opts.gpu, packages: opts.packages || [],
        language: opts.language || "python",
      }),
      signal: AbortSignal.timeout(opts.planTimeoutMs || 2500),
    });
    if (!resp.ok) return null;
    return await resp.json();
  } catch {
    return null; // planning is an optimisation, never a requirement
  }
}

/**
 * Run at the nearest Cloudflare PoP, on a ticket the control plane signed.
 *
 * This is the only tier the browser talks to directly, and the reason is the
 * whole point of it: going through the control plane to reach a Worker three
 * hops away would cost more latency than it saves. The run comes back with a
 * receipt, which is handed to /v1/settle to be billed. A receipt that never
 * arrives is a run we do not charge for.
 */
export async function runAtEdge(code, opts, edge) {
  const started = performance.now();
  const resp = await fetch(edge.url.replace(/\/$/, "") + "/run", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Mojojojo-Ticket": edge.ticket,
    },
    body: JSON.stringify({
      code,
      stdin: opts.stdin || "",
      args: opts.args || [],
      timeout_ms: opts.timeoutMs || 0,
    }),
  });
  const data = await resp.json();
  if (!resp.ok) {
    const err = new Error(data.error || `edge: HTTP ${resp.status}`);
    err.status = resp.status;
    err.body = data;
    throw err;
  }
  data.where = "edge";
  data.why = opts.why || "nearest point of presence";
  data.round_trip_ms = Math.round((performance.now() - started) * 1000) / 1000;
  // Settling is what turns the run into a charge. It is deliberately not
  // awaited-or-failed: the answer is already in the caller's hands, and a
  // settlement we could not deliver is our loss, not their error.
  if (data.receipt) {
    settle(data, opts).catch(() => {});
  }
  return data;
}

async function settle(result, opts) {
  const endpoint = (opts.endpoint || DEFAULT_ENDPOINT).replace(/\/$/, "");
  const headers = { "Content-Type": "application/json" };
  if (opts.apiKey) headers["Authorization"] = "Bearer " + opts.apiKey;
  const resp = await fetch(endpoint + "/v1/settle", {
    method: "POST",
    headers,
    credentials: opts.apiKey ? "omit" : "include",
    body: JSON.stringify({
      ticket: opts._ticket,
      ms: result.billable_ms,
      receipt: result.receipt,
      source: opts.source || "js:edge",
    }),
  });
  if (resp.ok) {
    const body = await resp.json();
    if (body.charge) result.charge = body.charge;
  }
}

/** Run on mojojojo.cc. Billed per millisecond of your program's wall clock. */
export async function runRemote(code, opts = {}) {
  const endpoint = (opts.endpoint || DEFAULT_ENDPOINT).replace(/\/$/, "");
  const headers = { "Content-Type": "application/json" };
  if (opts.apiKey) headers["Authorization"] = "Bearer " + opts.apiKey;

  const resp = await fetch(endpoint + "/v1/run", {
    method: "POST",
    headers,
    // Cookies carry mojojojo auth when called on-site; an API
    // key covers everywhere else.
    credentials: opts.apiKey ? "omit" : "include",
    body: JSON.stringify({
      code,
      stdin: opts.stdin || "",
      args: opts.args || [],
      packages: opts.packages || [],
      timeout_ms: opts.timeoutMs || 0,
      language: opts.language || "python",
      gpu: !!opts.gpu,
      source: opts.source || "js",
    }),
  });
  const data = await resp.json();
  if (!resp.ok) {
    const err = new Error(data.error || `mojojojo: HTTP ${resp.status}`);
    err.status = resp.status;
    err.body = data;
    throw err;
  }
  return { ...data, where: "server", why: opts.why || "ran on mojojojo" };
}

async function vectorRequest(method, path, payload, opts = {}) {
  const endpoint = (opts.endpoint || DEFAULT_ENDPOINT).replace(/\/$/, "");
  const headers = { "Content-Type": "application/json" };
  if (opts.apiKey) headers.Authorization = "Bearer " + opts.apiKey;
  const resp = await fetch(endpoint + path, {
    method, headers, credentials: opts.apiKey ? "omit" : "include",
    ...(payload === undefined ? {} : { body: JSON.stringify(payload) }),
  });
  const data = await resp.json();
  if (!resp.ok) {
    const err = new Error(data.error || `mojojojo vector: HTTP ${resp.status}`);
    err.status = resp.status;
    err.body = data;
    throw err;
  }
  return data;
}

/** Create a managed folded-embedding retrieval project. */
export async function createVectorProject(name, opts = {}) {
  const data = await vectorRequest("POST", "/v1/vector/projects", {
    name, slug: opts.slug || "", tier: opts.tier || "exact",
  }, opts);
  return data.project;
}

/** Upsert up to 256 {id, text, metadata} documents. */
export function vectorUpsert(projectId, documents, opts = {}) {
  return vectorRequest("POST", `/v1/vector/projects/${encodeURIComponent(projectId)}/upsert`,
    { documents }, opts);
}

/** Retrieve ranked documents and the measured backend latency. */
export function vectorSearch(projectId, query, opts = {}) {
  return vectorRequest("POST", `/v1/vector/projects/${encodeURIComponent(projectId)}/search`,
    { query, limit: opts.limit || 10 }, opts);
}

/**
 * Run `code` wherever it belongs.
 *
 * opts: {packages, stdin, args, timeoutMs, gpu, native, apiKey, endpoint,
 *        language: "python"|"mojo", where: "auto"|"browser"|"server", onPlan}
 */
export async function run(code, opts = {}) {
  // One planning request, best effort: it buys the fleet's state, what this
  // program cost last time, and — when the server decides the edge is the
  // right place — a signed ticket to go there directly.
  const planned = opts.plan === false ? null : await fetchPlan(code, opts);
  const fleet = planned ? planned.fleet : undefined;
  const estimate = planned ? planned.estimate : undefined;
  const decision = route(code, opts, fleet, estimate);
  if (opts.onPlan) opts.onPlan({ ...decision, estimate, fleet });

  const edge = planned && planned.edge && planned.edge.ticket ? planned.edge : null;
  const attempts = [];
  for (const where of decision.chain) {
    if (where === "browser") attempts.push(["browser", () => runLocal(code, opts)]);
    else if (where === "edge" && edge) {
      attempts.push(["edge", () => runAtEdge(code, { ...opts, _ticket: edge.ticket,
        why: decision.reasons.edge }, edge)]);
    } else if (!attempts.some(([w]) => w === "server")) {
      // home, rented and a ticketless edge are all "ask the service": it owns
      // the choice between its own box, the edge and rented capacity, and it
      // is the only participant that can bill for them.
      attempts.push(["server", () => runRemote(code,
        { ...opts, why: decision.reasons[where] || decision.why })]);
    }
  }
  if (!attempts.length) {
    attempts.push(["server", () => runRemote(code, { ...opts, why: decision.why })]);
  }

  let lastError = null;
  for (let i = 0; i < attempts.length; i += 1) {
    const [where, attempt] = attempts[i];
    const isLast = i === attempts.length - 1;
    try {
      const result = await attempt();
      if (!shouldFailOver(result, null) || isLast) {
        result.planned = decision.where;
        return result;
      }
      // The tier that ran it said "not me" — a GPU it does not have, a package
      // it cannot install, memory it could not get. Somebody else's turn.
      lastError = new Error(result.error || "tier declined the work");
    } catch (err) {
      lastError = err;
      if (!shouldFailOver(null, err) || isLast) throw err;
    }
    if (opts.onFailover) opts.onFailover({ from: where, error: lastError });
  }
  throw lastError || new Error("mojojojo: nowhere to run this");
}

export const mojojojo = { run, runLocal, runRemote, runAtEdge, plan, fetchPlan,
  imports, route, createVectorProject, vectorUpsert, vectorSearch };
export default mojojojo;

// Also usable from a plain <script> tag, where imports are not available.
if (typeof window !== "undefined") window.mojojojo = mojojojo;
