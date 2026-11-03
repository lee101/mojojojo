# mojojojo

Per-millisecond Python execution as a service at mojojojo.cc. See README.md for the
product; this file is the map for working in the repo.

## Layout

| path | what |
| --- | --- |
| `main.go` `api.go` `auth.go` `billing.go` `runner.go` `config.go` `templates.go` | Go web tier, port 4341 |
| `oauth.go` | local signup/login/logout forms and session cookies |
| `scripts/visualbench.mjs` | every page at two viewports; served at `/visualbench` |
| `templates/*.html` `static/` `web/mojojojo.js` | pages and the browser runtime |
| `web/hl.js` | the Dracula syntax highlighter (pages + the editor's colour layer) |
| `agent_install.go` `templates/agent.html` | coding-agent downloads and landing page |
| `runner/runner.py` | worker HTTP server, port 4342, stdlib only |
| `runner/envs.py` `runner/autoinstall.py` | uv venvs, and what to put in them |
| `runner/sandbox.py` `runner/mojojail` | process isolation |
| `runner/pool.py` `runner/forkserver.py` | warm interpreters (fast start) |
| `runner/hostcompile.py` `runner/mojo_stub.py` | compiles happen OUT of the sandbox |
| `runner/mojoprog.py` | whole programs written in Mojo (`language: "mojo"`) |
| `wasmworker/` | the edge tier: a Cloudflare Worker running Python in wasm |
| `web/route.js` `plan.go` `edge.go` | placement: which machine, what it costs, who pays |
| `runner/cachestore.py` | disk reserve, archive tier, R2/CDN sync, upload keys |
| `runner/mojolibs.py` | the ../mojo-* ports: registry, build, opt-in aliasing |
| `runner/autoinstall.py` | imports → distributions |
| `uploads.go` | POST /v1/uploads, prefix-locked |
| `businesses.go` `businesses.json` | mojo business catalog, subdomain routing, metered proxy |
| `meters.go` `dbaas.go` | usage meters/pricing, Database API tiers and usage |
| `runner/bootstrap.py` `runner/accel.py` | in-child entrypoint and the Mojo policy |
| `scale/` | autoscaling policy, RunPod/Hetzner providers, worker bootstrap |
| `sdk/python/` | the `mojojojo` PyPI package (`run`, `@remote`, CLI) |
| `templates/research/*.html` | the research posts; `posts` in templates.go is the index |
| `docs/notebooks.md` | hosted Notebook boundary, plugin model, and reviewed AI workflow |
| `bench/corpus.py` `bench/fallback_study.py` | labelled corpus + the study that tunes the accel filter |
| `scripts/logo/make_logo.py` | mascot pipeline (OmniServe + BiRefNet + WebP) |
| `business_stream.go` | own-upstream business proxy (how): SSE streaming, done-event token metering |
| `deploy.sh` `systemd/` `nginx/` | deployment |

## Commands

```bash
go build -o mojojojo . && go test ./...          # web tier + scale policy
.venv/bin/python -m pytest runner/tests sdk/python/tests -q   # MOJOJOJO_JAIL=/nonexistent skips the jail
node --test web/mojojojo.test.mjs web/hl.test.mjs web/route.test.mjs  # browser runtime, highlighter, router
node --test wasmworker/test/edge.test.mjs        # the edge tier, under real workerd (~1min)
bash wasmworker/dev.sh                           # the edge tier locally
bash tests/e2e.sh                                # auth → key → run → credits → pages → browser
node tests/browser.mjs http://127.0.0.1:4341     # playwright pass alone (skips if absent)
bench/run.sh bench/fallback_study.py             # where the accel pipeline wastes work
bench/run.sh bench/fallback_study.py --compile --crossover --copyback  # + real builds
bash deploy.sh                                   # idempotent; installs jail + sudoers + units
journalctl -u mojojojo -u mojojojo-runner -f
```

## The compile trust boundary (do not weaken this)

The sandbox has **no compiler** and **no writable shared state**. That first
half was aspirational until 2026-07-30: the jail bound the whole `../mojosub`
checkout, so `.pixi/envs/default/bin/mojo` was sitting inside every sandbox,
runnable. It now binds the *package* (`mojosub/mojosub`) plus the Mojo runtime
libraries a compiled binary links against, and nothing else from that tree. A run gets a
private directory of symlinks into the shared cache; when mojosub wants a build,
`mojo_stub.py` writes the generated Mojo into the run's request directory and
fails, and `hostcompile.promote()` compiles it here afterwards. Promotion keeps
the learned return type and drops every verdict (`verified`, `raced`, `retired`)
— so one tenant can never tell another tenant's run that a wrong answer is fine.
`runner/tests/test_platform.py` pins all of that; if a change makes those tests
awkward, the change is wrong.

## Four places a program can run

| tier | reach | what it can do | what it costs us |
| --- | --- | --- | --- |
| browser | 0ms | Pyodide + its wheels | nothing |
| edge (`wasmworker/`) | ~25ms | stdlib Python only | $0.30/M req + $0.02/M CPU-ms |
| our box | ~180ms | everything: packages, Mojo, GPU | electricity |
| rented | ~200ms | overflow and GPUs | RunPod/Hetzner rates |

`web/route.js` decides, in the caller's browser, because only the browser knows
whether it can run the program for free. `plan.go` mirrors it for callers that
are not browsers, and supplies the two things a client cannot know: the fleet's
state, and **what this exact program cost last time anybody ran it** (the `seen`
table). That estimate is what turns placement from a guess into arithmetic — and
it is what `/v1/plan` quotes before a caller spends anything.

Two numbers, both measured, both in `route.js`: an edge run pays for its own
request fee above **4ms**, and stops being the cheap option above **200ms**.

## Two ways to get native code

- **`language: "python"`** (default) — mojosub compiles the numeric functions
  it can take and CPython runs the rest. First call interpreted, verified
  against CPython, then native.
- **`language: "mojo"`** (`runner/mojoprog.py`) — the caller writes Mojo and we
  run all of it. Built on the host, content-addressed exactly like a kernel
  (source + compiler version + host CPU), binary hardlinked into the run
  directory, executed by the same jail. The build is never billed, including
  when it fails; the run is timed from inside the jail by `sandbox._TIMED`,
  because a compiled program has no interpreter to write a trace file.
  This Mojo is 1.0: `def` not `fn`, stdlib under `std.`.

## Security invariants (all four were once broken; all four are tested)

1. rlimits are applied **inside** the jail by `bootstrap._apply_limits` —
   `sudo`/PAM discards inherited ones.
2. caller-supplied environment merges **under** ours, never over
   (`sandbox._RESERVED_ENV`).
3. the jail hides `/home`, `/root`, `/sdb-disk`, `/opt`, `/var/log` and
   everything under `/nvme0n1-disk`, binding back only the allowlist in
   `mojojail`. Adding a path there is a security decision. `--ports` may only
   *narrow* that allowlist: it names `mojo-*` directories the script would have
   bound anyway, and anything else is refused.
4. cache seeding is per-program (`hostcompile.seed(private, code)`), never the
   whole cache.

## Where the latency actually is (measured, this box)

A run that answers in 300ms once spent 2.9s doing it, and none of the
difference was the program. In order of what it cost:

- **Binding the ~175 `../mojo-*` ports cost 2.3s of every cold start** — three
  mount syscalls each, for directories almost no program imports. A run now
  asks for only the ports it imports (`sandbox.jail_command` → `--ports`); a
  pool slot still asks for `all`, once, because it builds its mounts before it
  knows whose run it will serve.
- **The warm pool drained to empty and stayed there.** Slots aged out at 30
  minutes, `acquire` retired them without starting replacements, and every run
  after that paid cold start. `_top_up` on every miss and a keeper thread fix
  it; `pool.stats()["warm"]` being `{}` on a live box is the symptom.
- **The warm path was broken and silent** — a failed fork returned exit 1 with
  no output, indistinguishable from a program that printed nothing.
  `forkserver._report` writes the traceback into the run's stderr now.
- **`accel.enabled()` started an interpreter to ask `mojo --version`**, ~35ms
  on every accelerated run, for a string the host had already put in the
  environment. `rebind_environment` installs it instead — in `mojosub`,
  `mojosub.cache` and `mojosub.toolchain`, all three, because the first two
  bind the function by value at import.
- **The eligibility filter runs before mojosub is imported.** A program with
  nothing worth compiling should not pay for the machinery that would compile
  it.
- **What is left is verification.** `verify=True` runs the first native call
  against CPython, and promotion deliberately drops the verdict, so every run
  re-verifies — one interpreted call per signature. On the homepage example
  that is ~390ms of the ~400ms that remains. Making it cheaper means a verdict
  *we* produced rather than one a sandbox reported, which is a change to the
  trust boundary, not a tuning knob.

## Facts worth not rediscovering

- **A kernel can go native part-way through its own first run.** `mojo_stub`
  queues the build and then *waits* for the host (`hostcompile.Watcher`) to
  drop the library beside the request; mojosub's compile thread is where the
  wait happens, so the program keeps running interpreted meanwhile.
  `bootstrap._release_compile_waiters` writes a `stop` marker when the user's
  code returns — past that point a library has no call left to serve and
  waiting would only make the caller sit through a compile.
- **sudo scrubs the environment.** The jail command rebuilds it with
  `/usr/bin/env -i K=V …` inside the jail; without that the program runs but has
  no trace file, no acceleration, and no timings.
- **`MODULAR_HOME` must match the resolved `mojo` binary**, or the compiler
  starts and then fails with `unable to locate module 'std'`. An inherited
  `MODULAR_HOME` from an old standalone Modular install is worse than none.
- **mojosub needs the program source** (`source=` on `jit`): user code is exec'd
  from an AST, so `inspect.getsource` has nothing to read.
- **uv's managed interpreters** default to `~/.local/share/uv`, which the sandbox
  user cannot traverse — `UV_PYTHON_INSTALL_DIR` must point somewhere world
  readable or every jailed run dies with `Permission denied` on the interpreter.
- **`RLIMIT_AS` breaks numpy** (OpenBLAS reserves enormous virtual space per
  thread); `RLIMIT_DATA` is the limit that works. `RLIMIT_NPROC` is per-uid, so
  it is only applied when the jail has already switched users.
- **Billing clock** stops before the warm phase, and subtracts compile time
  capped at half the run. Changing that changes what users are charged — it is
  covered by `billing_test.go` and it should stay covered.
- **A warm pool slot imports mojosub before any run exists**, so
  `cache.DEFAULT_ROOT` and the memoised toolchain lookup are bound to the
  *pool's* environment. `accel.rebind_environment()` re-reads them per run —
  without it every kernel looks new forever and nothing is ever native.
- **Both execution paths must share one `try/finally`.** The warm path used to
  return from inside its own `try`, which skipped cache promotion and leaked the
  run directory. Symptom: `cache.compiled` stays 0 and `runs/` fills up.
- **The pool socket must be 0666** (the host connects as the service user) and
  `pool/` and `runs/` are **0733** — traversable, not listable. Unguessable
  names are what separate one sandboxed run from another, not the mode.
- **Anonymous server runs are off** (`ALLOW_ANONYMOUS=false`) and the $1 welcome
  grant is once per account, in mojojojo's own `grants` table. Both exist so
  that free compute is per *person*, not per request.
- **The autoscaler never spends by default.** `SCALE_DRY_RUN` must be exactly
  `false` to allow a rental, and providers only exist when their credentials do.
- **The hidden-tree tmpfs is mode 0111, not 000**, where an allowlisted path
  sits underneath it: 000 blocks traversal and the interpreter disappears.
  Sources must be staged (bound aside) *before* their parent is overmounted,
  or the rebind binds the empty replacement.
- **uv picks the interpreter** unless told exactly which one. A venv whose
  `bin/python` resolves into `/home` cannot be executed inside the jail —
  `envs.base_interpreter()` pins it and `ensure()` refuses a venv that lands
  outside `VISIBLE_PREFIXES`.
- **A kernel that calls a helper could never compile.** `mojosub._module_of`
  kept only the entry function, so `total += helper(x)` was `Unsupported: call
  to helper` every time — the filter offered it (it checks helpers on purpose),
  the transpiler refused it, and the host compiled nothing useful. It now keeps
  siblings and drops only rival definitions of the entry itself.
- **`hostcompile.source_keys` must agree with `mojosub._module_of`, exactly.**
  A run writes its learned signature under mojosub's key and `seed()` looks it
  up under ours; when the two drifted apart nothing errored, nothing was ever
  seeded, and every run recompiled from scratch for ever.
- **A pool slot outlives the worker unless it is killed by name.** `sudo` opens
  a PAM session, which moves the jailed process into its own cgroup, so
  systemd's cgroup kill misses it and `proc.terminate()` only kills our `sudo`.
  Every restart leaked four slots; after a day of restarts, 251 `mojorun`
  threads sat against a per-uid `RLIMIT_NPROC` of 256 and compiles silently
  stopped starting. `pool.reap_slot` (via `mojojail --reap-slot`) runs on
  close, on startup, and on SIGTERM.
- **RLIMIT_NPROC is shared by every tenant**, because every run is the same
  uid. It bounds the box, not the run, and running out of it is invisible from
  inside a program — mojosub now falls back to CPython when it cannot start a
  compile thread rather than letting the `RuntimeError` end the run.
- **A module-level constant is folded into the emitted Mojo as its literal**
  (`mojosub.transpile.module_constants`, mirrored by `accel.module_constants`).
  `WINDOW = 64` at the top of a file used to mean `Unsupported: unbound name`
  for every kernel that read it, which was the most common rejection there was.
  Folding the literal — rather than reading the value at call time — is what
  keeps it safe: the unit's content hash moves when the constant does. A name
  the module rebinds, or that any function assigns, is not a constant.
- **The accel filter must be narrower than the transpiler, never wider.** A
  false "yes" costs ~5s of *host* compiler for a unit nobody loads, on every
  submission, for ever — and it is invisible, because the tiered design is built
  to swallow failed compiles. `bench/fallback_study.py` measures the gap against
  a labelled corpus; it currently catches 13/14 with 0 wasted transpiles.
  `compilable()` and `eligible()` are deliberately separate: only the second
  needs a loop, because a helper called from inside someone else's loop is worth
  compiling with none of its own.
  Narrower is safe but it is not free: `math.sqrt` is how a numeric kernel is
  written and the transpiler emits it, yet an `Attribute` is not an allowed node
  and `math` is not a name the body binds, so every such function was rejected
  twice over before reaching the branch written to accept it — 67x, silently
  declined, for years. `_module_call_nodes` whitelists that exact shape
  (`root.attr(...)`, root in `_MODULE_ROOTS`, attr in `_ALLOWED_CALLS`) and
  nothing else: `math.pi`, `math.gamma`, `m = math` and `np.random.rand` all
  still stop at the filter.
- **Generated Mojo identifiers are namespaced (`u_` user, `n_` lengths).** `var`
  and `fn` are legal Python names and reserved Mojo ones, and a user variable
  called `xs_len` used to silently overwrite the synthesised length of `xs` — a
  wrong answer. Do not "simplify" this back to emitting names verbatim.
- **The compile cache key includes the host CPU's feature flags.** `mojo build`
  targets the host CPU by default and the cache syncs across rented workers, so
  without it a unit built where AVX-512 exists dies on an AVX2 box.
- **JIT and whole-program builds share one bounded compiler queue.** Distinct
  source strings must not turn into an unbounded fleet of compiler processes or
  background promotion threads. Queue pressure is transient: Python stays on
  CPython and a Mojo program may retry; it is never written to the negative
  compile cache under the program's content hash.
- **A read-only buffer parameter is not copied back** (`Signature.writes`), which
  is 45-63% of a native call on a list argument. `writes=None` must keep meaning
  "may write everything", and a buffer passed to a helper stays conservative.
- **A matrix and a flat array emit the same Mojo but are not the same type.**
  Both are a bare pointer; only the shape arguments beside them differ. Variant
  keys and signature memos use `Ty.key`, not `Ty.mojo`, or a 2-D call reuses the
  1-D library and reads the wrong addresses. `Array.key` deliberately keeps its
  old spelling so memos already on disk still resolve.
- **A 2-D argument must be C-contiguous, and that is checked.** The emitted index
  is `row * cols + col`, which is the wrong address for a transposed or sliced
  array — and reading the wrong memory returns a plausible number, not an error.
- **`.shape[0]` / `.shape[1]` is the only attribute access in the subset.** It is
  there because a kernel written against numpy already says it; everything else
  behind a dot is still refused, in both the filter and the transpiler.
- **Emitted declarations go where *Python* puts them, not where the statement is.**
  Python scopes a local to its function; a Mojo `var` is scoped to its block. So
  locals are hoisted to the top of the emitted function, a reassigned parameter
  arrives as `a_x` and is copied into a mutable `u_x`, and a loop counter that
  outlives its loop is driven by a generated iterator. Ordinary Python breaks all
  three ways otherwise, and a failed compile is invisible because the run falls
  back to CPython and returns the right answer slowly. `bench/fuzz.py` in mojosub
  finds these; three of its first dozen programs did.
- **`_helpers` must compute reachability.** A unit is emitted whole or not at all,
  so including every annotated sibling made one function using `sorted` break
  every other function in the file — and reported `call to sorted` for kernels
  that had none.
- **The database is mojojojo's PostgreSQL database.** `users`, `sessions`,
  `api_keys`, `credit_ledger`, usage, grants, estimates, and edge tickets are
  created by `migrateDatabase`. Never add a read path to app.nz's SQLite file.

## One account origin

mojojojo.cc is the canonical and only identity origin. `mojojojo.app.nz` is an
HTTP 308 redirect and must never validate an app.nz cookie.

- Passwords are bcrypt hashes; unknown-email logins still run bcrypt to reduce
  account enumeration through timing.
- Login and signup forms use a short-lived host cookie as their CSRF token,
  compared with `subtle.ConstantTimeCompare`.
- Sessions and API keys store only SHA-256 hashes. Raw secrets are shown once.
- **`safeNext` refuses `//evil.com` and `/\evil.com`**, which a browser treats
  as absolute. All post-auth redirects remain on the local origin.
- Cloudflare's `X-Forwarded-Proto` controls the production cookie's Secure flag.
- The shared agent backend receives `MJJ_AGENT_TOKEN` plus
  `X-Mojojojo-User`; browser cookies and developer API keys stop at this web tier.
- Runpod/Hetzner workers use `RUNNER_TOKEN` and know no user identity.

## Money at the edge

The browser talks to the Worker directly — proxying through the control plane
would spend exactly the latency the tier exists to save. So `/v1/plan` signs a
ticket (account, budget, expiry, nonce), the Worker verifies it and signs a
receipt for the milliseconds used, and `/v1/settle` bills against the receipt.
The caller reports their own bill; the receipt is what makes the number
unforgeable.

An unsettled ticket is a free run. That is bounded (a ticket's `max_ms`, four
outstanding per account) and deliberate: billing before the work charges people
for runs that never happened.

## Related

- `../mojosub` — the execution engine (github.com/lee101/mojosub)
- `../mojo-*` — ~190 library ports; `runner/mojolibs.py` registers 106 of them.
  A port's package must be namespaced under `mojo` or it shadows the library it
  stands in for; `mojolibs.REJECTED` catches that and a test pins it.
- `../mojo-factory` — the port pipeline (184 libraries) and `bin/convert.py`,
  the function-level converter: mojosub first, then a coding agent for what it
  refuses, with the agent's output gated on compile + parity vs CPython + measured
  speedup. The agent is a generator; the gate is the judge. Never import that
  tooling from the runtime path — an agent must not be reachable from a sandbox.
- `../mojo-rl` — batched classic-control RL environments (2.5x a NumPy batch at
  4096 envs, 0.76x at 16384 — read its README before quoting a number)
- `../app-site` — app.nz; deliberately not an auth or database dependency
