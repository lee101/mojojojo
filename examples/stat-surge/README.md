# Stat Surge

Viral CSV stats + share card. Paste numbers (or drop a `.csv`), get mean,
stddev, p50/p90/p99 and a 16-bin histogram from a Python kernel that mojosub
compiles to native Mojo, plus an SVG share card you can copy.

```sh
mojojojo project "Stat Surge" --runtime app --category tool
mojojojo publish PROJECT_ID examples/stat-surge
mojojojo submit PROJECT_ID
```

`POST /api/surge` with `{"values": [...], "bins": 16}` (1–100k finite
numbers, 2–64 bins) returns count/mean/stddev/min/max/p50/p90/p99/histogram.

## Perf notes

Three kernels, three shapes, all annotated + loop-bearing so all three
compile:

- `mean_and_stddev` — two sequential passes, scalar, no buffer copy-back
  (`writes` analysis sees no stores, so the list is not copied back).
- `percentile` — random access into a sorted buffer; the *sort* stays in
  the handler because `sorted()` is a method call the filter rejects.
- `histogram` — one pass, integer bin index. The `int()` cast is why the
  index type stays `Int` and the loop vectorises cleanly.

The takeaway for your own kernels: keep sorts, dicts and JSON in the
handler; keep the hot loop numeric. The boundary is exactly the boundary
`accel.why_not()` reports — run it on your function (`bench/` has the
corpus scanner) before you assume it compiled.
