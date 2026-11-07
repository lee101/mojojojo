# Compound Growth

Viral finance calculator. Monthly compounding with annual contribution
raises, inflation adjustment, and the "crossover year" where growth
overtakes contributions. The three projection kernels are scalar loops —
the exact shape that compiles.

```sh
mojojojo project "Compound Growth" --runtime app --category tool
mojojojo publish PROJECT_ID examples/compound-growth
mojojojo submit PROJECT_ID
```

`POST /api/grow` with `{principal, monthly, annual_return (decimal),
years, annual_raise (decimal), inflation (decimal)}` returns final
balance, total paid in, growth, crossover year, and inflation-adjusted
final. Inputs are range-checked; the URL hash encodes the scenario so
links are shareable.

## Perf notes

`project_final` / `total_contributions` / `crossover_year` are annotated,
loop-bearing, `math.pow`-only — all three pass `accel.eligible()` and
compile via mojosub. The two nested loops (years × months) are why this
is worth compiling at all: 360 iterations of trivial Python becomes
360 iterations of trivial native code, and the verify-once-then-cache
policy means the first caller pays the compile and everyone after rides
the content-addressed cache.

## Honest limits

Deterministic projection, not financial advice. Returns are assumed
constant; real markets are not. No tax, fees, or volatility modelling.
