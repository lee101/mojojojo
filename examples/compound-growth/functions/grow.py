"""Compound Growth kernels: retirement / savings projection, compiled to Mojo.

The model is three scalar reductions — each is a tight loop + annotations,
exactly what the accel filter offers to mojosub (`runner/accel.py`
`eligible()`). No lists, no tuples, no method calls. The handler validates
inputs and stays on CPython.

Request: {"principal": 10000, "monthly": 500, "annual_return": 0.07,
          "years": 30, "annual_raise": 0.03, "inflation": 0.025}
Response: {final, total_contrib, growth, crossover_year, real_final}
"""
from __future__ import annotations

import math


def project_final(principal: float, monthly: float, annual_return: float,
                  years: int, annual_raise: float) -> float:
    """End balance after `years` of monthly compounding with annual
    contribution raises. Scalar loop — the canonical compile shape."""
    balance = principal
    monthly_rate = math.pow(1.0 + annual_return, 1.0 / 12.0) - 1.0
    contrib = monthly
    for y in range(years):
        for m in range(12):
            balance = balance * (1.0 + monthly_rate) + contrib
        contrib = contrib * (1.0 + annual_raise)
    return balance


def total_contributions(principal: float, monthly: float, years: int,
                        annual_raise: float) -> float:
    """Sum of everything paid in (principal + contributions)."""
    total = principal
    contrib = monthly
    for y in range(years):
        for m in range(12):
            total += contrib
        contrib = contrib * (1.0 + annual_raise)
    return total


def crossover_year(principal: float, monthly: float, annual_return: float,
                   years: int, annual_raise: float) -> int:
    """First year where growth > contributions (the 'money making money'
    moment). Returns `years` if it never crosses within the horizon."""
    balance = principal
    paid = principal
    monthly_rate = math.pow(1.0 + annual_return, 1.0 / 12.0) - 1.0
    contrib = monthly
    for y in range(years):
        for m in range(12):
            balance = balance * (1.0 + monthly_rate) + contrib
            paid += contrib
        contrib = contrib * (1.0 + annual_raise)
        if balance - paid > paid - principal:
            return y + 1
    return years


def handler(request):
    body = request.get("body") or {}
    try:
        principal = float(body.get("principal", 10000.0))
        monthly = float(body.get("monthly", 500.0))
        annual_return = float(body.get("annual_return", 0.07))
        years = int(body.get("years", 30))
        annual_raise = float(body.get("annual_raise", 0.03))
        inflation = float(body.get("inflation", 0.025))
    except (TypeError, ValueError):
        return {"status": 400, "body": {"error": "Numeric fields required."}}
    if not all(math.isfinite(x) for x in (principal, monthly, annual_return,
                                          annual_raise, inflation)):
        return {"status": 400, "body": {"error": "Values must be finite."}}
    if not (0.0 <= principal <= 1e9 and 0.0 <= monthly <= 1e6
            and -0.95 <= annual_return <= 0.50 and 0 <= years <= 100
            and -0.5 <= annual_raise <= 0.5 and -0.1 <= inflation <= 0.3):
        return {"status": 400, "body": {"error": "Value out of range."}}
    final = project_final(principal, monthly, annual_return, years, annual_raise)
    paid = total_contributions(principal, monthly, years, annual_raise)
    real_final = final * math.pow(1.0 / (1.0 + inflation), float(years))
    return {"body": {
        "final": round(final, 2),
        "total_contrib": round(paid, 2),
        "growth": round(final - paid, 2),
        "crossover_year": crossover_year(principal, monthly, annual_return,
                                         years, annual_raise),
        "real_final": round(real_final, 2),
        "note": "Projection kernels compile to native Mojo via mojosub; "
                "numbers are verified against CPython on first call.",
    }}
