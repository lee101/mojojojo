"""`mojojojo` on the command line.

    mojojojo run script.py [--packages numpy,pandas] [--gpu] [--timeout 30]
    mojojojo run -c "print(6*7)"
    cat script.py | mojojojo run -
    mojojojo pricing
    mojojojo me
    mojojojo credits [--json]
    mojojojo project "My app" && mojojojo deploy PROJECT_ID site.zip
    mojojojo run --project-dir src --entrypoint main.mojo

Output goes to stdout exactly as the program printed it; the cost line goes to
stderr, so piping this into something else stays clean.
"""

from __future__ import annotations

import argparse
import json
import mimetypes
from pathlib import Path
import sys

from . import Client, MojojojoError, __version__


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="mojojojo", description=__doc__)
    parser.add_argument("--version", action="version", version=__version__)
    sub = parser.add_subparsers(dest="command", required=True)

    def add_run_arguments(command):
        command.add_argument("file", nargs="?", help="path, or - for stdin")
        command.add_argument("-c", "--code", help="run this snippet instead of a file")
        command.add_argument("--packages", default="", help="comma-separated PyPI specs")
        command.add_argument("--timeout", type=float, default=0, help="seconds")
        command.add_argument("--gpu", action="store_true")
        command.add_argument("--language", choices=("python", "mojo"), default="python")
        command.add_argument("--project-dir", help="send every .mojo file below this directory")
        command.add_argument("--entrypoint", default="main.mojo")
        command.add_argument("--mojo-libs", action="store_true",
                             help="opt Python into compatible Mojo-backed libraries")
        command.add_argument("--no-accel", action="store_true",
                             help="plain CPython, no Mojo compilation")
        command.add_argument("--no-auto-install", action="store_true")
        command.add_argument("--json", action="store_true", help="full response")

    add_run_arguments(sub.add_parser("run", help="run a Python file, snippet, or stdin"))
    add_run_arguments(sub.add_parser(
        "precompile", help="run a representative workload and prepare compiled artifacts"))

    sub.add_parser("pricing", help="what a millisecond costs")
    sub.add_parser("me", help="who you are and what you have left")
    credits = sub.add_parser("credits", help="balance, what it buys, and recent movements")
    credits.add_argument("--json", action="store_true", help="full response")
    sub.add_parser("projects", help="list cloud projects")
    project = sub.add_parser("project", help="create a cloud project")
    project.add_argument("name")
    project.add_argument("--slug", default="")
    project.add_argument("--runtime", choices=("static", "app", "mojo"), default="static")
    project.add_argument("--category", choices=("app", "tool"), default="app")
    deploy = sub.add_parser("deploy", help="deploy an immutable ZIP bundle")
    deploy.add_argument("project_id")
    deploy.add_argument("bundle")
    publish = sub.add_parser("publish", help="save a directory and publish it from Studio storage")
    publish.add_argument("project_id")
    publish.add_argument("directory", nargs="?", default=".")
    submit = sub.add_parser("submit", help="submit a published project to the showcase")
    submit.add_argument("project_id")
    upload = sub.add_parser("upload", help="upload a public asset")
    upload.add_argument("file")
    upload.add_argument("--project", default="", dest="project_id")
    sub.add_parser("storage", help="storage usage and recent assets")
    sub.add_parser("vector-projects", help="list managed vector-search projects")
    vector_project = sub.add_parser("vector-project", help="create a vector-search project")
    vector_project.add_argument("name")
    vector_project.add_argument("--slug", default="")
    vector_project.add_argument("--tier", choices=("exact", "performance"), default="exact")
    vector_upsert = sub.add_parser("vector-upsert", help="index a JSON/JSONL document file")
    vector_upsert.add_argument("project_id")
    vector_upsert.add_argument("file", help="JSON array, JSON object, or JSONL with id/text/metadata")
    vector_search = sub.add_parser("vector-search", help="search a vector project")
    vector_search.add_argument("project_id")
    vector_search.add_argument("query")
    vector_search.add_argument("--limit", type=int, default=10)

    args = parser.parse_args(argv)
    client = Client()

    try:
        if args.command == "pricing":
            json.dump(client.pricing(), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "me":
            json.dump(client.me(), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "credits":
            return _credits(client, args.json)
        if args.command == "projects":
            json.dump(client.projects(), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "project":
            json.dump(client.create_project(args.name, slug=args.slug,
                                            runtime=args.runtime, category=args.category),
                      sys.stdout, indent=2)
            print()
            return 0
        if args.command == "deploy":
            json.dump(client.deploy(args.project_id, args.bundle),
                      sys.stdout, indent=2)
            print()
            return 0
        if args.command == "publish":
            root = Path(args.directory).resolve()
            project_files = {
                path.relative_to(root).as_posix(): path.read_text(encoding="utf-8")
                for path in root.rglob("*")
                if path.is_file() and not any(part.startswith(".") for part in path.relative_to(root).parts)
                and path.stat().st_size <= 1_000_000
            }
            client.save_project_files(args.project_id, project_files)
            json.dump(client.publish_project(args.project_id), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "submit":
            json.dump(client.submit_project(args.project_id), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "upload":
            path = Path(args.file)
            kind = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
            json.dump(client.upload(path.name, path.read_bytes(), content_type=kind,
                                    project_id=args.project_id),
                      sys.stdout, indent=2)
            print()
            return 0
        if args.command == "storage":
            json.dump(client.storage(), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "vector-projects":
            json.dump(client.vector_projects(), sys.stdout, indent=2)
            print()
            return 0
        if args.command == "vector-project":
            json.dump(client.create_vector_project(args.name, slug=args.slug,
                                                   tier=args.tier),
                      sys.stdout, indent=2)
            print()
            return 0
        if args.command == "vector-upsert":
            documents = _vector_documents(Path(args.file))
            json.dump(client.vector_upsert(args.project_id, documents),
                      sys.stdout, indent=2)
            print()
            return 0
        if args.command == "vector-search":
            json.dump(client.vector_search(args.project_id, args.query,
                                           limit=args.limit),
                      sys.stdout, indent=2)
            print()
            return 0

        files = None
        if args.project_dir:
            root = Path(args.project_dir)
            files = {
                path.relative_to(root).as_posix(): path.read_text(encoding="utf-8")
                for path in root.rglob("*.mojo") if path.is_file()
            }
            if not files:
                raise MojojojoError(f"no .mojo files below {root}")
            code = ""
            args.language = "mojo"
        else:
            code = _source(args)
            if args.file and args.file.endswith(".mojo"):
                args.language = "mojo"
        run_options = dict(
            packages=[p for p in args.packages.split(",") if p.strip()],
            timeout_ms=int(args.timeout * 1000) if args.timeout else None,
            gpu=args.gpu,
            accel=not args.no_accel,
            auto_install=not args.no_auto_install,
            language=args.language,
            files=files,
            entrypoint=args.entrypoint,
            mojo_libs=args.mojo_libs,
        )
        prepared = None
        if args.command == "precompile":
            prepared = client.precompile(code, source="sdk-python:cli:precompile", **run_options)
            result = prepared.result
        else:
            result = client.run(code, source="sdk-python:cli", **run_options)
    except MojojojoError as exc:
        print(f"mojojojo: {exc}", file=sys.stderr)
        return 2

    if args.json:
        if prepared is not None:
            json.dump({"ready": prepared.ready, "message": prepared.message,
                       "kernels": prepared.kernels, "run": result.raw}, sys.stdout, indent=2)
        else:
            json.dump(result.raw, sys.stdout, indent=2)
        print()
        return 0 if result.ok else 1

    sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    bits = [f"{result.charge.billed_ms:.0f}ms billed",
            f"{result.charge.credits} credit(s)",
            f"${result.charge.usd:.6f}".rstrip("0")]
    if prepared is not None:
        bits.insert(0, "ready" if prepared.ready else prepared.message)
    if result.auto_installed:
        bits.append("installed " + ", ".join(result.auto_installed))
    if result.accel.native_calls:
        bits.append(f"{result.accel.native_calls} native calls")
    elif result.accel.queued:
        bits.append("compiling kernels for next time")
    if result.welcome_credits:
        bits.append(f"+{result.welcome_credits} welcome credits")
    print("— " + ", ".join(bits), file=sys.stderr)
    return 0 if result.ok else 1


def _credits(client, as_json: bool) -> int:
    data = client.credits()
    if as_json:
        json.dump(data, sys.stdout, indent=2)
        print()
        return 0
    if not data.get("signed_in"):
        print(f"Not signed in. Create an account at {data.get('signup', 'https://mojojojo.cc/auth/signup')}"
              f" — new accounts get {data.get('welcome_credits', 0)} credits free — "
              f"then make a key at {client.endpoint}/keys.", file=sys.stderr)
        return 2
    balance = data.get("balance", {})
    buys = data.get("buys", {})
    print(f"{data['credits']} credits (${balance.get('usd', 0):.3f}) — "
          f"{balance.get('free', 0)} free, {balance.get('plan', 0)} plan, "
          f"{balance.get('paid', 0)} paid")
    print(f"buys {buys.get('cpu_ms', 0) / 3600000:.1f} core-hours "
          f"or {buys.get('gpu_ms', 0) / 60000:.0f} GPU-minutes")
    print(f"{data.get('total_ms', 0):.0f} ms run here, "
          f"{data.get('owed_ms', 0):.0f} ms accruing towards the next credit")
    for entry in data.get("ledger", [])[:5]:
        print(f"  {entry['at'][:19]}  {entry['delta']:+6d}  {entry['reason']}  ({entry['app']})")
    print(f"manage credits: {data.get('top_up', 'https://mojojojo.cc/credits')}")
    return 0


def _source(args) -> str:
    if args.code:
        return args.code
    if args.file in (None, "-"):
        if sys.stdin.isatty():
            raise MojojojoError("nothing to run: pass a file, -c CODE, or pipe stdin")
        return sys.stdin.read()
    with open(args.file, "r", encoding="utf-8") as fh:
        return fh.read()


def _vector_documents(path: Path) -> list[dict]:
    raw = path.read_text(encoding="utf-8")
    try:
        value = json.loads(raw)
        if isinstance(value, dict):
            value = [value]
        if not isinstance(value, list):
            raise ValueError("top-level JSON must be an object or array")
        return value
    except json.JSONDecodeError:
        documents = [json.loads(line) for line in raw.splitlines() if line.strip()]
        if not documents:
            raise MojojojoError("document file is empty")
        return documents


if __name__ == "__main__":
    sys.exit(main())
