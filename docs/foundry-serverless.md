# Foundry Models on Mojojojo

## What works in this change

- Function manifests accept `billing: caller` and `gpu: true`.
- Published caller-paid functions require POST and a valid API key; cookies do not authorize payment.
- The existing ledger bills the caller's measured execution at the CPU/GPU rate. Previews remain owner-paid.
- `examples/foundry-numeric` trains a bounded numeric centroid baseline and returns a downloadable JSON artifact.
- The rental manager can select by measured workload completion cost instead of hourly price.

This is source implementation, not a production deployment. It does not make
all Foundry scaffolds trained, optimized, zero-shot, or release-approved.

## Deploy an app/API

Use the installed SDK or Studio. Publication is an explicit operator step:

```sh
mojojojo project "Foundry Numeric Baseline" --runtime app --category tool
mojojojo publish PROJECT_ID examples/foundry-numeric
```

The page is `/p/PROJECT_ID/`; the function is `/p/PROJECT_ID/api/model`.
The installed SDK manages account access; never put infrastructure secrets
into a bundle. Do not publish logs, training data or unreviewed weights.

```json
{
  "runtime": "app",
  "entrypoint": "index.html",
  "functions": {
    "infer": {
      "language": "python",
      "entrypoint": "functions/infer.py",
      "billing": "caller",
      "gpu": false,
      "timeout_ms": 10000
    }
  }
}
```

Python functions export `handler(request)` and read JSON from `request["body"]`.
For API clients, pass `Authorization: Bearer YOUR_MOJOJOJO_KEY` over HTTPS.
Provider credentials and the caller's key are not included in the request
envelope handed to the function. Invalid credentials never fall back to the
owner's wallet. CORS allows explicit API-key headers, not cookie-based billing.
Callers opt into the manifest's compute pricing by invoking this authenticated
endpoint; this change does not implement per-call spending caps or credit holds.

The function source is currently loaded alone. Bundling sibling Python files or
weights does not automatically mount them into the worker. For reviewed real
models, implement immutable, hash-verified artifact staging and private access
before exposing inference. Never load caller-supplied pickle/joblib files.
Hosted functions retain the 1 MiB request and ten-second execution limits.
Do not describe synchronous functions as durable asynchronous training jobs.

## All 21 Workloads

These are recommended first execution profiles, not measured winners. Start
on local CPU/GPU capacity; rent only for sustained paid overflow after measurement.
Except for tabular's existing local artifacts, the top-level project READMEs
describe scaffolds. Each still needs an independent quality/license/release gate.

| Project | API contract | First inference profile | Training artifact / gate |
| --- | --- | --- | --- |
| mojo-tabular | rows -> probabilities or regression values | CPU | reviewed sklearn ensemble; schema and preprocessing pinned |
| mojo-learning-to-rank | query + candidates -> scores | CPU | ranker + feature schema; grouped query splits |
| mojo-anomaly-detection | observations -> anomaly scores | CPU | detector + threshold; validation-only threshold selection |
| mojo-fraud-risk | transactions -> risk scores | CPU | calibrated classifier; temporal evaluation, no decision automation |
| mojo-recommendation | user + items -> scores | CPU | embeddings/ranker; user/time split and cold-start evaluation |
| mojo-embeddings | texts -> vectors | CPU, GPU for measured batches | pinned encoder + tokenizer; retrieval held-out evaluation |
| mojo-rerankers | query + documents -> scores | CPU, GPU for measured batches | encoder + tokenizer; query-group split |
| mojo-agent-routing | task features -> route scores | CPU | routing policy; quality/cost trace evaluation |
| mojo-content-moderation | content -> taxonomy scores | CPU or measured GPU | classifier + taxonomy; false-positive and shift review |
| mojo-document-classification | document -> class scores | CPU | encoder + labels; source-separated splits |
| mojo-document-extraction | document -> structured fields | measured GPU | extractor + schema; field-level evaluation and PII controls |
| mojo-audio-events | audio -> event scores | CPU | encoder + sample-rate preprocessing; source-separated audio |
| mojo-wake-word | audio -> activation score | CPU | model + threshold; false triggers per hour |
| mojo-speaker-emotion | audio -> descriptive scores | CPU | model + labels; speaker-disjoint split, no sensitive decisions |
| mojo-vision-classification | image -> class scores | CPU or measured GPU | model + image normalization; source-separated evaluation |
| mojo-object-detection | image -> boxes/classes | measured GPU | detector + thresholds; held-out detection evaluation |
| mojo-segmentation | image -> masks | measured GPU | model + label map; held-out mask evaluation |
| mojo-pose-keypoints | image -> keypoints/confidence | measured GPU | model + skeleton schema; occlusion/geometry evaluation |
| mojo-robotics-perception | sensor observations -> descriptive estimates | measured GPU | perception model; offline safety review, no actuator access |
| mojo-learned-control | offline observations -> policy evaluation | CPU or measured GPU | research-only policy artifact; never deploy to physical control |
| mojo-scientific-industrial | numeric inputs -> predictions | CPU | task-specific model + units; physical constraints and shift checks |

Small models do not automatically support zero-shot inference. Advertise that
capability only for a licensed pinned model with task-specific held-out evidence.
Mojo acceleration similarly requires parity and measured benefit against the
reference backend; project names are not evidence of native execution.

## Cost per workload

`scale.CheapestWorkload` compares one homogeneous batch at a time:

`total USD = hourly USD * max(cold start + jobs * measured job time, minimum billed duration) / hour + extra costs`

It filters by compute kind, device memory, revision, deadline and explicit batch
cost cap. Missing, invalid or revision-mismatched measurements cannot authorize
a rental. NaN/infinite money values are rejected. Equal-cost offers prefer the
shorter completion time. Measurements must include batch size, precision,
runtime/model/data revisions and sufficient memory headroom in their revision
identity; rates must come from reviewed current provider offers. Existing
hardcoded provider rates are not live quotes or a guaranteed bill.

To use the selector, trusted queue telemetry supplies `Snapshot.Workload` with
`WorkloadMeasurement` records. The manager then applies its existing daily
budget, paid-revenue margin, queue age, worker cap and cooldown gates. If no
workload is supplied, legacy hourly selection stays unchanged. The current
web-tier `scaleSignal` does not yet supply model-specific telemetry: this change
does not claim automatic measured routing is live in production. CPU offers
also need reviewed memory metadata before participating in measured selection.

Prefer the existing home GPU when it has admitted capacity. Do not rent for
tiny CPU inference simply because training can use a GPU. Measure cold loads,
per-request latency and throughput separately. Batch compatible requests only;
model caches never mix tenant datasets or private fine-tuned weights. Apply
draining/worker health and idle teardown before enabling real rental spending.

## Durable training jobs still required

The numeric example is synchronous and returns its artifact directly. Real
long-running training needs a separate durable lifecycle using existing auth,
runner, sandbox, storage and billing rather than a second execution service:

1. Authenticate the payer and reserve a bounded budget transactionally.
2. Pin immutable split/model/source hashes; check licenses and accepted task limits.
3. Persist an idempotent job and lease it to compatible local capacity first.
4. Enforce GPU memory, wall time, disk, retries, cancellation and total cost.
5. Upload a tenant-private content-addressed artifact and manifest before success.
6. Settle measured usage once, release unused holds, and expose authenticated download.
7. Recover leases/holds after restart; evaluate held-out data only after validation selection.

Do not launch user training in a web-handler goroutine, raise synchronous
timeouts to pretend persistence exists, allow remote model code, or publish
artifacts automatically. Control-model jobs remain offline research only.
No GPU experiments, provider calls or production publishing were performed
while developing this change.
