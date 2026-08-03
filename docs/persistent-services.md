# Persistent services

mojojojo has two compute products with intentionally different contracts.

## Scale-to-zero functions

This is the current default and the right answer for HTTP APIs, conversions,
webhooks, background jobs, and most multiplayer coordination. A request loads
an immutable deployment, runs one declared Python or Mojo function in the
sandbox, records its measured milliseconds, and exits. Durable state belongs
in managed data, object storage, vector projects, or the room broker.

There is no idle charge. Queueing, environment construction, and compilation
are not billed. Function memory is never a source of truth because another
request may run in another process.

## Persistent services

A persistent service reserves an isolated process and a fixed resource
profile. It is for WebSockets, custom protocols, in-memory indexes, continuously
warm models, and long-running Mojo event loops. It is billed for the reservation
while running, including idle time, because the capacity cannot serve another
tenant.

The deployment contract is:

```json
{
  "runtime": "app",
  "entrypoint": "server.mojo",
  "compute": {
    "mode": "persistent",
    "profile": "cpu-1",
    "health_path": "/health",
    "min_instances": 1
  }
}
```

Persistent manifests are currently rejected with an explicit limited-preview
error. They must not silently become scale-to-zero functions. General
availability requires the supervisor gates below to be complete.

## Runtime boundary

The persistent process uses the same root-owned jail as one-shot execution:
an unprivileged uid, private PID and mount namespaces, no host filesystem, no
compiler, bounded memory/process/file descriptors, and an immutable deployment.
The jail keeps its network namespace. Customer HTTP is carried over a
per-service Unix socket exposed to the platform proxy; the process never binds
a host port and never receives infrastructure credentials.

The supervisor must:

1. compile or resolve the immutable deployment before billing starts;
2. start it in a project-specific jail and wait for the health check;
3. route only after health succeeds;
4. restart with bounded backoff and expose restart/crash history;
5. drain connections before an update or cancellation;
6. stop routing and billing when capacity is unavailable;
7. restore active reservations after a worker or control-plane restart.

Mojo services use a persistent request loop. Python services load the module
once and call its handler for each request. Process memory survives requests,
but durable state still belongs in managed storage because restarts are part of
the service contract.

## Billing and margin guardrails

One credit remains $0.001. Persistent capacity is charged hourly in advance,
with no per-request CPU charge inside the reserved profile:

| Profile | Reserved resources | Credits/hour | USD/hour | Approx. 30-day maximum |
| --- | --- | ---: | ---: | ---: |
| `cpu-nano` | 0.25 shared CPU, 512 MiB | 200 | $0.20 | $144 |
| `cpu-1` | 1 CPU, 2 GiB | 750 | $0.75 | $540 |
| `cpu-2` | 2 CPU, 4 GiB | 1,400 | $1.40 | $1,008 |
| `gpu-24` | 4 CPU, 16 GiB, 24 GiB GPU | 3,500 | $3.50 | $2,520 |

Cancellation ends at the next hourly renewal. Failed builds are never charged.
If the platform cannot supply the reserved profile, the reservation pauses and
billing pauses with it. Object storage remains separately metered; delivery and
queue time remain unbilled.

These rates deliberately price the reserved, unavailable-to-others capacity
above burst execution. Scale-to-zero should remain cheaper for workloads that
do not genuinely need a live process.

## Release gates

- Unix-socket proxy and request-size/time limits
- Python and Mojo persistent adapters
- worker restart reconciliation
- health, crash-loop, drain, update, and cancellation tests
- hourly ledger idempotency and insufficient-credit pause tests
- per-profile concurrency and memory enforcement
- customer lifecycle UI and API
- load and abuse tests, including slow clients and connection exhaustion

