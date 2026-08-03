"""Normalize a short phone list with full international numbering metadata."""

try:
    import mojo_phonenumbers as phonenumbers
    ENGINE = "Mojo"
except (ImportError, OSError, RuntimeError):
    import phonenumbers
    ENGINE = "Python"


def handler(request):
    body = request.get("body") or {}
    region = str(body.get("region", "US")).strip().upper() or "US"
    lines = [line.strip() for line in str(body.get("numbers", "")).splitlines() if line.strip()]
    if not lines:
        return {"status": 400, "body": {"error": "Paste at least one phone number."}}
    if len(lines) > 50:
        return {"status": 413, "body": {"error": "Normalize at most 50 numbers at once."}}
    rows = []
    for raw in lines:
        try:
            number = phonenumbers.parse(raw, region)
            rows.append({
                "input": raw,
                "e164": phonenumbers.format_number(number, phonenumbers.PhoneNumberFormat.E164),
                "international": phonenumbers.format_number(number, phonenumbers.PhoneNumberFormat.INTERNATIONAL),
                "national": phonenumbers.format_number(number, phonenumbers.PhoneNumberFormat.NATIONAL),
                "region": phonenumbers.region_code_for_number(number) or "—",
                "possible": bool(phonenumbers.is_possible_number(number)),
                "valid": bool(phonenumbers.is_valid_number(number)),
            })
        except phonenumbers.NumberParseException as exc:
            rows.append({"input": raw, "error": str(exc), "possible": False, "valid": False})
    return {"body": {"rows": rows, "engine": ENGINE, "default_region": region}}
