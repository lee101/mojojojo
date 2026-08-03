from __future__ import annotations

import os
import tempfile
import time
from pathlib import Path

import numpy as np
import pytest

from vectorworker.engine import SearchKernel, VectorEngine
from vectorworker.model import DIMENSION, FoldedModel


ROOT = Path(__file__).resolve().parents[1]
SIBLING = ROOT.parent
MODEL = SIBLING / "gobed/model/modelint8_512dim.safetensors"
TOKENIZER = SIBLING / "gobed/model/tokenizer.json"
LIBRARY = SIBLING / "mojo-embed/build/libmojo_embed.so"
RUNTIME = SIBLING / "mojosub/.pixi/envs/default/lib"


def test_model_matches_known_wordpiece_shape() -> None:
    model = FoldedModel(MODEL, TOKENIZER)
    tokens = model.tokenize("Machine learning, fast-search!")
    assert tokens[0] == 101 and tokens[-1] == 102
    vector, factor = model.embed("semantic retrieval")
    assert vector.shape == (DIMENSION,) and vector.dtype == np.int8
    assert factor > 0


def test_upsert_search_replace_and_reload() -> None:
    with tempfile.TemporaryDirectory() as directory:
        engine = VectorEngine(directory, MODEL, TOKENIZER, LIBRARY, RUNTIME)
        collection = "vec_abcdefghijk"
        result = engine.upsert(collection, [
            {"id": "python", "text": "Python data science and machine learning", "metadata": {"lang": "python"}},
            {"id": "mojo", "text": "Mojo GPU kernels and systems programming", "metadata": {"lang": "mojo"}},
            {"id": "garden", "text": "Growing tomatoes and herbs in a garden", "metadata": {"lang": "nature"}},
        ])
        assert result == {"inserted": 3, "updated": 0, "vector_count": 3}
        found = engine.search(collection, "fast GPU programming", 2)
        assert found["matches"] and found["matches"][0]["id"] == "mojo"
        assert found["backend"] == "mojo-int8-exact"
        replaced = engine.upsert(collection, [{"id": "mojo", "text": "tomato garden", "metadata": {"changed": True}}])
        assert replaced == {"inserted": 0, "updated": 1, "vector_count": 3}
        reloaded = VectorEngine(directory, MODEL, TOKENIZER, LIBRARY, RUNTIME)
        assert reloaded.search(collection, "tomatoes", 1)["matches"][0]["metadata"] == {"changed": True}
        reloaded.delete(collection)
        assert not (Path(directory) / collection).exists()


def test_benchmark_reports_capability_without_making_latency_claims() -> None:
    with tempfile.TemporaryDirectory() as directory:
        engine = VectorEngine(directory, MODEL, TOKENIZER, LIBRARY, RUNTIME)
        health = engine.health()
        assert health["mojo"] is True and health["gpu"] is False
        result = engine.benchmark(1_000, 2)
        assert result["backend"] == "mojo-int8-exact"
        assert result["p50_ms"] > 0


@pytest.mark.skipif(os.getenv("VECTOR_GPU_TEST") != "1", reason="explicit GPU probe")
def test_cagra_matches_exact_search_and_reuses_its_index(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("VECTOR_GPU_REQUESTED", "1")
    monkeypatch.setenv("VECTOR_CAGRA_MIN_VECTORS", "1000")
    kernel = SearchKernel(None)
    assert kernel.gpu is not None, kernel.gpu_error
    rng = np.random.default_rng(91)
    vectors = rng.integers(-127, 128, (10_000, DIMENSION), dtype=np.int8)
    factors = rng.uniform(0.001, 0.02, len(vectors)).astype(np.float32)
    query_position = 417
    query = vectors[query_position].copy()
    query_factor = np.float32(factors[query_position])

    positions, _ = kernel.search(vectors, factors, query, query_factor, 10)
    exact_scores = (vectors.astype(np.int32) @ query.astype(np.int32)).astype(np.float32)
    exact_scores *= factors * query_factor
    exact = np.argsort(exact_scores)[-10:][::-1]
    assert int(positions[0]) == int(exact[0]) == query_position
    assert len(set(map(int, positions)) & set(map(int, exact))) >= 8

    started = time.perf_counter()
    positions, _ = kernel.search(vectors, factors, query, query_factor, 10)
    warm_ms = (time.perf_counter() - started) * 1000
    assert int(positions[0]) == query_position
    assert warm_ms < 100
