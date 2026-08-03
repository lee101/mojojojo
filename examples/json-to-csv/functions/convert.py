import csv
import io


def handler(request):
    rows = request.get("body", {}).get("rows", [])
    if not isinstance(rows, list) or not rows:
        return {"status": 400, "body": "Send a non-empty rows array"}
    keys = list(dict.fromkeys(key for row in rows for key in row))
    output = io.StringIO()
    writer = csv.DictWriter(output, fieldnames=keys)
    writer.writeheader()
    writer.writerows(rows)
    return {
        "headers": {"Content-Type": "text/csv; charset=utf-8"},
        "body": output.getvalue(),
    }
