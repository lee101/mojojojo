"""Worldline kernels: OWID-style per-country series stats.

The numeric work lives in `per_capita_first` / `rank_by_latest` style
helpers — annotated, loop-bearing, `math`-only. Strings, dicts, CSV and
JSON stay in the handler on CPython. Same contract as Trendfolio.

Request: {"countries": {"NZ": [values...], "US": [...]}, "min_points": 2}
Response: per-country {first, last, growth_pct, cagr_pct, n} sorted by
growth, plus the global ranking. The browser draws small multiples.
"""
from __future__ import annotations

import math


def growth_pct(first: float, last: float) -> float:
    if first == 0.0:
        return 0.0
    return (last / first - 1.0) * 100.0


def series_cagr(first: float, last: float, periods: int) -> float:
    if first <= 0.0 or last <= 0.0 or periods <= 0:
        return 0.0
    return (math.pow(last / first, 1.0 / periods) - 1.0) * 100.0


def latest_value(values: list[float]) -> float:
    """Last non-missing observation."""
    for i in range(len(values) - 1, -1, -1):
        v = values[i]
        if v >= 0.0:
            return v
    return 0.0


def handler(request):
    body = request.get("body") or {}
    countries = body.get("countries")
    if not isinstance(countries, dict) or not 1 <= len(countries) <= 250:
        return {"status": 400, "body": {"error": "Send 1..250 country series."}}
    min_points = body.get("min_points", 2)
    if type(min_points) is not int or not 1 <= min_points <= 100:
        return {"status": 400, "body": {"error": "min_points must be 1..100."}}

    rows = []
    for code, values in countries.items():
        if not isinstance(code, str) or not 1 <= len(code) <= 12:
            return {"status": 400, "body": {"error": f"Bad country code: {code!r}"}}
        if not isinstance(values, list) or len(values) > 500:
            return {"status": 400, "body": {"error": f"{code}: series must be a list of <=500 numbers."}}
        clean = []
        for v in values:
            if v is None:
                continue
            if type(v) not in (int, float) or not math.isfinite(v):
                return {"status": 400, "body": {"error": f"{code}: values must be finite numbers."}}
            clean.append(float(v))
        if len(clean) < min_points:
            continue
        first, last = clean[0], latest_value(clean)
        rows.append({
            "code": code,
            "n": len(clean),
            "first": round(first, 4),
            "last": round(last, 4),
            "growth_pct": round(growth_pct(first, last), 2),
            "cagr_pct": round(series_cagr(first, last, len(clean) - 1), 4),
        })
    rows.sort(key=lambda r: (r["growth_pct"], r["code"]), reverse=True)
    return {"body": {
        "ranked": rows,
        "count": len(rows),
        "note": "Kernel stats (growth, CAGR, latest) compile via mojosub; "
                "ranking/JSON stay on CPython.",
    }}
