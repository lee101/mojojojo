"""Persistent tenant collection engine backed by the Mojo search ABI."""

from __future__ import annotations

import ctypes
import json
import os
import re
import threading
import time
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .model import DIMENSION, FoldedModel


SAFE_ID = re.compile(r"^[A-Za-z0-9_-]{8,96}$")


def _address(array: np.ndarray) -> int:
    if not array.flags.c_contiguous:
        raise ValueError("search arrays must be contiguous")
    return int(array.ctypes.data)


class SearchKernel:
    def __init__(self, library_path: str | Path | None, runtime_dir: str | Path | None = None):
        self.library_path = str(library_path or "")
        self.library = None
        self.name = "numpy-exact"
        self.error = ""
        self.gpu = None
        self.gpu_error = ""
        if self.library_path:
            try:
                if runtime_dir:
                    runtime = Path(runtime_dir)
                    # A Mojo shared library records these by soname. Preloading
                    # them also works in a minimal systemd environment where
                    # the Pixi activation hook has not amended the loader path.
                    for dependency in ("libKGENCompilerRTShared.so", "libAsyncRTMojoBindings.so"):
                        ctypes.CDLL(str(runtime / dependency), mode=ctypes.RTLD_GLOBAL)
                library = ctypes.CDLL(self.library_path)
                integer = ctypes.c_ssize_t
                library.embed_search_i8.argtypes = [
                    integer, integer, integer, integer, integer, integer, integer,
                    integer, integer, integer, ctypes.c_float,
                ]
                library.embed_search_i8.restype = None
                self.library = library
                self.name = "mojo-int8-exact"
            except OSError as error:
                self.error = str(error)
        if os.getenv("VECTOR_GPU_REQUESTED") == "1":
            try:
                self.gpu = CuVSCagraKernel()
            except Exception as error:  # capability probe, never a fake fallback
                self.gpu_error = str(error)

    def search(
        self, vectors: np.ndarray, factors: np.ndarray, query: np.ndarray,
        query_factor: np.float32, limit: int,
    ) -> tuple[np.ndarray, np.ndarray]:
        count = len(vectors)
        limit = min(limit, count)
        if not count or not limit:
            return np.empty(0, dtype=np.int64), np.empty(0, dtype=np.float32)
        if self.gpu is not None and count >= int(os.getenv("VECTOR_CAGRA_MIN_VECTORS", "1000")):
            return self.gpu.search(vectors, factors, query, query_factor, limit)
        if self.library is None:
            scores = vectors.astype(np.int32) @ query.astype(np.int32)
            scores = scores.astype(np.float32) * factors * query_factor
            positions = np.argsort(scores)[-limit:][::-1].astype(np.int64)
            return positions, scores[positions]
        ids = np.arange(count, dtype=np.int64)
        scratch = np.empty(count, dtype=np.float32)
        output_ids = np.empty(limit, dtype=np.int64)
        output_scores = np.empty(limit, dtype=np.float32)
        self.library.embed_search_i8(
            _address(vectors), _address(factors), _address(ids), _address(query),
            _address(scratch), _address(output_ids), _address(output_scores), count,
            vectors.shape[1], limit, ctypes.c_float(query_factor),
        )
        return output_ids, output_scores

    @property
    def active_name(self) -> str:
        return "cuvs-cagra-gpu" if self.gpu is not None else self.name

    def backend_for(self, count: int) -> str:
        if self.gpu is not None and count >= int(os.getenv("VECTOR_CAGRA_MIN_VECTORS", "1000")):
            return "cuvs-cagra-gpu"
        return self.name


class CuVSCagraKernel:
    """Optional real NVIDIA cuVS CAGRA backend, loaded only on GPU nodes.

    Per-row int8 factors are applied before the index is built, so inner
    product ranking is numerically the same ranking used by the Mojo exact
    kernel. The int8 snapshot remains the portable source representation.
    """

    def __init__(self):
        import cupy as cp
        from cuvs.neighbors import cagra

        if cp.cuda.runtime.getDeviceCount() < 1:
            raise RuntimeError("no CUDA device")
        self.cp = cp
        self.cagra = cagra
        self._indexes = OrderedDict()
        self._cache_size = max(1, int(os.getenv("VECTOR_CAGRA_CACHE_SIZE", "4")))
        self._lock = threading.Lock()
        # Importing a CUDA package is not a capability probe. Build and search
        # a real index before this worker is allowed into the GPU pool.
        rng = np.random.default_rng(7)
        sample = rng.normal(size=(1024, DIMENSION)).astype(np.float32)
        sample /= np.linalg.norm(sample, axis=1, keepdims=True)
        index = cagra.build(cagra.IndexParams(metric="inner_product"), cp.asarray(sample))
        distances, neighbors = cagra.search(cagra.SearchParams(), index, cp.asarray(sample[:1]), 1)
        if int(cp.asnumpy(neighbors)[0, 0]) != 0:
            raise RuntimeError("CAGRA self-retrieval probe failed")
        cp.cuda.Stream.null.synchronize()
        del index, distances, neighbors

    def search(self, vectors, factors, query, query_factor, limit):
        key = (int(vectors.ctypes.data), int(factors.ctypes.data), len(vectors), vectors.shape[1])
        with self._lock:
            cached = self._indexes.pop(key, None)
            if cached is None:
                dataset = vectors.astype(np.float32)
                dataset *= factors[:, None]
                device_dataset = self.cp.asarray(dataset)
                index = self.cagra.build(
                    self.cagra.IndexParams(metric="inner_product"), device_dataset,
                )
                cached = (index, device_dataset)
            self._indexes[key] = cached
            while len(self._indexes) > self._cache_size:
                self._indexes.popitem(last=False)
            index, _ = cached
            device_query = self.cp.asarray(
                (query.astype(np.float32) * query_factor)[None, :]
            )
            distances, neighbors = self.cagra.search(
                self.cagra.SearchParams(), index, device_query, limit,
            )
            positions = self.cp.asnumpy(neighbors)[0].astype(np.int64, copy=False)
            scores = self.cp.asnumpy(distances)[0].astype(np.float32, copy=False)
            return np.ascontiguousarray(positions), np.ascontiguousarray(scores)


@dataclass
class Collection:
    ids: list[str]
    metadata: list[dict[str, Any]]
    vectors: np.ndarray
    factors: np.ndarray


class VectorEngine:
    def __init__(
        self, data_dir: str | Path, model_path: str | Path,
        tokenizer_path: str | Path, library_path: str | Path | None,
        runtime_dir: str | Path | None = None,
    ):
        self.root = Path(data_dir)
        self.root.mkdir(parents=True, exist_ok=True)
        self.model = FoldedModel(model_path, tokenizer_path)
        self.kernel = SearchKernel(library_path, runtime_dir)
        self._collections: dict[str, Collection] = {}
        self._locks: dict[str, threading.RLock] = {}
        self._master = threading.Lock()
        self.started_at = time.time()

    def _path(self, collection_id: str) -> Path:
        if not SAFE_ID.fullmatch(collection_id):
            raise ValueError("invalid collection id")
        return self.root / collection_id

    def _lock(self, collection_id: str) -> threading.RLock:
        with self._master:
            return self._locks.setdefault(collection_id, threading.RLock())

    def _load(self, collection_id: str) -> Collection:
        cached = self._collections.get(collection_id)
        if cached is not None:
            return cached
        path = self._path(collection_id) / "snapshot.npz"
        if not path.exists():
            collection = Collection([], [], np.empty((0, DIMENSION), dtype=np.int8), np.empty(0, dtype=np.float32))
        else:
            with np.load(path, allow_pickle=False) as snapshot:
                ids = json.loads(str(snapshot["ids_json"]))
                metadata = json.loads(str(snapshot["metadata_json"]))
                collection = Collection(
                    ids, metadata, np.ascontiguousarray(snapshot["vectors"], dtype=np.int8),
                    np.ascontiguousarray(snapshot["factors"], dtype=np.float32),
                )
        self._collections[collection_id] = collection
        return collection

    def _save(self, collection_id: str, collection: Collection) -> None:
        directory = self._path(collection_id)
        directory.mkdir(parents=True, exist_ok=True)
        temporary = directory / f"snapshot-{os.getpid()}-{threading.get_ident()}.npz"
        with temporary.open("wb") as handle:
            np.savez(
                handle, vectors=collection.vectors, factors=collection.factors,
                ids_json=np.asarray(json.dumps(collection.ids, separators=(",", ":"))),
                metadata_json=np.asarray(json.dumps(collection.metadata, separators=(",", ":"))),
            )
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, directory / "snapshot.npz")

    def upsert(self, collection_id: str, documents: list[dict[str, Any]]) -> dict[str, Any]:
        if not documents:
            raise ValueError("documents are required")
        with self._lock(collection_id):
            current = self._load(collection_id)
            by_id = {external_id: index for index, external_id in enumerate(current.ids)}
            texts = [document["text"] for document in documents]
            incoming_vectors, incoming_factors = self.model.embed_many(texts)
            ids = list(current.ids)
            metadata = list(current.metadata)
            vectors = current.vectors.copy()
            factors = current.factors.copy()
            inserted = updated = 0
            for offset, document in enumerate(documents):
                external_id = document["id"]
                document_metadata = document.get("metadata") or {}
                position = by_id.get(external_id)
                if position is None:
                    by_id[external_id] = len(ids)
                    ids.append(external_id)
                    metadata.append(document_metadata)
                    vectors = np.vstack((vectors, incoming_vectors[offset:offset + 1]))
                    factors = np.append(factors, incoming_factors[offset])
                    inserted += 1
                else:
                    vectors[position] = incoming_vectors[offset]
                    factors[position] = incoming_factors[offset]
                    metadata[position] = document_metadata
                    updated += 1
            collection = Collection(ids, metadata, np.ascontiguousarray(vectors), np.ascontiguousarray(factors))
            self._save(collection_id, collection)
            self._collections[collection_id] = collection
            return {"inserted": inserted, "updated": updated, "vector_count": len(ids)}

    def search(self, collection_id: str, query_text: str, limit: int) -> dict[str, Any]:
        started = time.perf_counter()
        with self._lock(collection_id):
            collection = self._load(collection_id)
            query, factor = self.model.embed(query_text)
            positions, scores = self.kernel.search(collection.vectors, collection.factors, query, factor, limit)
            matches = [
                {"id": collection.ids[int(position)], "score": round(float(score), 6),
                 "metadata": collection.metadata[int(position)]}
                for position, score in zip(positions, scores)
            ]
        return {"matches": matches, "vector_count": len(collection.ids),
                "backend": self.kernel.backend_for(len(collection.ids)),
                "latency_ms": round((time.perf_counter() - started) * 1000, 3)}

    def delete(self, collection_id: str) -> None:
        with self._lock(collection_id):
            self._collections.pop(collection_id, None)
            directory = self._path(collection_id)
            snapshot = directory / "snapshot.npz"
            if snapshot.exists():
                snapshot.unlink()
            if directory.exists():
                directory.rmdir()

    def benchmark(self, count: int = 10_000, iterations: int = 10) -> dict[str, Any]:
        rng = np.random.default_rng(42)
        vectors = rng.integers(-127, 128, (count, DIMENSION), dtype=np.int8)
        factors = rng.uniform(0.001, 0.02, count).astype(np.float32)
        query = rng.integers(-127, 128, DIMENSION, dtype=np.int8)
        # Build or hydrate the index before measuring query latency. Indexing
        # is an upsert concern and must not be mixed into the retrieval sample.
        self.kernel.search(vectors, factors, query, np.float32(0.01), 10)
        samples = []
        for _ in range(max(1, iterations)):
            started = time.perf_counter()
            self.kernel.search(vectors, factors, query, np.float32(0.01), 10)
            samples.append((time.perf_counter() - started) * 1000)
        return {"backend": self.kernel.backend_for(count), "vectors": count, "dimension": DIMENSION,
                "p50_ms": round(float(np.percentile(samples, 50)), 3),
                "p95_ms": round(float(np.percentile(samples, 95)), 3), "iterations": len(samples)}

    def health(self) -> dict[str, Any]:
        return {"ok": True, "service": "mojojojo-vector-worker", "dimension": DIMENSION,
                "model": "folded-static-int8-512", "search_backend": self.kernel.active_name,
                "mojo": self.kernel.library is not None, "gpu": self.kernel.gpu is not None,
                "gpu_backend": "cuvs-cagra" if self.kernel.gpu is not None else "unavailable",
                "gpu_error": self.kernel.gpu_error, "kernel_error": self.kernel.error,
                "loaded_collections": len(self._collections),
                "uptime_seconds": round(time.time() - self.started_at, 1)}
