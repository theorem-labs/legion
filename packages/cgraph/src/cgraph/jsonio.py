"""Stable JSON: sorted keys, no timestamps, one array element per line so that diffs stay small."""

from __future__ import annotations

import collections
import json
from pathlib import Path
from typing import Any


def canonical(value: Any) -> str:
    """The C4 ordering key for JSON values."""
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def exclusion_document(part: str, items: list[dict[str, Any]]) -> dict[str, Any]:
    """The C3 exclusions document, with one canonical order and no duplicate items."""
    unique: dict[str, dict[str, Any]] = {}
    for item in items:
        normalized = {
            "kind": item["kind"],
            "reason": item["reason"],
            "function": item.get("function"),
            "target": item.get("target"),
            "detail": item.get("detail"),
            "provenance": item.get("provenance", []),
        }
        unique[canonical(normalized)] = normalized
    ordered = [unique[key] for key in sorted(unique)]
    counts: collections.defaultdict[str, collections.Counter[str]] = collections.defaultdict(collections.Counter)
    for item in ordered:
        counts[item["kind"]][item["reason"]] += 1
    return {
        "format": "cgraph/exclusions/v1",
        "part": part,
        "counts": {kind: dict(sorted(reasons.items())) for kind, reasons in sorted(counts.items())},
        "items": ordered,
    }


def _compact(value: Any) -> str:
    return canonical(value)


def dumps(value: Any) -> str:
    """Objects at the top level are spread over lines; each element of a top-level list (or of a
    list one level down) is one compact line."""
    if not isinstance(value, dict):
        return _compact(value) + "\n"
    lines = ["{"]
    items = sorted(value.items())
    for index, (key, item) in enumerate(items):
        comma = "," if index + 1 < len(items) else ""
        lines.append(f"{_compact(key)}:{_block(item)}{comma}")
    lines.append("}")
    return "\n".join(lines) + "\n"


def _block(value: Any) -> str:
    if isinstance(value, list) and value:
        return "[\n" + ",\n".join(_compact(item) for item in value) + "\n]"
    if isinstance(value, dict) and value and any(isinstance(item, list) and len(item) > 4 for item in value.values()):
        items = sorted(value.items())
        return "{\n" + ",\n".join(f"{_compact(key)}:{_block(item)}" for key, item in items) + "\n}"
    return _compact(value)


def write(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(dumps(value))
    temporary.replace(path)


def read(path: Path) -> Any:
    return json.loads(path.read_text())
