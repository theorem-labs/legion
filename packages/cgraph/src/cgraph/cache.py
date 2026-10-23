"""The facts of each translation unit, kept between runs.

A unit's entry is reused only when its key matches: the hash of the extraction code, of the
extraction vocabulary, of the unit's compile arguments, and of the contents of the unit's file and
of every file it included the last time it was parsed. A change to any of them gives a new key, so
the unit is parsed again; no other unit is.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from . import clangfacts, libclang

_TOOL = hashlib.sha256(
    b"".join(Path(module.__file__ or "").read_bytes() for module in (clangfacts, libclang))
).hexdigest()


class Contents:
    """SHA-256 of files, each read once per run."""

    def __init__(self, c_tree: Path) -> None:
        self.c_tree = c_tree
        self.known: dict[str, str] = {}

    def digest(self, relative: str) -> str:
        if relative not in self.known:
            path = Path(relative) if Path(relative).is_absolute() else self.c_tree / relative
            try:
                self.known[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
            except OSError:
                self.known[relative] = "missing"
        return self.known[relative]


def unit_key(source: str, arguments: list[str], vocabulary_digest: str, includes: list[str], contents: Contents) -> str:
    payload = {
        "tool": _TOOL,
        "vocabulary": vocabulary_digest,
        "arguments": arguments,
        "source": [source, contents.digest(source)],
        "includes": [[name, contents.digest(name)] for name in sorted(includes)],
    }
    return hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest()


def sources_digest(facts: dict[str, dict[str, Any]], contents: Contents) -> str:
    """The C text the graph was built from: every unit's file and includes, by content."""
    payload = [
        [source, contents.digest(source), [[name, contents.digest(name)] for name in unit["includes"]]]
        for source, unit in sorted(facts.items())
    ]
    return hashlib.sha256(json.dumps(payload).encode()).hexdigest()


class Cache:
    def __init__(self, directory: Path | None) -> None:
        self.directory = directory
        self.manifest: dict[str, dict[str, Any]] = {}
        if directory is not None and (directory / "manifest.json").exists():
            self.manifest = json.loads((directory / "manifest.json").read_text())

    def lookup(
        self, source: str, arguments: list[str], vocabulary_digest: str, contents: Contents
    ) -> dict[str, Any] | None:
        entry = self.manifest.get(source)
        if self.directory is None or entry is None:
            return None
        if unit_key(source, arguments, vocabulary_digest, entry["includes"], contents) != entry["key"]:
            return None
        path = self.directory / "facts" / f"{entry['key']}.json"
        if not path.exists():
            return None
        facts: dict[str, Any] = json.loads(path.read_text())
        return facts

    def store(
        self, source: str, arguments: list[str], vocabulary_digest: str, facts: dict[str, Any], contents: Contents
    ) -> None:
        if self.directory is None:
            return
        key = unit_key(source, arguments, vocabulary_digest, facts["includes"], contents)
        path = self.directory / "facts" / f"{key}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            temporary = path.with_suffix(".tmp")
            temporary.write_text(json.dumps(facts, sort_keys=True, separators=(",", ":")))
            temporary.replace(path)
        self.manifest[source] = {"key": key, "includes": facts["includes"]}

    def save(self) -> None:
        if self.directory is None:
            return
        self.directory.mkdir(parents=True, exist_ok=True)
        temporary = self.directory / "manifest.json.tmp"
        temporary.write_text(json.dumps(self.manifest, sort_keys=True, indent=1))
        temporary.replace(self.directory / "manifest.json")
