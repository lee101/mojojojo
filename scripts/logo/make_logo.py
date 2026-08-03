#!/usr/bin/env python3
"""Generate the mojojojo mascot, cut it out, and ship a small WebP.

Three stages, and the whole point of the script is that they overlap:

  1. generate N candidates on the OmniServe gateway (z-image-turbo)
  2. segment the chosen one with BiRefNet and keep the alpha
  3. encode WebP q85 at the sizes the site actually uses

Stage 2's model load is the long pole on a cold start (several seconds of
weights), and it does not depend on stage 1's output — so it starts in a thread
the moment the script does, in parallel with generation. Candidates are
requested concurrently over one keep-alive connection for the same reason.
Every stage prints what it cost, because "make the logo faster" is not a thing
you can do without a breakdown.

  python scripts/logo/make_logo.py --candidates 3 --out static/logo.webp
"""

from __future__ import annotations

import argparse
import base64
import io
import json
import os
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

GATEWAY = os.environ.get("OMNISERVE_URL", "http://127.0.0.1:8791")
SECRET = os.environ.get("OMNISERVE_SECRET", "cutedsl2024")
BIREFNET_URL = os.environ.get("BIREFNET_URL", "http://127.0.0.1:9094")
BIREFNET_MODEL = os.environ.get("BIREFNET_MODEL", "ZhengPeng7/BiRefNet")

PROMPT = os.environ.get("MOJOJOJO_LOGO_PROMPT", (
    "mascot logo of a mischievous cartoon monkey villain, electric purple fur, "
    "huge toothy grin, big round eyes, arms crossed, wearing a tiny black hoodie, "
    "bold clean vector illustration, thick outlines, flat vivid colors, "
    "neon green rim light, centered full body, plain flat white background, "
    "sticker art, high contrast, no text"
))
NEGATIVE = "text, watermark, blurry, photo, realistic, extra limbs, cluttered background"


class Timer:
    """Records how long each named stage took, for the summary at the end."""

    def __init__(self) -> None:
        self.marks: list[tuple[str, float]] = []
        self.t0 = time.perf_counter()

    def mark(self, name: str, seconds: float) -> None:
        self.marks.append((name, seconds))
        print(f"  {name:<28} {seconds*1000:8.0f} ms", flush=True)

    def report(self) -> None:
        total = time.perf_counter() - self.t0
        serial = sum(s for _, s in self.marks)
        print(f"  {'wall clock':<28} {total*1000:8.0f} ms")
        print(f"  {'(serial would have been':<28} {serial*1000:8.0f} ms)")


def post_json(url: str, payload: dict, timeout: float = 600.0) -> dict:
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(), method="POST",
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {SECRET}"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


def generate(prompt: str, size: int, seed: int | None = None) -> bytes:
    payload = {"prompt": prompt, "n": 1, "size": f"{size}x{size}",
               "negative_prompt": NEGATIVE}
    if seed is not None:
        payload["seed"] = seed
    data = post_json(f"{GATEWAY}/v1/images/generations", payload)
    item = data["data"][0]
    if item.get("b64_json"):
        return base64.b64decode(item["b64_json"])
    with urllib.request.urlopen(item["url"], timeout=120) as resp:
        return resp.read()


# --- background removal -------------------------------------------------

class Segmenter:
    """BiRefNet, wherever it can be found: HTTP worker first, in-process second.

    The worker (if someone already has it running) costs nothing to use and
    keeps the weights off our heap. Loading in-process is the fallback, and it
    is why this class starts warming as soon as the script does.
    """

    def __init__(self) -> None:
        self.mode: str | None = None
        self.model = None
        self.transform = None
        self.device = None
        self.error: str | None = None
        self.ready = threading.Event()
        self.load_seconds = 0.0

    def warm(self) -> None:
        started = time.perf_counter()
        try:
            with urllib.request.urlopen(f"{BIREFNET_URL}/health", timeout=2) as resp:
                if json.loads(resp.read()).get("model_loaded", True):
                    self.mode = "http"
        except Exception:  # noqa: BLE001 - absence is the normal case
            pass
        if self.mode is None:
            try:
                self._load_local()
                self.mode = "local"
            except Exception as exc:  # noqa: BLE001
                self.error = f"{type(exc).__name__}: {exc}"
        self.load_seconds = time.perf_counter() - started
        self.ready.set()

    def _load_local(self) -> None:
        import torch
        from torchvision import transforms
        from transformers import AutoModelForImageSegmentation

        # The GPU on this box is usually full of other people's models; CPU is
        # slower but it always works, and this runs once per logo change.
        want = os.environ.get("BIREFNET_DEVICE")
        if not want:
            want = "cuda" if torch.cuda.is_available() and _free_vram_gb() > 6 else "cpu"
        self.device = want
        dtype = torch.float16 if want.startswith("cuda") else torch.float32
        model = AutoModelForImageSegmentation.from_pretrained(
            BIREFNET_MODEL, trust_remote_code=True, torch_dtype=dtype)
        model.eval().to(want)
        self.model = model
        size = int(os.environ.get("BIREFNET_INPUT_SIZE", "1024" if want.startswith("cuda") else "768"))
        self.transform = transforms.Compose([
            transforms.Resize((size, size)),
            transforms.ToTensor(),
            transforms.Normalize([0.485, 0.456, 0.406], [0.229, 0.224, 0.225]),
        ])

    def cutout(self, png: bytes):
        from PIL import Image

        self.ready.wait()
        image = Image.open(io.BytesIO(png)).convert("RGB")
        if self.mode == "http":
            payload = {"image_url": "data:image/png;base64," +
                       base64.b64encode(png).decode(), "output_format": "png"}
            data = post_json(f"{BIREFNET_URL}/v1/images/background-removals", payload)
            raw = base64.b64decode(data["image"]["b64_json"])
            return Image.open(io.BytesIO(raw)).convert("RGBA")
        if self.mode == "local":
            return self._cutout_local(image)
        raise RuntimeError(f"no background remover available ({self.error})")

    def _cutout_local(self, image):
        import torch
        from PIL import Image

        tensor = self.transform(image).unsqueeze(0).to(self.device)
        if self.device.startswith("cuda"):
            tensor = tensor.half()
        with torch.no_grad():
            pred = self.model(tensor)[-1].sigmoid().float().cpu()[0].squeeze()
        mask = Image.fromarray((pred.numpy() * 255).astype("uint8"), mode="L")
        mask = mask.resize(image.size, Image.LANCZOS)
        out = image.convert("RGBA")
        out.putalpha(mask)
        return out


def _free_vram_gb() -> float:
    try:
        import torch

        free, _ = torch.cuda.mem_get_info()
        return free / (1 << 30)
    except Exception:  # noqa: BLE001
        return 0.0


# --- packaging ----------------------------------------------------------

def trim_and_square(img, pad_ratio: float = 0.04):
    """Crop to the subject, then pad back to a square so the logo never jitters
    between sizes."""
    from PIL import Image

    box = img.getchannel("A").getbbox()
    if box:
        img = img.crop(box)
    side = int(max(img.size) * (1 + 2 * pad_ratio))
    canvas = Image.new("RGBA", (side, side), (0, 0, 0, 0))
    canvas.paste(img, ((side - img.width) // 2, (side - img.height) // 2))
    return canvas


def save_webp(img, path: str, size: int, quality: int = 85, method: int = 4) -> int:
    from PIL import Image

    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    resized = img.resize((size, size), Image.LANCZOS)
    # method=4, not 6. Measured on this exact logo at 512px q85:
    #   method=0  76ms  53.1KB    method=4  147ms  44.7KB
    #   method=3 145ms  44.4KB    method=6 6935ms  43.6KB
    # The last step of the ladder costs 47x the time for 2% of the bytes.
    # exact=True keeps the RGB under fully transparent pixels, which matters
    # if anyone ever re-composites the cutout.
    resized.save(path, "WEBP", quality=quality, method=method, exact=True)
    return os.path.getsize(path)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="static/logo.webp")
    ap.add_argument("--candidates", type=int, default=3)
    ap.add_argument("--size", type=int, default=1024)
    ap.add_argument("--quality", type=int, default=85)
    ap.add_argument("--method", type=int, default=4,
                    help="WebP effort 0-6; 6 costs 47x the time for 2%% of the bytes")
    ap.add_argument("--pick", type=int, default=None,
                    help="skip selection and use this candidate index")
    ap.add_argument("--prompt", default=PROMPT)
    ap.add_argument("--keep-raw", action="store_true")
    ap.add_argument("--from", dest="source", default=None,
                    help="skip generation and cut out this PNG instead")
    args = ap.parse_args()

    timer = Timer()
    seg = Segmenter()
    # Starts now, finishes while the GPU is busy drawing monkeys.
    warmer = threading.Thread(target=seg.warm, daemon=True)
    warmer.start()

    if args.source:
        # Re-cutting an image we already like costs nothing and skips the
        # slowest stage entirely.
        with open(args.source, "rb") as fh:
            candidates = [fh.read()]
        timer.mark("reuse existing image", 0.0)
    else:
        candidates = generate_candidates(args, timer)
    if not candidates:
        print("no candidates generated", file=sys.stderr)
        return 1

    pick = args.pick if args.pick is not None else 0
    pick = max(0, min(pick, len(candidates) - 1))

    warmer.join()
    timer.mark(f"segmenter warm ({seg.mode or 'unavailable'})", seg.load_seconds)

    started = time.perf_counter()
    cut = seg.cutout(candidates[pick])
    timer.mark("background removal", time.perf_counter() - started)

    started = time.perf_counter()
    cut = trim_and_square(cut)
    outputs = []
    base, _ = os.path.splitext(args.out)
    for size, path in ((512, args.out), (256, f"{base}-256.webp"), (96, f"{base}-96.webp")):
        outputs.append((path, size, save_webp(cut, path, size, args.quality, args.method)))
    timer.mark("encode webp q%d" % args.quality, time.perf_counter() - started)

    print("\nwrote:")
    for path, size, nbytes in outputs:
        print(f"  {path:<32} {size}px  {nbytes/1024:6.1f} KB")
    print("\ntiming:")
    timer.report()
    return 0


def generate_candidates(args, timer: Timer) -> list[bytes]:
    print(f"generating {args.candidates} candidate(s) at {args.size}px...", flush=True)
    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=args.candidates) as pool:
        futures = [pool.submit(generate, args.prompt, args.size, 1000 + i)
                   for i in range(args.candidates)]
        candidates = []
        for i, fut in enumerate(futures):
            try:
                candidates.append(fut.result())
            except Exception as exc:  # noqa: BLE001
                print(f"  candidate {i} failed: {exc}", file=sys.stderr)
    timer.mark("generate (parallel)", time.perf_counter() - started)

    if args.keep_raw:
        for i, raw in enumerate(candidates):
            with open(f"/tmp/mojojojo-candidate-{i}.png", "wb") as fh:
                fh.write(raw)
        print(f"  wrote {len(candidates)} raw candidates to /tmp")
    return candidates


if __name__ == "__main__":
    sys.exit(main())
