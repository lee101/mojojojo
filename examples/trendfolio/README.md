# Trendfolio

Macrotrends-style price chart + stats, with a Python kernel that mojosub
transpiles to native Mojo. Self-contained: stdlib only, no database, no
external API.

```sh
mojojojo project "Trendfolio" --runtime app --category tool
mojojojo publish PROJECT_ID examples/trendfolio
mojojojo submit PROJECT_ID
```

`POST /api/stats` with `{"prices": [...]}` (2–5000 numbers) returns
`summary` (total return, CAGR, max drawdown, MA20/MA50 last) plus the full
MA20/MA50 arrays for charting. The frontend draws the chart on canvas and
puts the series in the URL hash, so the link *is* the share card.

## Why it is fast

`rolling_mean` and `max_drawdown` are annotated, loop-bearing, top-level
functions over `list[float]` — exactly the subset `runner/accel.py`
`eligible()` hands to mojosub. First call verifies native vs CPython once,
then the content-addressed cache serves everyone. Measured on a 1M-element
reduction the native loop runs ~50–100x CPython; the copy of a Python list
across the C ABI is the floor, so for large series pass numpy arrays via
the run API (`__array_interface__` buffers are passed by address — see the
hint in `accel.hints()`).

## The perf contract (worth copying into your own kernels)

1. Annotate every parameter and the return; unannotated helpers are not in
   the transpiler unit and take their callers down with them.
2. Keep one loop per kernel — `eligible()` needs a loop to offer it.
3. Use `math.sqrt`/`math.exp` etc. — whitelisted; `math.gamma` is not.
4. Module constants must be numeric literals bound once; `WINDOW = 64`
   works, rebinding does not.

## Honest limits

Demo data is synthetic (LCG) unless you paste real prices. No live
market data is fetched — that keeps the example self-contained and free of
terms-of-service issues. There is no persistence; the URL hash is the store.
