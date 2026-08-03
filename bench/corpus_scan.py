"""What actually stops real Python from compiling?

The subset grows by guessing unless somebody counts. This walks a tree of real
Python — or notebooks — finds every function with a loop in it, asks the static
filter why it refused, and reports the reasons by frequency. That list is the
work queue: the top entry is the feature worth adding next, and anything in the
tail is not.

    bench/corpus_scan.py /nvme0n1-disk/code/mojo-factory
    bench/corpus_scan.py ~/notebooks --limit 400 --show 25
    bench/corpus_scan.py . --reason "call to sorted"     # list the examples

Static only, so it is seconds rather than hours: no compiling and no running, and
therefore no argument types. That is the right trade for deciding *what* to build
— a construct the filter rejects can never reach the transpiler, so the filter's
reasons are a strict superset of the real blockers and the ordering is what
matters.

A function with no loop is not counted at all. There is nothing to win there, so
counting them would just dilute every percentage with glue code.
"""

from __future__ import annotations

import argparse
import ast
import json
import os
import sys
from collections import Counter

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__))), "runner"))

import accel  # noqa: E402

SKIP_DIRS = {".git", ".pixi", "node_modules", "__pycache__", ".venv", "venv",
             "site-packages", ".mypy_cache", ".pytest_cache", "build", "dist",
             ".cargo-target", "target"}


def python_sources(root: str, limit: int):
    """Every .py and .ipynb under `root`, as (label, source) pairs."""
    seen = 0
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS
                       and not d.startswith(".")]
        for name in sorted(filenames):
            if seen >= limit:
                return
            path = os.path.join(dirpath, name)
            if name.endswith(".py"):
                try:
                    with open(path, encoding="utf-8", errors="replace") as fh:
                        yield path, fh.read()
                    seen += 1
                except OSError:
                    continue
            elif name.endswith(".ipynb"):
                for label, source in notebook_cells(path):
                    yield label, source
                seen += 1


def notebook_cells(path: str):
    """Each code cell of a notebook as its own source unit.

    Cells, not the concatenation: a notebook's cells do not always parse
    together — a later cell can redefine a name or depend on `%magic` output —
    and one unparseable cell should not hide the functions in all the others.
    """
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            book = json.load(fh)
    except (OSError, ValueError):
        return
    for i, cell in enumerate(book.get("cells", [])):
        if cell.get("cell_type") != "code":
            continue
        source = "".join(cell.get("source", []))
        # IPython magics and shell escapes are not Python and never parse.
        lines = [ln for ln in source.splitlines()
                 if not ln.lstrip().startswith(("%", "!", "?"))]
        yield f"{path}#cell{i}", "\n".join(lines)


def has_loop(fn: ast.FunctionDef) -> bool:
    return any(isinstance(n, (ast.For, ast.While)) for n in ast.walk(fn))


def scan(root: str, limit: int):
    reasons = Counter()
    sole = Counter()
    examples: dict[str, list[str]] = {}
    accepted = []
    files = candidates = 0

    for label, source in python_sources(root, limit):
        try:
            tree = ast.parse(source)
        except (SyntaxError, ValueError):
            continue
        files += 1
        # Nested functions count too: a hot loop inside a method is still a hot
        # loop, and excluding them would flatter the numbers.
        defs = [n for n in ast.walk(tree) if isinstance(n, ast.FunctionDef)]
        local = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
        consts = frozenset(_module_consts(tree))
        for fn in defs:
            if not has_loop(fn):
                continue
            candidates += 1
            why = accel.blockers(fn, local, consts=consts)
            if not why:
                accepted.append(f"{label}:{fn.name}")
                continue
            buckets = []
            for reason in why:
                key = _bucket(reason)
                if key not in buckets:
                    buckets.append(key)
            for key in buckets:
                reasons[key] += 1
                examples.setdefault(key, []).append(f"{label}:{fn.name}")
            if len(buckets) == 1:
                # Blocked by exactly one thing: adding it makes this function
                # compile. This is the number that decides what to build.
                sole[buckets[0]] += 1
    return files, candidates, accepted, reasons, examples, sole


def _module_consts(tree: ast.Module) -> set[str]:
    try:
        from mojosub.transpile import module_constants

        return set(module_constants(tree))
    except Exception:  # noqa: BLE001 - the scan works without mojosub importable
        return set()


def _bucket(why: str) -> str:
    """Collapse a reason to something countable.

    `call to json.loads` and `call to np.argsort` are the same work item only if
    you are adding "attribute calls"; they are different if you are adding
    `argsort`. So the callee name is kept and the *helper* prefix is not — a
    helper's reason belongs in the same bucket as the reason itself.
    """
    if why.startswith("helper "):
        return _bucket(why.split(": ", 1)[1]) if ": " in why else why
    if why.startswith("unbound name "):
        return "unbound name (module global or closure)"
    return why


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("root")
    ap.add_argument("--limit", type=int, default=2000, help="files to read")
    ap.add_argument("--show", type=int, default=20, help="reasons to list")
    ap.add_argument("--reason", default="", help="list examples for one reason")
    ap.add_argument("--json", default="")
    args = ap.parse_args()

    files, candidates, accepted, reasons, examples, sole = scan(args.root, args.limit)
    if not candidates:
        print("no functions with loops found", file=sys.stderr)
        return 2

    print(f"{files} files parsed, {candidates} functions with a loop")
    print(f"{len(accepted)} compilable as-is ({len(accepted) / candidates:.1%})\n")

    if args.reason:
        for line in examples.get(args.reason, [])[:40]:
            print(" ", line)
        return 0

    refused = candidates - len(accepted)
    print(f"{'blocks':>7} {'of all':>7} {'SOLE':>6} {'would fix':>10}  reason")
    print("-" * 68)
    for reason, n in reasons.most_common(args.show):
        only = sole.get(reason, 0)
        print(f"{n:>7} {n / refused:>6.1%} {only:>6} {only / refused:>9.1%}"
              f"  {reason}")
    print(f"\n{len(reasons)} distinct blockers over {refused} refused functions.")
    print("`blocks` counts every function the construct appears in; `SOLE` counts "
          "the ones it is the\nonly blocker for — which is what adding it would "
          "actually buy. Examples: --reason \"<r>\"")
    top = sole.most_common(args.show)
    if top:
        cumulative = 0
        print("\nwork queue, by marginal value:")
        for reason, n in top:
            cumulative += n
            print(f"  +{n / refused:>5.1%}  (running {cumulative / refused:>5.1%})"
                  f"  {reason}")

    if args.json:
        with open(args.json, "w") as fh:
            json.dump({"files": files, "candidates": candidates,
                       "accepted": len(accepted),
                       "reasons": reasons.most_common()}, fh, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
