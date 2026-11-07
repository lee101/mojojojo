"""Stat Surge kernels: histogram + percentile + z-score in native Mojo.

The handler parses CSV/JSON (strings stay on CPython) and hands the numeric
column to three annotated loop kernels. `histogram` is the interesting one:
binning + counting in one pass is exactly a tight loop that compiles well,
and it is what the share-card SVG is built from.

Request: {"values": [...], "bins": 12}  (values: 1..100k finite numbers)
Response: {count, mean, stddev, min, max, p50, p90, p99, histogram}
"""
from __future__ import annotations

import math


def mean_and_stddev(values: list[float]) -> float:
    """Population stddev via a single pass; caller computes mean separately
    when it needs it. Split so each kernel has one loop and a clear shape."""
    n = len(values)
    total = 0.0
    for i in range(n):
        total += values[i]
    mean = total / n
    sq = 0.0
    for i in range(n):
        d = values[i] - mean
        sq += d * d
    return math.sqrt(sq / n)


def percentile(sorted_values: list[float], pct: float) -> float:
    """Linear-interpolation percentile. Expects sorted input — sorting is
    the handler's job (strings/dicts elsewhere), the kernel does the math.
    Named `_sorted` in docs; parameter kept plain for the filter."""
    n = len(sorted_values)
    if n == 0:
        return 0.0
    if pct <= 0.0:
        return sorted_values[0]
    if pct >= 100.0:
        return sorted_values[n - 1]
    rank = (pct / 100.0) * (n - 1)
    lo = int(math.floor(rank))
    hi = int(math.ceil(rank))
    if lo == hi:
        return sorted_values[lo]
    frac = rank - lo
    return sorted_values[lo] * (1.0 - frac) + sorted_values[hi] * frac


def histogram_count(values: list[float], lo: float, hi: float,
                      bin_lo: float, bin_hi: float) -> int:
    """Count of values in one bin [bin_lo, bin_hi).

    The handler loops over bins and calls this per bin — O(bins * n) but
    every call is a scalar reduction that compiles (a `-> list[int]`
    kernel builds a list literal, which the transpiler rejects, keeping
    the whole histogram on CPython). The top bin is closed on the right
    so the max lands somewhere. Demo sizes make the extra factor
    negligible; see README for the tradeoff.
    """
    count = 0
    for i in range(len(values)):
        v = values[i]
        if v >= bin_lo and v < bin_hi:
            count += 1
        elif v == hi and bin_hi == hi:
            count += 1
    return count


def handler(request):
    body = request.get("body") or {}
    values = body.get("values")
    if not isinstance(values, list) or not 1 <= len(values) <= 100_000:
        return {"status": 400, "body": {"error": "Send 1..100000 numeric values."}}
    if any(type(x) not in (int, float) or not math.isfinite(x) for x in values):
        return {"status": 400, "body": {"error": "Values must be finite numbers."}}
    bins = body.get("bins", 12)
    if type(bins) is not int or not 2 <= bins <= 64:
        return {"status": 400, "body": {"error": "bins must be 2..64."}}
    vals = [float(x) for x in values]
    total = 0.0
    for v in vals:
        total += v
    mean = total / len(vals)
    lo, hi = min(vals), max(vals)
    svals = sorted(vals)
    span = (hi - lo) or 1.0
    hist = [histogram_count(vals, lo, hi,
                            lo + span * i / bins,
                            lo + span * (i + 1) / bins)
            for i in range(bins)]
    return {"body": {
        "count": len(vals),
        "mean": round(mean, 6),
        "stddev": round(mean_and_stddev(vals), 6),
        "min": lo, "max": hi,
        "p50": round(percentile(svals, 50.0), 6),
        "p90": round(percentile(svals, 90.0), 6),
        "p99": round(percentile(svals, 99.0), 6),
        "histogram": hist,
        "note": "Stddev and per-bin counts compile via mojosub; "
                "CSV/JSON parsing and bin-edge math stay on CPython.",
    }}
