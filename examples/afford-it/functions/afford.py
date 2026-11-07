"""Afford It kernels: can I buy X without breaking the budget?

Three scalar kernels, all compile via mojosub:
- `months_to_save` — how long until the goal is funded (loop with a break)
- `monthly_cost` — amortised monthly cost of a purchase over its life
- `stress_test` — balance after N months of an emergency (loop)

The handler validates and stays on CPython. No lists, no strings in kernels.

Request: {"price": 800, "savings": 200, "monthly_income": 3000,
          "monthly_essential": 2200, "monthly_saving": 300,
          "months": 12}
Response: {months_to_save, monthly_cost, stress_balance, verdict}
"""
from __future__ import annotations

import math


def months_to_save(price: float, savings: float, monthly_saving: float) -> int:
    """Months until savings + n*monthly_saving covers price. -1 if never."""
    if price <= savings:
        return 0
    if monthly_saving <= 0.0:
        return -1
    balance = savings
    months = 0
    while balance < price:
        balance += monthly_saving
        months += 1
        if months > 1200:
            return -1
    return months


def monthly_cost(price: float, months: float, residual: float) -> float:
    """Amortised monthly cost: (price - residual) / months."""
    if months <= 0.0:
        return price
    return (price - residual) / months


def stress_balance(income: float, essential: float, balance: float,
                   months: int) -> float:
    """Balance after `months` with a 30% income drop. Pure loop."""
    bal = balance
    stressed_income = income * 0.7
    for i in range(months):
        bal += stressed_income - essential
    return bal


def handler(request):
    body = request.get("body") or {}
    try:
        price = float(body.get("price", 800.0))
        savings = float(body.get("savings", 200.0))
        income = float(body.get("monthly_income", 3000.0))
        essential = float(body.get("monthly_essential", 2200.0))
        saving = float(body.get("monthly_saving", 300.0))
        residual = float(body.get("residual", 0.0))
        months = int(body.get("months", 12))
    except (TypeError, ValueError):
        return {"status": 400, "body": {"error": "Numeric fields required."}}
    if not all(math.isfinite(x) for x in (price, savings, income, essential, saving, residual)):
        return {"status": 400, "body": {"error": "Values must be finite."}}
    if not (0.0 <= price <= 1e7 and 0.0 <= savings <= 1e7 and 0.0 <= income <= 1e6
            and 0.0 <= essential <= 1e6 and 0.0 <= saving <= 1e6
            and 0.0 <= residual <= price and 1 <= months <= 120):
        return {"status": 400, "body": {"error": "Value out of range."}}
    wait = months_to_save(price, savings, saving)
    cost = monthly_cost(price, max(1.0, float(months)), residual)
    stressed = stress_balance(income, essential, savings, months)
    if wait == -1:
        verdict = "Not with this saving rate — raise income or cut the price."
    elif wait == 0:
        verdict = "You already have it covered."
    elif stressed < 0.0:
        verdict = f"Save {wait} months first — a 30% income drop for {months} months would wipe you out."
    else:
        verdict = f"Doable in {wait} months, and you survive a 30% income drop."
    return {"body": {
        "months_to_save": wait,
        "monthly_cost": round(cost, 2),
        "stress_balance": round(stressed, 2),
        "verdict": verdict,
        "note": "Affordability kernels compile to native Mojo via mojosub.",
    }}
