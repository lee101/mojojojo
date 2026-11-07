# Season Line

Sports form visualizer. Paste a season as W/L/D characters, get record,
win %, last-N form, best/worst runs, and a simplified Elo-like rating.
The four kernels are scalar loops over a `list[float]` signal — all
compile via mojosub.

```sh
mojojojo project "Season Line" --runtime app --category tool
mojojojo publish PROJECT_ID examples/season-line
mojojojo submit PROJECT_ID
```

`POST /api/form` with `{results: [...], last_n: 5, k: 10}` where results
are `"W"/"L"/"D"` or `+1/-1/0` numbers (1–500 entries) returns the full
form summary. The chart on the page is a cumulative W−L sparkline drawn
from the same input, and the season is encoded in the URL hash to share.

## Perf notes

`last_n_sum`, `best_run`, `worst_run`, `elo_like` — four annotated
reductions, zero lists built, zero method calls. The handler absorbs
all string parsing (which the filter correctly rejects as `str` literals
and method calls) and hands the kernels a clean numeric buffer. This is
the recommended pattern for data-vis examples: **parse in Python,
reduce in Mojo**.
