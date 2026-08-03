"""Folded static text embeddings compatible with gobed's 512-dimension model."""

from __future__ import annotations

import json
import struct
from pathlib import Path

import numpy as np


DIMENSION = 512
MAX_TOKENS = 512


class FoldedModel:
    """Memory-map the small static model and reproduce gobed's WordPiece fold."""

    def __init__(self, model_path: str | Path, tokenizer_path: str | Path):
        model_path = Path(model_path)
        tokenizer_path = Path(tokenizer_path)
        with model_path.open("rb") as handle:
            header_length = struct.unpack("<Q", handle.read(8))[0]
            header = json.loads(handle.read(header_length))
        base = 8 + header_length
        weight = header["embeddings.weight"]
        scales = header["embeddings.scales"]
        if weight["dtype"] != "I8" or weight["shape"][1] != DIMENSION:
            raise ValueError("the folded model must contain 512-dimension int8 weights")
        self.weights = np.memmap(
            model_path, dtype=np.int8, mode="r", offset=base + weight["data_offsets"][0],
            shape=tuple(weight["shape"]),
        )
        self.scales = np.memmap(
            model_path, dtype="<f4", mode="r", offset=base + scales["data_offsets"][0],
            shape=tuple(scales["shape"]),
        )
        tokenizer = json.loads(tokenizer_path.read_text(encoding="utf-8"))
        self.vocab: dict[str, int] = tokenizer["model"]["vocab"]
        self.cls = self.vocab["[CLS]"]
        self.sep = self.vocab["[SEP]"]
        self.unk = self.vocab["[UNK]"]

    @staticmethod
    def _words(text: str) -> list[str]:
        words: list[str] = []
        current: list[str] = []
        for character in text.lower():
            if character.isalpha() or character.isdigit() or character in "_'":
                current.append(character)
            else:
                if current:
                    words.append("".join(current))
                    current.clear()
                if not character.isspace():
                    words.append(character)
        if current:
            words.append("".join(current))
        return words

    def tokenize(self, text: str) -> list[int]:
        tokens = [self.cls]
        for word in self._words(text):
            direct = self.vocab.get(word)
            if direct is not None:
                tokens.append(direct)
                continue
            pieces: list[int] = []
            start = 0
            while start < len(word):
                match = None
                match_end = start
                for end in range(len(word), start, -1):
                    piece = word[start:end]
                    if start:
                        piece = "##" + piece
                    candidate = self.vocab.get(piece)
                    if candidate is not None:
                        match, match_end = candidate, end
                        break
                if match is None:
                    pieces = [self.unk]
                    break
                pieces.append(match)
                start = match_end
            tokens.extend(pieces)
            if len(tokens) >= MAX_TOKENS - 1:
                break
        tokens.append(self.sep)
        return tokens[:MAX_TOKENS]

    def embed_float(self, text: str) -> np.ndarray:
        token_ids = np.asarray(self.tokenize(text), dtype=np.int64)
        rows = self.weights[token_ids].astype(np.float32)
        rows *= self.scales[token_ids, None]
        return np.ascontiguousarray(rows.mean(axis=0), dtype=np.float32)

    @staticmethod
    def quantize(vector: np.ndarray) -> tuple[np.ndarray, np.float32]:
        vector = np.asarray(vector, dtype=np.float32)
        peak = float(np.max(np.abs(vector))) if vector.size else 0.0
        scale = np.float32(peak / 127.0 if peak else 1.0)
        scaled = vector / scale
        rounded = np.where(scaled >= 0, scaled + 0.5, scaled - 0.5)
        return np.ascontiguousarray(np.clip(rounded, -127, 127).astype(np.int8)), scale

    def embed(self, text: str) -> tuple[np.ndarray, np.float32]:
        return self.quantize(self.embed_float(text))

    def embed_many(self, texts: list[str]) -> tuple[np.ndarray, np.ndarray]:
        vectors = np.empty((len(texts), DIMENSION), dtype=np.int8)
        factors = np.empty(len(texts), dtype=np.float32)
        for index, text in enumerate(texts):
            vectors[index], factors[index] = self.embed(text)
        return vectors, factors

