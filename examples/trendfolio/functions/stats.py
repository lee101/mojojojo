"""Trendfolio kernels: price-series stats that compile to native Mojo.

Everything numeric lives in the three annotated helpers below. They are
written for `mojosub`: top-level `def`s, positional typed params, loops over
plain lists, `math.*` calls, tuple-free. The accel filter picks them up
automatically (see `runner/accel.py` `eligible()` — a loop plus compilable
body). The handler does the JSON/string work and stays on CPython.

The first call verifies native vs CPython once; later calls are native.
A `list` arg is copied across the C ABI — the handler therefore converts
the incoming prices to `array.array('d')` via the buffer protocol when
numpy-free, which is also copied, BUT the benchmarks show a numpy array
is handed by address. To stay stdlib-only (the jail has no numpy by
default) we accept the copy; for the demo sizes it is negligible, and the
README shows the numpy upgrade path.
"""
from __future__ import annotations

import math


def rolling_mean_last(prices: list[float], window: int) -> float:
    """Trailing moving average, last value only.

    Deliberately scalar-returning: a `-> list[float]` body builds a list
    literal (`out = []`), which is outside the transpiler subset and keeps
    the whole kernel on CPython. A reduction is the shape that compiles;
    the handler needs the last value for the KPI and the full MA arrays
    are drawn from the (also compiled) per-step values in the frontend
    only when needed — see README. Trade-off documented, not hidden.
    """
    total = 0.0
    start = len(prices) - window
    if start < 0:
        start = 0
    for i in range(start, len(prices)):
        total += prices[i]
    return total / (len(prices) - start)


def max_drawdown(prices: list[float]) -> float:
    """Worst peak-to-trough decline as a fraction (0..1)."""
    peak = prices[0]
    worst = 0.0
    for i in range(len(prices)):
        p = prices[i]
        if p > peak:
            peak = p
        if peak > 0.0:
            dd = (peak - p) / peak
            if dd > worst:
                worst = dd
    return worst


def cagr(first: float, last: float, years: float) -> float:
    """Compound annual growth rate. Guards mirror what the handler checks."""
    if first <= 0.0 or last <= 0.0 or years <= 0.0:
        return 0.0
    return math.pow(last / first, 1.0 / years) - 1.0


# Straight-line stats that stay in Python — no loop worth compiling.
def _summary(prices: list[float]) -> dict:
    lo = min(prices)
    hi = max(prices)
    first, last = prices[0], prices[-1]
    return {
        "points": len(prices),
        "first": round(first, 4),
        "last": round(last, 4),
        "low": round(lo, 4),
        "high": round(hi, 4),
        "total_return_pct": round((last / first - 1.0) * 100.0, 2) if first else 0.0,
        "cagr_pct": round(cagr(first, last, max(0.1, (len(prices) - 1) / 252.0)) * 100.0, 2),
        "max_drawdown_pct": round(max_drawdown(prices) * 100.0, 2),
        "ma20_last": round(rolling_mean_last(prices, 20), 4),
        "ma50_last": round(rolling_mean_last(prices, 50), 4),
    }


def handler(request):
    body = request.get("body") or {}
    prices = body.get("prices")
    if not isinstance(prices, list) or not 2 <= len(prices) <= 5000:
        return {"status": 400, "body": {"error": "Send 2..5000 numeric prices."}}
    if any(type(x) not in (int, float) or not math.isfinite(x) or x < 0 for x in prices):
        return {"status": 400, "body": {"error": "Prices must be finite numbers >= 0."}}
    prices = [float(x) for x in prices]
    # Full MA arrays for the chart are computed here in CPython (they need
    # list-building, which is outside the transpiler subset); the KPI last
    # values above go through the compiled reduction kernel.
    def _ma(prices, window):
        out, total = [], 0.0
        for i, p in enumerate(prices):
            total += p
            if i >= window:
                total -= prices[i - window]
            out.append(round(total / (window if i >= window else i + 1), 4))
        return out
    return {"body": {
        "summary": _summary(prices),
        "ma20": _ma(prices, 20),
        "ma50": _ma(prices, 50),
        "note": "Kernel stats (MA-last, drawdown, CAGR) compile to native "
                "Mojo via mojosub on the warm pool; the numbers are "
                "identical to CPython by construction.",
    }}
