import json
from pathlib import Path
import numpy as np

LEAF = 0xFFFF
MODELS = Path(__file__).resolve().parent.parent / "models"
_CACHE = {}


def load(name):
    if name not in _CACHE:
        if name not in {p.stem for p in MODELS.glob("*.npz")}:
            raise ValueError("unknown model")
        a = np.load(MODELS / (name + ".npz"), allow_pickle=False)
        arr = {k: a[k] for k in a.files}
        meta = json.loads(str(arr["meta"]))
        _CACHE[name] = (arr, meta, [{c: i for i, c in enumerate(cs)} for cs in meta["cats"]])
    return _CACHE[name]


def design(rows, arr, meta, maps):
    n, nn = len(rows), len(meta["num_cols"])
    x = np.zeros((n, meta["n_feat"]), np.float32)
    for j, c in enumerate(meta["num_cols"]):
        for r, row in enumerate(rows):
            v = row.get(c)
            v = float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else np.nan
            x[r, j] = arr["med"][j] if v != v else v
    for i, c in enumerate(meta["cat_cols"]):
        cm, fill = arr[f"cmap{i}"], str(arr["fill"][i])
        for r, row in enumerate(rows):
            v = row.get(c)
            code = maps[i].get(fill if v is None else str(v), -1)
            col = int(cm[code]) if code >= 0 else meta["unk"][i]
            if col >= 0:
                x[r, meta["cat_base"][i] + col] = 1.0
    return x


def forest_sum(nodes, off, x):
    thr, meta = nodes[0::2].view(np.float32), nodes[1::2]
    rows = np.arange(len(x))
    out = np.zeros(len(x))
    for base in off:
        idx = np.zeros(len(x), np.int64)
        while True:
            m = meta[base + idx]
            f = (m >> 16).astype(np.int64)
            leaf = f == LEAF
            if leaf.all():
                break
            go = x[rows, np.where(leaf, 0, f)] > thr[base + idx]
            idx = np.where(leaf, idx, (m & 0xFFFF).astype(np.int64) + go)
        out += thr[base + idx]
    return out


def predict(name, rows):
    arr, meta, maps = load(name)
    x = design(rows, arr, meta, maps)
    acc = np.zeros(len(x))
    for k, h in enumerate(meta["heads"]):
        p = f"h{k}_"
        if h["kind"] == "hgb":
            acc += 1 / (1 + np.exp(-(h["bias"] + forest_sum(arr[p + "nodes"], arr[p + "off"], x))))
        elif h["kind"] == "et":
            acc += forest_sum(arr[p + "nodes"], arr[p + "off"], x) / h["n_trees"]
        else:
            acc += 1 / (1 + np.exp(-(x.astype(np.float64) @ arr[p + "w"] + h["b"])))
    return (acc / len(meta["heads"])).tolist()


def handler(request):
    try:
        body = request.get("body")
        if not isinstance(body, dict):
            raise ValueError("JSON object body required")
        if body.get("operation") == "models":
            return {"body": {"models": sorted(p.stem for p in MODELS.glob("*.npz"))}}
        rows = body.get("rows")
        if not isinstance(rows, list) or not 1 <= len(rows) <= 1024 or any(not isinstance(r, dict) for r in rows):
            raise ValueError("rows must be 1..1024 objects keyed by feature name")
        name = body.get("model")
        if not isinstance(name, str):
            raise ValueError("model name required")
        return {"body": {"model": name, "probabilities": predict(name, rows)}}
    except (ValueError, TypeError, OverflowError) as exc:
        return {"status": 400, "body": {"error": str(exc)}}
