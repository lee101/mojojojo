# Afford It

"Can I buy this?" calculator. Months to save, amortised monthly cost,
and a stress test: balance after N months at 70% income. Three scalar
kernels, all compile via mojosub.

```sh
mojojojo project "Afford It" --runtime app --category tool
mojojojo publish PROJECT_ID examples/afford-it
mojojojo submit PROJECT_ID
```

`POST /api/afford` with `{price, savings, monthly_income,
monthly_essential, monthly_saving, months}` returns months-to-save,
monthly cost, stress balance, and a plain-English verdict. Inputs are
range-checked; the scenario is encoded in the URL hash for sharing.

## Perf notes

`months_to_save` is a bounded `while` — the shape that most rewards
compilation relative to CPython's per-iteration overhead. `stress_balance`
is a fixed-trip loop. Both are pure scalar reductions with no list
building, which is exactly what the transpiler subset allows. The
verdict string is assembled in the handler (strings are `str` literals
to the filter — correctly rejected from kernels).
