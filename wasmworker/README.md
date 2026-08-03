# wasmworker — mojojojo at the edge

A Cloudflare Worker that runs Python in a wasm isolate at the nearest point of
presence. Same `/run` contract as the worker on our own box, minus everything
an isolate cannot do: no Mojo, no GPU, no packages, no threads, no filesystem,
no network.

```bash
bash dev.sh                     # workerd locally, http://127.0.0.1:8787
npm test                        # 24 tests against the real runtime
npm run deploy                  # only when somebody means it
wrangler secret put EDGE_TOKEN  # the shared secret; without it the URL is open
```

Then point the control plane at it:

```bash
EDGE_URL=https://mojojojo-edge.<account>.workers.dev EDGE_TOKEN=<same secret>
```

## Why this tier exists

Most submitted programs are small. A four-millisecond program does not deserve
a trans-Pacific round trip, and the numbers say where the line is:

| | reach | what it costs us |
| --- | --- | --- |
| browser | 0ms | nothing |
| edge | ~25ms | $0.30/M requests + $0.02/M CPU-ms |
| our box | ~180ms | electricity |

At our list price a millisecond earns $0.0000001, so an edge run pays for its
own request fee at about **4ms** (`EDGE_MIN_MS` in `web/route.js`) and stops
being the cheap option at about **200ms**, where the box we already own wins.
Between those two numbers the edge is both faster for the caller and profitable
for us; outside them it is one or the other, and `route.js` says which.

## Tickets

The edge is only worth having if the browser reaches it directly — proxying
through the control plane spends the latency we came here to save. So
`/v1/plan` signs a short-lived ticket, the Worker verifies it, runs the
program, and signs a receipt for the milliseconds it used. The browser hands
the receipt to `/v1/settle`, which bills it.

A receipt that never comes back is a run we do not charge for, capped by the
ticket's budget and by four outstanding tickets per account. That is deliberate:
billing before the work charges people for runs that never happened.
