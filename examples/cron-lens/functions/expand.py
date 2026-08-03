"""Expand cron schedules with Mojo when warm and croniter everywhere else."""

from datetime import datetime
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

try:
    from mojo_croniter import croniter
    ENGINE = "Mojo"
except (ImportError, OSError, RuntimeError):
    from croniter import croniter
    ENGINE = "Python"


def handler(request):
    body = request.get("body") or {}
    expression = str(body.get("expression", "")).strip()
    timezone = str(body.get("timezone", "UTC")).strip() or "UTC"
    if not expression or len(expression) > 120:
        return {"status": 400, "body": {"error": "Enter a cron expression under 120 characters."}}
    try:
        zone = ZoneInfo(timezone)
    except ZoneInfoNotFoundError:
        return {"status": 400, "body": {"error": "Unknown time zone."}}
    try:
        raw = str(body.get("start", "")).strip()
        start = datetime.fromisoformat(raw.replace("Z", "+00:00")) if raw else datetime.now(zone)
        start = start.replace(tzinfo=zone) if start.tzinfo is None else start.astimezone(zone)
        schedule = croniter(expression, start, ret_type=datetime)
        occurrences = [schedule.get_next(datetime).isoformat() for _ in range(12)]
    except (TypeError, ValueError, KeyError) as exc:
        return {"status": 400, "body": {"error": str(exc)}}
    return {"body": {"expression": expression, "timezone": timezone,
                      "start": start.isoformat(), "occurrences": occurrences,
                      "engine": ENGINE}}
