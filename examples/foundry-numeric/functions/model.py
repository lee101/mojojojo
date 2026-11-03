"""Bounded numeric centroid baseline; a deployment contract demo, not zero-shot AI."""
import hashlib
import json
import math
import platform
import time
from pathlib import Path


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def digest(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def matrix(value, width=None):
    if not isinstance(value, list) or not 1 <= len(value) <= 512:
        raise ValueError("provide 1..512 numeric rows")
    if not isinstance(value[0], list):
        raise ValueError("rows must be lists")
    width = width if width is not None else len(value[0])
    if not 1 <= width <= 32:
        raise ValueError("provide 1..32 features")
    for row in value:
        if not isinstance(row, list) or len(row) != width:
            raise ValueError("inconsistent row width")
        if any(type(x) not in (int, float) or not math.isfinite(x) or abs(x) > 1e6 for x in row):
            raise ValueError("features must be finite numbers with absolute value <= 1e6")
    return value


def labels(value, count):
    if not isinstance(value, list) or len(value) != count or any(type(x) is not int or x not in (0, 1) for x in value):
        raise ValueError("provide one binary integer label per row")
    return value


def predict(model, rows):
    if not isinstance(model, dict) or model.get("schema") != "foundry-centroid-v1":
        raise ValueError("unsupported model schema")
    centers = matrix(model.get("centers"))
    if len(centers) != 2:
        raise ValueError("model requires two centers")
    width = len(centers[0])
    scale = matrix([model.get("scale")], width)[0]
    if any(x < 1e-6 for x in scale):
        raise ValueError("invalid feature scale")
    rows = matrix(rows, width)
    return [int(sum(((x - centers[1][j]) / scale[j]) ** 2 for j, x in enumerate(row)) <
                sum(((x - centers[0][j]) / scale[j]) ** 2 for j, x in enumerate(row))) for row in rows]


def handler(request):
    started = time.monotonic()
    try:
        body = request.get("body")
        if not isinstance(body, dict):
            raise ValueError("JSON object body required")
        if body.get("operation") == "infer":
            return {"body": {"predictions": predict(body.get("model"), body.get("rows")),
                             "baseline_only": True}}
        if body.get("operation") != "train":
            raise ValueError("operation must be train or infer")
        for field in ("dataset_revision", "data_license"):
            if not isinstance(body.get(field), str) or not 1 <= len(body[field]) <= 200:
                raise ValueError(field + " must be a nonempty string up to 200 characters")
        train, validation = body.get("train"), body.get("validation")
        if not isinstance(train, dict) or not isinstance(validation, dict):
            raise ValueError("explicit immutable train and validation splits required")
        rows = matrix(train.get("rows"))
        targets = labels(train.get("labels"), len(rows))
        if set(targets) != {0, 1}:
            raise ValueError("training split must contain both classes")
        width = len(rows[0])
        val_rows = matrix(validation.get("rows"), width)
        val_labels = labels(validation.get("labels"), len(val_rows))
        centers = [[sum(row[j] for row, label in zip(rows, targets) if label == c) / targets.count(c)
                    for j in range(width)] for c in (0, 1)]
        means = [sum(row[j] for row in rows) / len(rows) for j in range(width)]
        scale = [max(1e-6, math.sqrt(sum((row[j] - means[j]) ** 2 for row in rows) / len(rows)))
                 for j in range(width)]
        model = {"schema": "foundry-centroid-v1", "centers": centers, "scale": scale}
        predictions = predict(model, val_rows)
        artifact = {"model": model, "model_sha256": digest(model),
                    "dataset_revision": body["dataset_revision"], "data_license": body["data_license"],
                    "split_sha256": {"train": digest(train), "validation": digest(validation)},
                    "seed": 0, "algorithm": "deterministic nearest centroid, train-only standardization",
                    "python_version": platform.python_version(),
                    "source_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                    "validation_accuracy": sum(a == b for a, b in zip(predictions, val_labels)) / len(val_labels),
                    "training_seconds": time.monotonic() - started, "baseline_only": True,
                    "limitations": "No held-out evaluation or zero-shot capability. No production or control validation."}
        return {"headers": {"Content-Type": "application/json", "Cache-Control": "no-store",
                            "Content-Disposition": 'attachment; filename="model.json"'}, "body": artifact}
    except (ValueError, TypeError, OverflowError) as exc:
        return {"status": 400, "body": {"error": str(exc)}}
