"""Season Line kernels: rolling form + streak stats for a game-by-game season.

Input is per-game point differentials (W = +, L = -) or W/L strings the
handler maps to +1/-1. The kernels are scalar reductions over the numeric
signal (`last_n_sum`, `best_run`, `elo_like`) — all compile. Strings,
names and JSON stay in the handler.

Request: {"results": [1, -1, 1, ...] or ["W","L",...], "last_n": 5}
Response: {record, win_pct, last_n_form, best_run, worst_run, form_last}
"""
from __future__ import annotations

import math


def last_n_sum(results: list[float], n: int) -> float:
    """Sum of the last n results — the 'form' number (range -n..n)."""
    total = 0.0
    start = len(results) - n
    if start < 0:
        start = 0
    for i in range(start, len(results)):
        total += results[i]
    return total


def best_run(results: list[float]) -> int:
    """Longest consecutive run of wins (results are +1/-1)."""
    best = 0
    cur = 0
    for i in range(len(results)):
        if results[i] > 0.0:
            cur += 1
            if cur > best:
                best = cur
        else:
            cur = 0
    return best


def worst_run(results: list[float]) -> int:
    """Longest consecutive run of losses."""
    worst = 0
    cur = 0
    for i in range(len(results)):
        if results[i] < 0.0:
            cur += 1
            if cur > worst:
                worst = cur
        else:
            cur = 0
    return worst


def elo_like(results: list[float], k: float) -> float:
    """Simplified rating: start 1500, W +k, L -k, draws ignored (0.0)."""
    rating = 1500.0
    for i in range(len(results)):
        r = results[i]
        if r > 0.0:
            rating += k
        elif r < 0.0:
            rating -= k
    return rating


def handler(request):
    body = request.get("body") or {}
    raw = body.get("results")
    if not isinstance(raw, list) or not 1 <= len(raw) <= 500:
        return {"status": 400, "body": {"error": "Send 1..500 results."}}
    last_n = body.get("last_n", 5)
    if type(last_n) is not int or not 1 <= last_n <= 50:
        return {"status": 400, "body": {"error": "last_n must be 1..50."}}
    k = body.get("k", 10.0)
    if type(k) not in (int, float) or not math.isfinite(k) or not 0.0 <= k <= 100.0:
        return {"status": 400, "body": {"error": "k must be 0..100."}}
    results: list[float] = []
    for r in raw:
        if r in ("W", "w", 1, 1.0, "+"):
            results.append(1.0)
        elif r in ("L", "l", -1, -1.0, "-"):
            results.append(-1.0)
        elif r in ("D", "d", 0, 0.0):
            results.append(0.0)
        elif type(r) in (int, float) and math.isfinite(r):
            results.append(1.0 if r > 0 else (-1.0 if r < 0 else 0.0))
        else:
            return {"status": 400, "body": {"error": f"Bad result: {r!r}"}}
    wins = 0.0
    for r in results:
        if r > 0.0:
            wins += 1.0
    return {"body": {
        "games": len(results),
        "record": f"{int(wins)}-{len(results) - int(wins) - sum(1 for r in results if r == 0.0)}",
        "win_pct": round(wins / len(results) * 100.0, 1),
        "last_n_form": int(last_n_sum(results, last_n)),
        "best_run": best_run(results),
        "worst_run": worst_run(results),
        "elo_like": round(elo_like(results, float(k)), 1),
        "note": "Form/run/rating kernels compile to native Mojo via mojosub.",
    }}
