#!/usr/bin/env python3
"""Build data/toolcatalog.json from the hermes-agent tool sources.

Sources (all optional; each directory is skipped if absent):

  plugin-catalog/*.yaml          one native plugin per file
  optional-mcps/*/manifest.yaml  Nous-approved remote/local MCP servers
  skills/**/SKILL.md             workflow skills, YAML frontmatter only

Stdlib only. The YAML reader below is deliberately a subset: mappings, lists,
inline lists, quoted scalars, comments and the |/>/- block scalars. That is
everything these catalogs use; it is not a general YAML parser.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

HERMES = Path("/vfast/data/code/hermes-agent")
DESCRIPTION_MAX = 600
TAG_MAX = 12


# ---------------------------------------------------------------- YAML subset


class YAMLError(ValueError):
    pass


def _strip_comment(line: str) -> str:
    quote = None
    escaped = False
    for i, ch in enumerate(line):
        if quote:
            if escaped:
                escaped = False
            elif ch == "\\" and quote == '"':
                escaped = True
            elif ch == quote:
                quote = None
        elif ch in "\"'":
            quote = ch
        elif ch == "#" and (i == 0 or line[i - 1] in " \t"):
            return line[:i]
    return line


def _unquote(text: str) -> str:
    text = text.strip()
    if len(text) >= 2 and text[0] == text[-1] == '"':
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return text[1:-1]
    if len(text) >= 2 and text[0] == text[-1] == "'":
        return text[1:-1].replace("''", "'")
    return text


def _scalar_complete(text: str) -> bool:
    text = text.strip()
    if not text or text[0] not in "\"'":
        return True
    return len(text) >= 2 and text[-1] == text[0]


def _split_inline(text: str) -> list[str]:
    out: list[str] = []
    buf: list[str] = []
    quote = None
    escaped = False
    depth = 0
    for ch in text:
        if quote:
            buf.append(ch)
            if escaped:
                escaped = False
            elif ch == "\\" and quote == '"':
                escaped = True
            elif ch == quote:
                quote = None
            continue
        if ch in "\"'":
            quote = ch
            buf.append(ch)
        elif ch in "[{":
            depth += 1
            buf.append(ch)
        elif ch in "]}":
            depth -= 1
            buf.append(ch)
        elif ch == "," and depth == 0:
            out.append("".join(buf).strip())
            buf = []
        else:
            buf.append(ch)
    if "".join(buf).strip():
        out.append("".join(buf).strip())
    return out


def _parse_value(text: str):
    text = text.strip()
    if text == "" or text in ("null", "~"):
        return None
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        if not inner:
            return []
        return [_parse_value(part) for part in _split_inline(inner) if part]
    if text.startswith("{") and text.endswith("}"):
        inner = text[1:-1].strip()
        if not inner:
            return {}
        obj = {}
        for part in _split_inline(inner):
            key, sep, val = part.partition(":")
            if sep:
                obj[_unquote(key)] = _parse_value(val)
        return obj
    if text.lower() in ("true", "false"):
        return text.lower() == "true"
    return _unquote(text)


class _Line:
    __slots__ = ("indent", "text")

    def __init__(self, indent: int, text: str):
        self.indent = indent
        self.text = text


def _indent_of(raw: str) -> int:
    return len(raw) - len(raw.lstrip(" "))


class _Parser:
    def __init__(self, lines: list[str]):
        self.lines: list[_Line] = []
        for raw in lines:
            stripped = _strip_comment(raw).rstrip()
            if not stripped.strip():
                self.lines.append(_Line(-1, ""))  # blank, kept for block scalars
                continue
            self.lines.append(_Line(_indent_of(raw), stripped.strip()))
        self.i = 0

    def _skip_blank(self) -> None:
        while self.i < len(self.lines) and self.lines[self.i].indent < 0:
            self.i += 1

    def parse(self):
        self._skip_blank()
        if self.i >= len(self.lines):
            return {}
        node, _ = self._node(self.lines[self.i].indent)
        return node

    def _node(self, indent: int):
        self._skip_blank()
        if self.i >= len(self.lines):
            return {}, self.i
        if self.lines[self.i].text.startswith("- ") or self.lines[self.i].text == "-":
            return self._list(indent)
        return self._map(indent)

    def _block_scalar(self, parent_indent: int, style: str) -> str:
        self.i += 1
        chunks: list[str] = []
        base = None
        while self.i < len(self.lines):
            entry = self.lines[self.i]
            if entry.indent >= 0:
                if entry.indent <= parent_indent:
                    break
                if base is None:
                    base = entry.indent
                chunks.append(" " * max(0, entry.indent - base) + entry.text)
            else:
                chunks.append("")
            self.i += 1
        while chunks and chunks[-1] == "":
            chunks.pop()
        if style.startswith(">"):
            folded = []
            for chunk in chunks:
                if not chunk:
                    folded.append("\n")
                elif folded and not folded[-1].endswith("\n"):
                    folded.append(" " + chunk)
                else:
                    folded.append(chunk)
            text = "".join(folded)
        else:
            text = "\n".join(chunks)
        return text.rstrip("\n") if style.endswith("-") else text

    def _map(self, indent: int):
        obj: dict = {}
        while True:
            self._skip_blank()
            if self.i >= len(self.lines):
                break
            entry = self.lines[self.i]
            if entry.indent < indent:
                break
            if entry.indent > indent:
                # Stray deeper line; skip rather than guess.
                self.i += 1
                continue
            if entry.text.startswith("- ") or entry.text == "-":
                break
            key, sep, rest = entry.text.partition(":")
            if not sep:
                self.i += 1
                continue
            key = _unquote(key.strip())
            rest = rest.strip()
            if rest in ("|", "|-", "|+", ">", ">-", ">+"):
                obj[key] = self._block_scalar(indent, rest)
                continue
            if rest == "":
                self.i += 1
                self._skip_blank()
                if self.i < len(self.lines) and self.lines[self.i].indent > indent:
                    obj[key], _ = self._node(self.lines[self.i].indent)
                else:
                    obj[key] = {}
                continue
            self.i += 1
            while not _scalar_complete(rest):
                self._skip_blank()
                if self.i >= len(self.lines):
                    break
                rest += " " + self.lines[self.i].text
                self.i += 1
            obj[key] = _parse_value(rest)
        return obj, self.i

    def _list(self, indent: int):
        out: list = []
        while True:
            self._skip_blank()
            if self.i >= len(self.lines):
                break
            entry = self.lines[self.i]
            if entry.indent != indent or not (entry.text.startswith("- ") or entry.text == "-"):
                break
            item = entry.text[2:].strip() if entry.text.startswith("- ") else ""
            if not item:
                self.i += 1
                self._skip_blank()
                if self.i < len(self.lines) and self.lines[self.i].indent > indent:
                    value, _ = self._node(self.lines[self.i].indent)
                    out.append(value)
                else:
                    out.append(None)
                continue
            key, sep, rest = item.partition(":")
            if sep and re.match(r"^[A-Za-z0-9_.-]+$", key.strip()):
                obj = {}
                key = _unquote(key.strip())
                rest = rest.strip()
                self.i += 1
                if rest in ("|", "|-", ">", ">-"):
                    obj[key] = self._block_scalar(indent, rest)
                elif rest == "":
                    self._skip_blank()
                    if self.i < len(self.lines) and self.lines[self.i].indent > indent:
                        obj[key], _ = self._node(self.lines[self.i].indent)
                    else:
                        obj[key] = {}
                else:
                    obj[key] = _parse_value(rest)
                self._skip_blank()
                if self.i < len(self.lines) and self.lines[self.i].indent > indent:
                    extra, _ = self._map(self.lines[self.i].indent)
                    if isinstance(extra, dict):
                        obj.update(extra)
                out.append(obj)
                continue
            out.append(_parse_value(item))
            self.i += 1
        return out, self.i


def parse_yaml(text: str) -> dict:
    text = text.lstrip("\ufeff")
    if text.lstrip().startswith("{"):
        try:
            doc = json.loads(text)
            return doc if isinstance(doc, dict) else {}
        except json.JSONDecodeError:
            return {}
    doc = _Parser(text.splitlines()).parse()
    return doc if isinstance(doc, dict) else {}


def parse_frontmatter(text: str) -> dict:
    text = text.lstrip("\ufeff")
    if not text.startswith("---"):
        return {}
    lines = text.splitlines()
    if lines[0].strip() != "---":
        return {}
    for idx in range(1, len(lines)):
        if lines[idx].strip() == "---":
            return parse_yaml("\n".join(lines[1:idx]))
    return {}


# ---------------------------------------------------------------- catalog


def clean(text, limit: int = DESCRIPTION_MAX) -> str:
    text = re.sub(r"\s+", " ", str(text or "")).strip()
    if len(text) <= limit:
        return text
    cut = text[:limit]
    space = cut.rfind(" ")
    if space > limit // 2:
        cut = cut[:space]
    return cut.rstrip(" ,;:.") + "…"


def tags(*groups) -> list[str]:
    seen: list[str] = []
    for group in groups:
        if not group:
            continue
        if isinstance(group, str):
            group = [group]
        for item in group:
            if not isinstance(item, str):
                continue
            tag = re.sub(r"\s+", "-", item.strip().lower()).strip("-")
            if tag and tag not in seen:
                seen.append(tag)
            if len(seen) >= TAG_MAX:
                return seen
    return seen


def indent_truncate(value, depth: int = 0):
    """Keep schema values JSON-small and primitive."""
    if depth > 3:
        return None
    if isinstance(value, dict):
        return {str(k): indent_truncate(v, depth + 1) for k, v in value.items() if v not in (None, "", [], {})}
    if isinstance(value, list):
        return [indent_truncate(v, depth + 1) for v in value[:12] if v not in (None, "", [], {})]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return clean(value, 300) if isinstance(value, str) else value
    return str(value)


def read(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""


def plugin_entries(root: Path) -> list[dict]:
    out = []
    for path in sorted(root.glob("*.yaml")):
        doc = parse_yaml(read(path))
        name = str(doc.get("name") or "").strip()
        if not name:
            # removed.yaml is a blocklist, not a plugin.
            continue
        caps = doc.get("capabilities") if isinstance(doc.get("capabilities"), dict) else {}
        out.append({
            "id": f"plugin:{name}",
            "name": clean(doc.get("title") or name, 120),
            "kind": "plugin",
            "description": clean(doc.get("description")),
            "tags": tags(doc.get("category"), doc.get("tier"), doc.get("platforms"),
                         name.replace("_", "-").split("-"), "plugin"),
            "install": f"hermes plugins install {name}",
            "schema": indent_truncate({
                "repo": doc.get("repo"),
                "subdir": doc.get("subdir"),
                "version": doc.get("version"),
                "tools": caps.get("provides_tools"),
            }),
        })
    return out


def mcp_entries(root: Path) -> list[dict]:
    out = []
    for path in sorted(root.glob("*/manifest.yaml")):
        doc = parse_yaml(read(path))
        name = str(doc.get("name") or path.parent.name).strip()
        if not name:
            continue
        transport = doc.get("transport") if isinstance(doc.get("transport"), dict) else {}
        auth = doc.get("auth") if isinstance(doc.get("auth"), dict) else {}
        suggest = doc.get("suggest") if isinstance(doc.get("suggest"), dict) else {}
        tools_block = doc.get("tools") if isinstance(doc.get("tools"), dict) else {}
        out.append({
            "id": f"mcp:{name}",
            "name": name,
            "kind": "mcp",
            "description": clean(doc.get("description")),
            "tags": tags("mcp", doc.get("connector_slug"), suggest.get("keywords"),
                         auth.get("type")),
            "install": f"hermes mcp install {name}",
            "schema": indent_truncate({
                "transport": transport.get("type"),
                "url": transport.get("url"),
                "command": transport.get("command"),
                "auth": auth.get("type"),
                "tools_excluded": tools_block.get("default_excluded"),
                "source": doc.get("source"),
            }),
        })
    return out


def skill_entries(root: Path) -> list[dict]:
    out = []
    for path in sorted(root.glob("**/SKILL.md")):
        doc = parse_frontmatter(read(path))
        name = str(doc.get("name") or path.parent.name).strip()
        if not name:
            continue
        metadata = doc.get("metadata") if isinstance(doc.get("metadata"), dict) else {}
        hermes = metadata.get("hermes") if isinstance(metadata.get("hermes"), dict) else {}
        out.append({
            "id": f"skill:{name}",
            "name": name.replace("-", " "),
            "kind": "skill",
            "description": clean(doc.get("description")),
            "tags": tags(hermes.get("tags"), doc.get("platforms"), "skill"),
            "install": f"hermes skills install {name}",
            "schema": indent_truncate({
                "path": str(path.parent.relative_to(root.parent)),
                "version": doc.get("version"),
                "platforms": doc.get("platforms"),
                "related_skills": hermes.get("related_skills"),
            }),
        })
    return out


def build(hermes: Path) -> dict:
    entries: list[dict] = []
    entries += plugin_entries(hermes / "plugin-catalog")
    entries += mcp_entries(hermes / "optional-mcps")
    entries += skill_entries(hermes / "skills")

    seen = set()
    unique = []
    for entry in entries:
        if entry["id"] in seen:
            continue
        seen.add(entry["id"])
        unique.append(entry)
    unique.sort(key=lambda e: (e["kind"], e["name"].lower()))
    counts = {}
    for entry in unique:
        counts[entry["kind"]] = counts.get(entry["kind"], 0) + 1
    return {
        "source": "hermes-agent",
        "count": len(unique),
        "counts": counts,
        "tools": unique,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--hermes", type=Path, default=HERMES)
    ap.add_argument("--out", type=Path, default=Path(__file__).resolve().parent.parent / "data" / "toolcatalog.json")
    args = ap.parse_args()

    catalog = build(args.hermes)
    if not catalog["tools"]:
        print(f"no catalog entries found under {args.hermes}", file=sys.stderr)
        return 1
    args.out.parent.mkdir(parents=True, exist_ok=True)
    tmp = args.out.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(catalog, ensure_ascii=False, separators=(",", ":")) + "\n", encoding="utf-8")
    os.replace(tmp, args.out)
    print(f"wrote {args.out} ({catalog['count']} entries: {catalog['counts']}, {args.out.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
