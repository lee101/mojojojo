"""Compare two bounded image previews using perceptual hashes."""

import base64
import binascii
import io

from PIL import Image, UnidentifiedImageError

try:
    import mojo_imagehash as imagehash
    ENGINE = "Mojo"
except (ImportError, OSError, RuntimeError):
    import imagehash
    ENGINE = "Python"


def _image(value):
    encoded = str(value).split(",", 1)[-1]
    if len(encoded) > 350_000:
        raise ValueError("Preview is too large.")
    raw = base64.b64decode(encoded, validate=True)
    if len(raw) > 256_000:
        raise ValueError("Preview is too large.")
    image = Image.open(io.BytesIO(raw))
    if image.width * image.height > 4_000_000:
        raise ValueError("Preview dimensions are too large.")
    image.load()
    return image.convert("RGB")


def handler(request):
    body = request.get("body") or {}
    try:
        left, right = _image(body.get("left", "")), _image(body.get("right", ""))
        left_hash, right_hash = imagehash.phash(left), imagehash.phash(right)
    except (ValueError, TypeError, binascii.Error, UnidentifiedImageError,
            Image.DecompressionBombError) as exc:
        return {"status": 400, "body": {"error": str(exc) or "Invalid image."}}
    distance = int(left_hash - right_hash)
    verdict = "same" if distance <= 2 else "near" if distance <= 10 else "different"
    return {"body": {"left_hash": str(left_hash), "right_hash": str(right_hash),
                      "distance": distance, "verdict": verdict, "engine": ENGINE}}
