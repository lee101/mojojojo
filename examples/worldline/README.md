# Worldline

Our-World-in-Data-style country explorer. Rank countries by growth, draw
small multiples. The numeric kernels (`growth_pct`, `series_cagr`,
`latest_value`) are mojosub-compilable; the chart is canvas, the table is
HTML, nothing else is required.

```sh
mojojojo project "Worldline" --runtime app --category tool
mojojojo publish PROJECT_ID examples/worldline
mojojojo submit PROJECT_ID
```

`POST /api/explore` with `{"countries": {"NZ": [...], "US": [...]}}`
returns each country ranked by growth with first/latest/CAGR. Demo data
on the page is synthetic — paste real series through the same endpoint.
No live scraping: no ToS issues, no keys, no cost beyond the function.

## Why this shape

The kernel is three tiny annotated functions over `list[float]`. That is
the whole perf trick: the accel filter only needs *a loop + annotations*,
so the example doubles as documentation of what compiles. See
`examples/trendfolio` for the heavier version and `runner/accel.py`
`eligible()` for the rule.
