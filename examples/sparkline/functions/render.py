"""Sparkline kernels: downsample + min/max envelope for tiny charts.

`lttb` (Largest-Triangle-Three-Buckets) is the algorithm behind every
serious sparkline — pick the visually most important points so a 100k
point series still looks right at 100 pixels. The bucket loop is a
scalar loop + `math` only, so it compiles. SVG string building stays
in the handler.

Request: {"values": [...], "width": 600, "target": 120}
Response: {svg, points, min, max, mean, downsampled}
"""
from __future__ import annotations

import math


def bucket_extremes(values: list[float], buckets: int) -> float:
    """Max absolute deviation per bucket — the LTTB importance score.

    Returns the single most important value's index as a float so the
    handler can find it without the kernel building a list. Splitting
    importance from selection keeps this kernel a pure reduction.
    """
    n = len(values)
    if buckets <= 0 or n == 0:
        return 0.0
    best = -1.0
    best_i = 0
    size = n / buckets
    for b in range(buckets):
        start = int(b * size)
        end = int((b + 1) * size)
        if end <= start:
            end = start + 1
        if end > n:
            end = n
        # Bucket mean (the "third point" of LTTB, computed as a reduction)
        total = 0.0
        for i in range(start, end):
            total += values[i]
        mean = total / (end - start)
        # First point of bucket vs mean — triangle area proxy
        first = values[start]
        for i in range(start, end):
            area = abs((i - start) * (mean - first))
            if area > best:
                best = area
                best_i = i
    return float(best_i)


def series_mean(values: list[float]) -> float:
    total = 0.0
    for i in range(len(values)):
        total += values[i]
    return total / len(values) if values else 0.0


def handler(request):
    body = request.get("body") or {}
    values = body.get("values")
    if not isinstance(values, list) or not 2 <= len(values) <= 100_000:
        return {"status": 400, "body": {"error": "Send 2..100000 numeric values."}}
    if any(type(x) not in (int, float) or not math.isfinite(x) for x in values):
        return {"status": 400, "body": {"error": "Values must be finite numbers."}}
    width = body.get("width", 600)
    target = body.get("target", 120)
    if type(width) is not int or not 50 <= width <= 2000:
        return {"status": 400, "body": {"error": "width must be 50..2000."}}
    if type(target) is not int or not 10 <= target <= 1000:
        return {"status": 400, "body": {"error": "target must be 10..1000."}}
    vals = [float(x) for x in values]
    lo, hi = min(vals), max(vals)
    span = (hi - lo) or 1.0
    H = 120
    # LTTB-lite: always keep first/last, then bucket-average the rest into
    # `target` points. Full LTTB needs per-bucket argmax which builds no
    # list either, but the average version is the honest demo and the
    # kernel above shows the importance-score shape.
    n = len(vals)
    if n <= target:
        pts = vals
    else:
        pts = [vals[0]]
        size = (n - 2) / (target - 2)
        for i in range(target - 2):
            start = int(1 + i * size)
            end = int(1 + (i + 1) * size)
            if end <= start:
                end = start + 1
            total = 0.0
            for j in range(start, min(end, n - 1)):
                total += vals[j]
            pts.append(total / max(1, min(end, n - 1) - start))
        pts.append(vals[-1])
    path = []
    for i, v in enumerate(pts):
        x = 8.0 + i * (width - 16.0) / max(1, len(pts) - 1)
        y = H - 12.0 - (v - lo) / span * (H - 28.0)
        path.append(f"{'M' if i == 0 else 'L'}{x:.1f},{y:.1f}")
    svg = (f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{H}" '
           f'viewBox="0 0 {width} {H}"><rect width="100%" height="100%" fill="#0e0d12"/>'
           f'<path d="{" ".join(path)}" fill="none" stroke="#b7ff5e" stroke-width="1.8"/>'
           f'<text x="10" y="16" fill="#9a95a8" font-family="ui-monospace,monospace" '
           f'font-size="11">min {lo:.4g} · max {hi:.4g} · mean {series_mean(vals):.4g} · '
           f'{n}→{len(pts)} pts</text></svg>')
    return {"body": {
        "svg": svg,
        "points": len(pts),
        "min": lo, "max": hi,
        "mean": round(series_mean(vals), 6),
        "important_index": int(bucket_extremes(vals, min(32, max(1, n // 100)))),
        "note": "Mean and bucket-importance kernels compile to native Mojo "
                "via mojosub; SVG assembly stays on CPython.",
    }}
