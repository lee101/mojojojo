import math
from pathlib import Path
import types
import unittest

source = Path(__file__).resolve().parents[1] / "examples" / "foundry-numeric" / "functions" / "model.py"
model = types.ModuleType("foundry_model")
model.__file__ = str(source)
exec(compile(source.read_text(), str(source), "exec"), model.__dict__)


class ModelTests(unittest.TestCase):
    def fixture(self):
        return {"operation": "train", "dataset_revision": "synthetic-v1", "data_license": "fixture",
                "train": {"rows": [[0, 0], [1, 1], [9, 9], [10, 10]], "labels": [0, 0, 1, 1]},
                "validation": {"rows": [[2, 2], [8, 8]], "labels": [0, 1]}}

    def test_artifact_roundtrip(self):
        body = self.fixture()
        result = model.handler({"body": body})
        artifact = result["body"]
        self.assertIn("attachment", result["headers"]["Content-Disposition"])
        self.assertEqual(artifact["validation_accuracy"], 1)
        self.assertEqual(artifact["model_sha256"], model.digest(artifact["model"]))
        self.assertEqual(artifact["split_sha256"]["train"], model.digest(body["train"]))
        self.assertEqual(len(artifact["source_sha256"]), 64)
        self.assertTrue(artifact["baseline_only"])
        prediction = model.handler({"body": {"operation": "infer", "model": artifact["model"], "rows": [[3, 3], [7, 7]]}})
        self.assertEqual(prediction["body"]["predictions"], [0, 1])

    def test_validation_does_not_change_model(self):
        body = self.fixture()
        a = model.handler({"body": body})["body"]
        body["validation"] = {"rows": [[10, 10]], "labels": [0]}
        b = model.handler({"body": body})["body"]
        self.assertEqual(a["model"], b["model"])
        self.assertNotEqual(a["split_sha256"]["validation"], b["split_sha256"]["validation"])

    def test_invalid_training_input(self):
        for field, value in [("rows", []), ("rows", [[math.nan, 0]]), ("rows", [[True, 0]]),
                             ("rows", [[1e7, 0]]), ("rows", [[0, 0]] * 513),
                             ("labels", [0, 0, 0, 0]), ("labels", [0, 1])]:
            with self.subTest(field=field, value=str(value)[:30]):
                body = self.fixture()
                body["train"][field] = value
                self.assertEqual(model.handler({"body": body})["status"], 400)
        for body in (None, {}, {"operation": "train"}, {"operation": "exec"}):
            self.assertEqual(model.handler({"body": body})["status"], 400)

    def test_untrusted_models_rejected(self):
        for bad in (None, "pickle", {}, {"schema": "foundry-centroid-v1", "centers": [[0], [1]], "scale": [0]}):
            self.assertEqual(model.handler({"body": {"operation": "infer", "model": bad, "rows": [[1]]}})["status"], 400)


if __name__ == "__main__":
    unittest.main()
