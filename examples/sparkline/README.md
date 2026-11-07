# Sparkline

Embeddable tiny charts. Any number column in, a self-contained SVG out —
good for READMEs, dashboards, and docs. Downsampling and the mean run as
Mojo-compiled kernels; SVG assembly stays in the handler.

```sh
mojojojo project "Sparkline" --runtime app --category tool
mojojojo publish PROJECT_ID examples/sparkline
mojojojo submit PROJECT_ID
```

`POST /api/render` with `{values: [...], width: 860, target: 140}` (2–100k
finite numbers) returns an SVG string, point count, min/max/mean, and the
index of the most important point (LTTB bucket-importance score). Copy
the SVG and paste it anywhere — it has no external dependencies.

## Perf notes

`series_mean` is a one-line reduction. `bucket_extremes` is the more
interesting shape: nested bucket loops with a running mean and a
deviation argmax, all scalar, all compilable. The handler's downsampling
loop is *not* compiled (it builds a list) — deliberately, and the code
says so. If you need full LTTB at scale, the per-bucket argmax kernels
are the pattern to copy.

## Honest limits

The demo downsampler is bucket-averaging, not full LTTB — it preserves
the envelope shape but smooths spikes. The importance kernel is real;
use its index to find the peak. See `trendfolio` if you need the chart
*and* the stats in one call.
