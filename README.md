# mojojojo

**Per-millisecond Python execution as a service.** Send code, get output, pay
for the milliseconds it ran. Numeric loops are transpiled to Mojo by
[mojosub](https://github.com/lee101/mojosub), compiled once, cached by content
hash, and dispatched natively from then on. Everything else is real CPython
3.13 with your packages installed by uv from a warm cache.

Live at **https://mojojojo.cc** with its own accounts, sessions, API keys,
credits, and PostgreSQL database.

It also exposes managed vector retrieval at **[/vector](https://mojojojo.cc/vector)**:
a folded static 512-dimensional embedding model, portable int8 indexes, Mojo exact
search, durable tenant-owned documents, and capability-gated GPU placement.

The open-source coding agent lives at **[/agent](https://mojojojo.cc/agent)**;
its complete source, tests, docs, and release automation are public at
**[lee101/mojojojo-agent](https://github.com/lee101/mojojojo-agent)** under the
MIT licence.
The hosted Mojo Notebook product and its plugin/AI boundary are specified in
[`docs/notebooks.md`](docs/notebooks.md).
That page links the browser agent, checksummed Linux/macOS/Windows binaries,
and the stable one-command installers at `/install.sh` and `/install.ps1`.

```bash
curl -s https://mojojojo.cc/v1/run \
  -H "Authorization: Bearer $MOJOJOJO_KEY" \
  -H "Content-Type: application/json" \
  -d '{"code": "print(sum(i*i for i in range(1000)))"}'
```

```json
{"ok": true, "stdout": "332833500\n", "wall_ms": 219.4, "billable_ms": 87.2,
 "charge": {"billed_ms": 87.2, "credits": 0, "owed_ms": 87.2, "balance": 4820}}
```

87ms does not cost a whole credit, so it does not round up to one. Milliseconds
accrue against your balance and a credit ($0.001) moves through the mojojojo
ledger each time they cross the price. Nothing is lost to rounding in either
direction, and the ledger line reads `exec:cpu:87ms`.

## What is billed

| | |
| --- | --- |
| **First top-up** | **+1000 bonus credits ($1)**, once per account |
| Your browser (`mojojojo.js`) | **free, always** — it never touches us |
| CPU | 1 credit / 10,000 ms ≈ **$0.36 per core-hour** |
| GPU (RTX 5090, time-sliced) | 1 credit / 2,000 ms ≈ **$1.80 per GPU-hour** |
| Queue time, env builds, cold start, our compiler | **free** |

`billable_ms` is your program's own wall clock, minus any time our compiler
spent competing with it (capped at half the run, so a compiling run is
discounted rather than given away).

Server-side runs need a mojojojo account with credits you have added. The
browser runtime is unlimited without one. Free compute behind an anonymous
endpoint, or behind a free signup, is free compute per *request*, which is a
botnet with good documentation. The $1 bonus is once per account, ever, and
only unlocks after a payment.

## Imports install themselves

```python
run("""
    import pandas as pd
    print(pd.DataFrame({"a": [1, 2, 3]}).sum().to_dict())
""")
# {"env": {"packages": ["numpy", "pandas"], "auto": ["pandas"], "build_ms": 1440}}
```

No `packages` field, no image to build. The program's imports are parsed,
stdlib and already-present modules are skipped, module names that differ from
their distribution are mapped (`cv2` → opencv, `PIL` → pillow, `sklearn` →
scikit-learn), and uv builds a content-addressed environment that every later
run with the same set reuses. A guess that will not install falls back to
exactly what you asked for rather than failing the run. Builds are never billed.

## Cold start

Runs land on a pre-warmed interpreter that already has the jail, the
unprivileged uid and numpy in place; each run is a `fork()` of it.

| | end to end |
| --- | ---: |
| warm slot (`warm_start: true`) | **~60 ms** |
| cold path (`sudo` + `unshare` + `setpriv` + interpreter + numpy) | ~220 ms |

Same isolation either way — the namespace and the uid switch happen when the
slot is created, not when your code runs. If a slot is missing, busy or
unhealthy, the cold path runs instead, so this can only ever make a run faster.

## Ported Mojo libraries

Sitting next to this repo are ~60 ports of common Python libraries to Mojo
(`../mojo-pillow`, `../mojo-opencv-python`, `../mojo-sklearn`, …). mojojojo
compiles *your* loops; these cover the case where the hot code is inside a
library. A run gets the ports for whatever it imports, already built:

```python
run("""
    import mojopillow           # always available for what you import
    ...
""")

run(code, mojo_libs=True)       # opt in to substitution: `import PIL` → mojopillow
```

Nothing is substituted unless you ask. A port covers a subset of its original's
API, so aliasing `PIL` behind a caller's back turns a speedup into an
AttributeError nobody can explain — `mojo_libs` is how you say you want it, and
only ports marked drop-in are ever aliased. A port whose kernels are not built
yet is not offered at all (the sandbox has no compiler); the host builds it in
the background and the next run gets it.

Imports nobody has ported are appended to a wishlist, which is the queue
`../mojo-factory` reads to decide what to port next — real usage, in the order
it was hit.

## Storage and deployment

Compute lifecycle is explicit. Functions use
`"compute":{"mode":"scale-to-zero"}` (the default): each HTTP request is
isolated and billed only for measured execution. The persistent-service
contract is documented in [docs/persistent-services.md](docs/persistent-services.md)
for WebSockets, warm models, and long-running Python or Mojo loops. Persistent
manifests currently fail with a limited-preview error rather than silently
deploying with the wrong lifecycle.

Compiled kernels live in three tiers, and each exists for a different reason:

| tier | what for |
| --- | --- |
| NVMe (`/nvme0n1-disk/data/mojojojo/mojosub-cache`) | the working set; a hit is a `stat` |
| archive (`/sdb-disk/mojojojo/cache`) | eviction target — a copy back beats a 4.5s recompile |
| object storage | durable sites, uploads, and cache artifacts |

Eviction is driven by **free space**, not entry count: this box's NVMe is 81%
full and the archive disk is at 97%, and a cache that grows until a disk is full
does not degrade, it takes the service down. `MOJOJOJO_DISK_RESERVE_GB` (20 by
default) is the floor, checked before every write.

**Uploads** are scoped to `uploads/<user>/`. `POST /v1/uploads/sign` creates a
15-minute upload URL; `POST /v1/uploads` remains the simple one-request form. Both
enforce the account quota and 64MB per-file cap. The first 1GB is included,
then storage is $0.05/GB-month with delivery included.

**Deployments** are immutable ZIP bundles. Create a project with
`POST /v1/projects`, then send a bundle containing `mojojojo.json` and its
entrypoint to `POST /v1/deployments`. Archive paths, expanded size, ownership,
and the entrypoint are validated before upload; the active deployment changes
only after every file and metadata row succeeds. See `examples/` for QR,
cron, phone-number, perceptual-image, ONNX, fractal, and multi-file Mojo apps.

### Hosted apps and tools

`/studio` is the browser IDE for an app project: a text workspace, sandboxed
live preview, Python/Mojo function tester, agent, publish control, and showcase
submission in one place. The same workflow is available without the UI:

```sh
mojojojo project "JSON to CSV" --runtime app --category tool
mojojojo publish PROJECT_ID examples/json-to-csv
mojojojo submit PROJECT_ID
```

An app bundle declares functions in `mojojojo.json`:

```json
{
  "runtime": "app",
  "entrypoint": "index.html",
  "functions": {
    "convert": {
      "language": "python",
      "entrypoint": "functions/convert.py",
      "timeout_ms": 3000
    }
  }
}
```

Python entrypoints export `handler(request)` and may return a value or a
`{status, headers, body, is_base64}` envelope. Mojo entrypoints read the JSON
request envelope on stdin and write plain text or that JSON envelope to stdout.
Published pages have a stable `/p/PROJECT_ID/` URL and call functions at
`./api/NAME`. User HTML runs with an opaque sandboxed origin. Function calls are
charged to the project owner, capped at 1,000 calls per project per UTC day by
default, limited to 1 MiB requests and ten seconds, and never receive account
cookies or secrets. Approved projects appear in `/apps` or `/tools`.

### One-click example apps

The [example gallery](examples/README.md) includes four GitHub-ready tools that
open directly in Studio: [QR Forge](https://mojojojo.cc/studio?example=qr-forge&kind=tool),
[Cron Lens](https://mojojojo.cc/studio?example=cron-lens&kind=tool),
[Phone Passport](https://mojojojo.cc/studio?example=phone-passport&kind=tool), and
[Photo Twin](https://mojojojo.cc/studio?example=photo-twin&kind=tool). Their
backends prefer the matching adjacent Mojo ports and retain upstream-Python
fallbacks so a cold worker still answers the first request.

Whole-program Mojo accepts either the original `code` string or a project:

```json
{"language":"mojo", "entrypoint":"main.mojo", "files":{
  "main.mojo":"from helper import answer\n...",
  "helper.mojo":"def answer() -> Int: return 42"
}}
```

## Managed vector search

```bash
curl -H "Authorization: Bearer $MOJOJOJO_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Docs","tier":"exact"}' \
  https://mojojojo.cc/v1/vector/projects

curl -H "Authorization: Bearer $MOJOJOJO_KEY" -H 'Content-Type: application/json' \
  -d '{"documents":[{"id":"guide","text":"Mojo compiles fast kernels."}]}' \
  https://mojojojo.cc/v1/vector/projects/vec_PROJECT/upsert
```

Vector projects keep durable source documents and offer exact or performance
retrieval behind the same API. Index placement and recovery are managed automatically.

## Managed JSON data

`/data` is a tenant-isolated document database over the same Postgres control
plane. It intentionally creates no per-customer process: the shared tier is ten
databases per account, 10,000 records and 100 MiB per database, with 64 KiB JSON
values. An optional vector-project link mirrors each record's `search_text` into
mojo-embed while keeping Postgres authoritative and the worker index rebuildable.

```python
database = client.create_database("Customers", vector_project_id="vec_PROJECT")
client.data_upsert(database["id"], "customer:123", {"name": "Ada"},
                   search_text="Ada builds services with Mojo")
```

This is the low-idle-cost starting point. Dedicated Postgres is a later tier for
workloads that actually need SQL connections, extensions, or isolated compute;
it is not kept alive for empty accounts.

## SDKs

```bash
pip install mojojojo          # Python: run(), @remote, and a CLI
npm/import https://mojojojo.cc/mojojojo.js   # browser-first
mojojojo run script.py        # Python SDK CLI
mojojojo precompile script.py # representative warm-up stage for deploy/CI
```

```python
from mojojojo import Client, run, remote

client = Client()
r = run("print(6*7)")
r.charge.usd          # 4.1e-06

prepared = client.precompile(open("backtest.py").read())
prepared.ready        # compiled artifacts are ready for the next run

@remote(packages=["numpy"])
def sharpe(values, window):
    import numpy as np
    a = np.array(values[-window:])
    return float(a.mean() / a.std())

sharpe(prices, 64)        # runs there, returns a float
sharpe.local(prices, 64)  # runs here
```

Arguments and return values are JSON; closures and module globals are not
shipped, and `@remote` says so rather than silently sending half a program.

## Mojo library ports

~190 ports of common Python libraries sit next to this repo, and
`runner/mojolibs.py` registers 106 of them — the whole ML stack included
(`shap`, `optuna`, `polars`, `duckdb`, `umap`, `hdbscan`, `river`, `tslearn`,
`statsforecast`, `prophet`, `xgboost`, `lightgbm`, …). An import with a port
that is not built yet triggers the build on the host and the *next* run gets it;
nothing is ever substituted for your import unless you ask with `mojo_libs: true`.

[`../mojo-rl`](../mojo-rl) is new: batched CartPole / MountainCar / Pendulum with
a Gymnasium-shaped API, 2.5x a NumPy batch at 4096 environments and exact parity
with Gymnasium's physics to 1e-12 a step.

## Research

Long-form notes on how this works and what it cost to find out, at
**[mojojojo.cc/research](https://mojojojo.cc/research)**:

- [There is no compiler in the sandbox](https://mojojojo.cc/research/no-compiler-in-the-sandbox)
  — sharing a compile cache between tenants without sharing a process, and the
  four isolation bugs we shipped on the way.
- [How early do we know it has to be CPython?](https://mojojojo.cc/research/how-early-do-we-know)
  — measuring the accel pipeline against a labelled corpus. 64% → 92% caught for
  free, 0 wasted transpiles.
- [The copy nobody needed](https://mojojojo.cc/research/the-copy-nobody-needed)
  — 45-63% off native calls on list arguments, plus three name-collision bugs
  found by reading compiler stderr. One kernel went from never compiling to 423x.

mojosub's own lab notebook is [docs/research.md](../mojosub/docs/research.md).

## Credits and the account

One mojojojo account, one local balance, spent by the millisecond here. Server runs
spend credits you have added; the first top-up comes with 1000 bonus credits ($1), once, ever.

- **[/credits](https://mojojojo.cc/credits)** — balance and what it buys in
  core-hours, the milliseconds accruing towards the next credit, every ledger
  line behind every charge, and account management.
- **`GET /v1/credits`** — the same, as JSON. `buys.cpu_ms` is how many
  milliseconds you can still pay for; checking it beats a 402 halfway through a
  backtest.
- **`mojojojo credits`** — the same from the CLI, `Client.credits()` from the SDK.

The balance and ledger live only in mojojojo's PostgreSQL database.

## The browser runtime

`mojojojo.js` runs Python on the caller's own machine first and only reaches for
our servers when the browser genuinely cannot do the job.

```js
import { run, plan } from "https://mojojojo.cc/mojojojo.js"

const r = await run(`print(sum(range(10)))`)
r.stdout   // "45\n"
r.where    // "browser" — Pyodide, free, private
r.why      // "runs locally, free"

const g = await run(train, { gpu: true, apiKey: KEY })
g.where    // "server" — wasm has no GPU
```

It goes remote for: a GPU, an import with no wasm build (`torch`, `polars`,
`requests`, …), a package outside the wasm registry, the native compiled path,
or a browser runtime that failed to start. `plan(code)` tells you which will
happen without running anything.

## Architecture

```
nginx :443 ─→ Go web tier :4341 ─→ Python worker :4342 ─→ mojojail ─→ your code
              auth + billing        envs + accel policy    netns, mojorun uid
```

- **Web tier** (`*.go`) — local password/session auth or `mj_live_` API key,
  per-millisecond accrual, the local credit ledger, and the public API shape. It
  never executes anything.
- **Worker** (`runner/`) — stdlib-only HTTP server. Content-addressed uv venvs,
  the acceleration policy, the sandbox. It has no idea who anybody is.
- **Sandbox** (`runner/mojojail`) — root-owned script behind one sudoers rule.
  Fresh network namespace (no network at all), unprivileged `mojorun` user,
  no-new-privs, hard kill deadline, rlimits on memory/CPU/files/processes, and a
  scratch directory deleted on exit. `isolated: true` in every response confirms
  it ran that way.

### Where a run happens

```
web tier :4341 ──→ local worker :4342 ──→ warm slot ──→ fork ──→ your code
       │                                   (mojojail: netns, mojorun uid)
       └──→ rented worker (only when the local queue is deep and paid for)
```

Sustained demand can expand into overflow capacity. The controller acts only
after demand remains elevated and returns idle capacity when it is no longer needed.

Two defaults make this safe to leave switched on: providers only exist when
their credentials do, and every provider is wrapped in a dry run unless
`SCALE_DRY_RUN=false` is set deliberately. The failure mode of the whole
subsystem is "nothing happens", and runs simply queue locally.

`GET /v1/scale` (admin) shows workers, spend and the last twenty decisions.

### How acceleration is decided

A top-level function is handed to mojosub when it takes arguments, contains a
loop, and stays inside the numeric subset. Everything else runs on CPython,
which is most code.

Compilation does not happen inside the sandbox — there is no compiler in there.
A run that meets a new kernel queues a build request and carries on
interpreting; the host compiles it afterwards, on our clock, and puts it in a
cache addressed by the hash of that generated Mojo. The next run to submit the
same kernel is seeded with that cache and dispatches natively **from its first
call**, after one interpreted call to check the native result against CPython's.
A mismatch retires the variant and you keep the Python answer.

Measured on this box, one kernel called four times per run, the same program
submitted three times:

| submission | billed | what happened |
| --- | ---: | --- |
| 1 | 651 ms | kernel queued for the host compiler; nothing extra billed |
| 2 | 294 ms | **4 of 4 calls native** — warm-started from the shared cache |
| 3 | 223 ms | same, cache hot |

CPython baseline for the same program (`"accel": false`): 678 ms.

## Develop

```bash
go build -o mojojojo . && go test ./...            # web tier + scaling policy
uv venv --python 3.13 .venv                        # worker dev env
.venv/bin/python -m pytest runner/tests -q         # worker
.venv/bin/python -m pytest sdk/python/tests -q     # Python SDK
node --test web/*.test.mjs                         # browser runtime + highlighter
bash tests/e2e.sh                                  # signup -> key -> run -> credits, in a browser too
node tests/browser.mjs http://127.0.0.1:4341       # just the browser pass
bash deploy.sh                                     # units, jail, nginx, restart
```

Without `/usr/local/bin/mojojail` everything still runs, `isolated` comes back
`false`, and you should not point it at untrusted code.

| env | default | |
| --- | --- | --- |
| `MS_PER_CREDIT` | 10000 | CPU price |
| `MS_PER_CREDIT_GPU` | 2000 | GPU price |
| `FREE_MS_PER_DAY` | 30000 | anonymous budget per address |
| `MOJOJOJO_CONCURRENCY` | 8 | concurrent runs; higher inflates everyone's wall clock |
| `MOJOJOJO_COMPILE_CONCURRENCY` | 1–4 by CPU count | shared ceiling for JIT and whole-program Mojo builds |
| `MOJOJOJO_COMPILE_QUEUE` | 8× compile concurrency | pending build cap; excess JIT work stays on CPython |
| `MOJOJOJO_COMPILE_QUEUE_TIMEOUT_S` | 30 | maximum time a build waits for a compiler process |
| `MOJOJOJO_HTTP_CONCURRENCY` | 4× run concurrency | cap on authenticated/parsing request threads |
| `MOJOJOJO_RUNNER_TOKEN` | required off loopback | worker bearer token; a public listener fails closed without it |
| `MOJOJOJO_WARM_MS` | 8000 | unbilled post-run compile window |
| `MOJOJOJO_POOL_SLOTS` | 4 | warm interpreters per environment |
| `MOJOJOJO_GPU_CONCURRENCY` | 2 | concurrent GPU runs; the rest queue |
| `WELCOME_CREDITS` | 1000 | bonus credits per account, usable after the first payment |
| `MOJOJOJO_DATABASE_URL` | local `mojojojo` PostgreSQL database | all identity, billing, and control-plane data |
| `MJJ_AGENT_TOKEN` | — | internal service credential for the shared agent backend |
| `ALLOW_ANONYMOUS` | false | server-side runs without an account |
| `SCALE_DRY_RUN` | true | `false` lets the autoscaler actually rent machines |
| provider credentials | — | absent means overflow capacity remains disabled |
| `MOJOJOJO_DISK_RESERVE_GB` | 20 | free space floor before eviction |
| `MOJOJOJO_CACHE_ARCHIVE` | /sdb-disk/mojojojo/cache | tier 2 |
| `MOJOJOJO_OBJECT_BUCKET` / `MOJOJOJO_STATIC_BASE` | configured at deploy time | durable cache, sites, and assets |
| `MOJOJOJO_STORAGE_SIGNING_KEY` | required in production | signs 15-minute upload tickets |
| `MOJOJOJO_STORAGE_FREE_BYTES` | 1 GiB | included storage per account |
| `MOJOJOJO_STORAGE_MAX_UPLOAD` | 64 MiB | asset and ZIP request limit |
| `MOJOJOJO_MOJO_LIBS` | 1 | offer the ported Mojo libraries |

### Stripe billing

The signed-in `/credits` page uses Stripe embedded Checkout for prepaid credits,
Developer subscriptions, and dedicated RTX 5090 subscriptions. The same local
ledger funds notebooks, hosted functions, CPU/GPU runs, and API-key requests.
Stripe webhooks—not the browser redirect—are authoritative for fulfillment.

Copy `.env.example` to a gitignored `.env` for local deployment configuration:

| env | purpose |
| --- | --- |
| `STRIPE_SECRET_KEY` / `STRIPE_PUBLISHABLE_KEY` | matching live or test account keys |
| `STRIPE_WEBHOOK_SECRET` | signing secret for `/v1/billing/webhook` |
| `STRIPE_CREDITS_PRICE_ID` | one-cent, one-time credit unit Price |
| `STRIPE_DEVELOPER_MONTHLY_PRICE_ID` / `STRIPE_DEVELOPER_ANNUAL_PRICE_ID` | Developer subscription Prices |
| `STRIPE_DEDICATED_MONTHLY_PRICE_ID` / `STRIPE_DEDICATED_ANNUAL_PRICE_ID` | dedicated GPU subscription Prices |
| `STRIPE_AUTOMATIC_TAX` | defaults to `false`; startup rejects `true` without an active Stripe Tax registration |

See [`STRIPE_INTEGRATION_PLAN.md`](STRIPE_INTEGRATION_PLAN.md) for the catalog,
fulfillment model, economics, Tax gate, and production checklist. Never commit
`.env`; production secrets should ultimately live in the deployment secret store.

## The logo

`scripts/logo/make_logo.py` generates the mascot on the local OmniServe gateway
(z-image-turbo), cuts the background with BiRefNet, and writes WebP q85 at 512 /
256 / 96 px. The BiRefNet weights load in a thread that starts before generation
does, because the two do not depend on each other. Measured stages:

```
generate 3 candidates (parallel)   24,618 ms   (~74s if serial)
segmenter warm                     11,931 ms   (overlapped with the above)
background removal                 19,571 ms   (CPU; the GPU was full)
encode webp q85 x3                    191 ms
```

The encode was 7,381 ms until it was measured: WebP `method=6` costs **47x** the
time of `method=4` for 2% of the bytes (6,935 ms → 147 ms, 43.6 KB → 44.7 KB at
512px). `--from image.png` re-cuts an image you already have and skips
generation entirely.

## Security notes

`runner/tests/test_security.py` is written from the attacker's side — "a program
that allocates 8GB does not take the box down", not "the sandbox sets
RLIMIT_DATA" — and the jailed cases skip themselves on a machine without
`mojojail`, which is how you can tell a dev box from production. Writing it
found four real holes, all fixed and all now covered:

- **rlimits were silently lost through `sudo`** (PAM resets the target user's
  limits), so the cold path was unlimited. Limits are now applied inside the
  jail by `bootstrap.py`, which is the first thing that runs there.
- **a caller could set `PATH`** in the run environment, because caller variables
  were merged over ours. They are now merged under, with a reserved list.
- **the sandbox could read any world-readable file on the box**, including
  control-plane databases. It now runs in a mount namespace with
  `/home`, `/root`, `/sdb-disk`, `/opt`, `/var/log` and everything under
  `/nvme0n1-disk` replaced by empty mounts, and only the interpreter, the
  runner, mojosub and the ported libraries bound back read-only.
- **cache seeding handed every run the whole shared cache**, which is every
  other tenant's compiled kernels. A run is now seeded with the kernels of its
  own program text and nothing else.

Also: a fresh **PID namespace**, so a run sees only its own processes and cannot
signal anything else; and run directories and pool sockets live in non-listable
(0733) directories with 32 random hex digits in their names.

- **There is no compiler in the sandbox.** A run sees the compile cache as
  symlinks it can read; writes land in its own directory and die with it. When
  a kernel is worth building, the request is queued and *the host* compiles it
  afterwards, from the source its content hash names — so a shared library can
  only ever be what its content address says it is, built by us.
- **A run teaches the cache one thing**: the return type it learned. Verdicts
  about whether a compiled variant is correct or fast stay behind, so no tenant
  can tell another tenant's run that a wrong answer has been verified. Every run
  re-checks its first native result against CPython for itself.
- Package installs happen **outside** the sandbox, before your code starts —
  which is how imports work despite there being no network inside.
- Run directories and pool sockets live in **non-listable** directories (0733)
  with 32 random hex digits in their names, so one sandboxed run cannot
  enumerate another's.
- `mojojail` lives in `/usr/local/bin`, owned by root, deliberately **not** in
  this repo: a root-run script the service user can edit is a root shell.
- A rented worker has no `mojojail`: a freshly rented VM is single-tenant, and
  the VM boundary is stronger than the namespace one. It authenticates every
  request with a shared token and serves nothing without it.

## Licence

MIT. The execution engine, [mojosub](https://github.com/lee101/mojosub), is
open source and runs anywhere Mojo does — mojojojo is the metered, hosted
version of it.
