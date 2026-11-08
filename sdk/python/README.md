# mojojojo (Python SDK)

Run Python on [mojojojo.cc](https://mojojojo.cc), billed by the
millisecond. Add credits to start, with a $1 bonus on your first top-up.

```bash
pip install mojojojo
export MOJOJOJO_API_KEY=sk-mj-...   # https://mojojojo.cc/keys
```

```python
from mojojojo import run

r = run("print(sum(range(10)))")
r.stdout            # '45\n'
r.charge.billed_ms  # 41.8
r.charge.usd        # 4.18e-06
```

Imports are installed for you:

```python
r = run("""
    import pandas as pd
    print(pd.DataFrame({"a": [1, 2, 3]}).sum().to_dict())
""")
r.auto_installed    # ['pandas']
```

Ship a function instead of a string:

```python
from mojojojo import remote

@remote(packages=["numpy"])
def sharpe(returns, window):
    import numpy as np
    a = np.array(returns)
    r = a[-window:]
    return float(r.mean() / r.std())

sharpe(prices, 64)   # runs remotely, returns a float
sharpe.local(prices, 64)   # …or right here
```

Arguments and return values are JSON; closures and module globals are not
shipped. A function that needs those wants to be a script.

```bash
mojojojo run script.py --packages numpy   # the CLI
mojojojo credits                          # balance, what it buys, where to top up
mojojojo pricing
```

Make compilation an explicit release stage when first-request latency matters:

```bash
mojojojo precompile backtest.py --json
mojojojo precompile main.mojo --language mojo --json
```

```python
prepared = client.precompile(open("backtest.py").read())
print(prepared.ready, prepared.kernels, prepared.message)
```

Python preparation executes a representative workload because argument and
return types are part of a compiled kernel. Execution is billed normally;
environment builds, transpilation, and Mojo compilation are not.

Multi-file Mojo and cloud hosting use the same client and API key:

```bash
mojojojo run --project-dir src --entrypoint main.mojo
mojojojo project "Fractal explorer" --runtime static
mojojojo projects
mojojojo deploy PROJECT_ID fractal.zip
mojojojo project "My converter" --runtime app --category tool
mojojojo publish PROJECT_ID ./my-converter
mojojojo submit PROJECT_ID
mojojojo upload tiny-style.onnx --project PROJECT_ID
mojojojo storage
```

```python
client.run(language="mojo", entrypoint="main.mojo", files={
    "main.mojo": open("src/main.mojo").read(),
    "palette.mojo": open("src/palette.mojo").read(),
})
```

`publish` saves the directory as an editable Studio workspace, validates its
`mojojojo.json`, and creates an immutable deployment. App manifests can place
metered Python or Mojo functions beside the frontend; the main repository's
`examples/json-to-csv` and `examples/mojo-serverless` are complete starters.

Managed JSON data and vector search share the same identity:

```python
database = client.create_database("Customers", vector_project_id="vec_PROJECT")
client.data_upsert(database["id"], "customer:123", {
    "name": "Ada", "plan": "starter"
}, search_text="Ada builds services with Mojo")

records = client.data_records(database["id"])
client.data_delete(database["id"], "customer:123")
```

JSON remains authoritative in Postgres; optional search text is mirrored into
the linked mojo-embed vector project and can be rebuilt on a new worker.

MIT.
