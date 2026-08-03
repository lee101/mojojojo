"""Generate compact SVG QR codes with a Mojo-first, Python-safe backend."""

try:
    import mojo_qrcode as qrcode
    ENGINE = "Mojo"
except (ImportError, OSError, RuntimeError):
    import qrcode
    ENGINE = "Python"


LEVELS = {
    "L": qrcode.ERROR_CORRECT_L,
    "M": qrcode.ERROR_CORRECT_M,
    "Q": qrcode.ERROR_CORRECT_Q,
    "H": qrcode.ERROR_CORRECT_H,
}


def handler(request):
    body = request.get("body") or {}
    text = str(body.get("text", "")).strip()
    if not text:
        return {"status": 400, "body": {"error": "Enter something to encode."}}
    if len(text.encode("utf-8")) > 2048:
        return {"status": 413, "body": {"error": "QR content is limited to 2 KiB."}}
    level = str(body.get("level", "M")).upper()
    if level not in LEVELS:
        return {"status": 400, "body": {"error": "Unknown correction level."}}

    qr = qrcode.QRCode(version=None, error_correction=LEVELS[level], box_size=1, border=4)
    qr.add_data(text)
    try:
        qr.make(fit=True)
    except (ValueError, OverflowError) as exc:
        return {"status": 400, "body": {"error": str(exc)}}
    matrix = qr.get_matrix()
    size = len(matrix)
    squares = "".join(
        f"M{x},{y}h1v1h-1z"
        for y, row in enumerate(matrix)
        for x, dark in enumerate(row) if dark
    )
    svg = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {size} {size}" '
           f'shape-rendering="crispEdges"><rect width="100%" height="100%" fill="white"/>'
           f'<path d="{squares}" fill="black"/></svg>')
    return {"body": {"svg": svg, "version": qr.version, "modules": size,
                      "engine": ENGINE, "bytes": len(text.encode("utf-8"))}}
