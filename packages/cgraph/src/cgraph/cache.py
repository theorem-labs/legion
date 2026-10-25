"""The facts of each translation unit, kept between runs."""

from __future__ import annotations

import ast
import fcntl
import functools
import hashlib
import json
import os
import platform
import sys
import tempfile
from pathlib import Path
from typing import Any, TextIO

import clang.cindex as cindex

from .errors import CacheError


def _digest(parts: list[bytes]) -> str:
    hashed = hashlib.sha256()
    for part in parts:
        hashed.update(len(part).to_bytes(8, "big"))
        hashed.update(part)
    return hashed.hexdigest()


def tool_digest(cindex_bytes: bytes, library_bytes: bytes, python_version: str, machine: str) -> str:
    """Hash the bindings, loaded library, and interpreter that produce facts."""
    return _digest([cindex_bytes, library_bytes, python_version.encode(), machine.encode()])


def _extractor_sources() -> list[Path]:
    """Every module of this package that extracting a unit's facts runs: ``clangfacts`` and, at any
    depth, what it imports. The content of each of them can change a fact, and a module added to the
    extractor is covered without a change here."""
    directory = Path(__file__).parent
    found: dict[str, Path] = {}
    pending = ["clangfacts"]
    while pending:
        name = pending.pop()
        if name in found:
            continue
        found[name] = directory.joinpath(*name.split(".")).with_suffix(".py")
        for node in ast.walk(ast.parse(found[name].read_bytes())):
            if isinstance(node, ast.ImportFrom) and node.level == 1:
                pending += [alias.name for alias in node.names] if node.module is None else [node.module]
    return [found[name] for name in sorted(found)]


@functools.cache
def _tool_digest() -> str:
    bindings = cindex.__file__
    library = cindex.conf.lib._name
    if bindings is None or not isinstance(library, str):
        raise RuntimeError("libclang did not disclose its bindings and library paths")
    return _digest(
        [
            *(part for path in _extractor_sources() for part in (path.name.encode(), path.read_bytes())),
            tool_digest(
                Path(bindings).read_bytes(), Path(library).read_bytes(), sys.version, platform.machine()
            ).encode(),
        ]
    )


_TOOL = _tool_digest()


class Contents:
    """SHA-256 of files, each read once per run."""

    def __init__(self, c_tree: Path) -> None:
        self.c_tree = c_tree
        self.known: dict[str, str] = {}

    def _path(self, name: str) -> Path:
        return Path(name) if Path(name).is_absolute() else self.c_tree / name

    def digest(self, relative: str) -> str:
        if relative not in self.known:
            try:
                self.known[relative] = hashlib.sha256(self._path(relative).read_bytes()).hexdigest()
            except OSError:
                self.known[relative] = "missing"
        return self.known[relative]

    def exists(self, probe: str) -> bool:
        return self._path(probe).exists()


def unit_key(
    source: str,
    arguments: list[str],
    vocabulary_digest: str,
    includes: list[str],
    contents: Contents,
    *,
    include_probes: list[str] | tuple[str, ...] = (),
) -> str:
    payload = {
        "tool": _TOOL,
        "vocabulary": vocabulary_digest,
        "arguments": arguments,
        "source": [source, contents.digest(source)],
        "includes": [[name, contents.digest(name)] for name in sorted(includes)],
        "include_probes": [[name, contents.exists(name)] for name in sorted(set(include_probes))],
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
        self._lock_file: TextIO | None = None

    def _manifest_error(self, path: Path) -> CacheError:
        return CacheError(f"cache manifest {path} is malformed; remove the cache directory {self.directory}")

    def _entry_error(self, path: Path) -> CacheError:
        return CacheError(f"cache entry {path} is missing or malformed; remove the cache directory {self.directory}")

    def _entry(self, source: str, value: Any, path: Path) -> dict[str, Any]:
        if not isinstance(value, dict):
            raise self._manifest_error(path)
        key, includes = value.get("key"), value.get("includes")
        probes = value.get("include_probes", [])
        if (
            not isinstance(key, str)
            or not key
            or not isinstance(includes, list)
            or not all(isinstance(name, str) for name in includes)
            or not isinstance(probes, list)
            or not all(isinstance(name, str) for name in probes)
        ):
            raise self._manifest_error(path)
        return {"key": key, "includes": includes, "include_probes": probes}

    def _load_manifest(self) -> None:
        assert self.directory is not None
        path = self.directory / "manifest.json"
        if not path.exists():
            self.manifest = {}
            return
        try:
            manifest = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError) as error:
            raise self._manifest_error(path) from error
        if not isinstance(manifest, dict) or not all(isinstance(source, str) for source in manifest):
            raise self._manifest_error(path)
        self.manifest = {source: self._entry(source, entry, path) for source, entry in manifest.items()}

    def _acquire(self) -> None:
        if self.directory is None or self._lock_file is not None:
            return
        self.directory.mkdir(parents=True, exist_ok=True)
        lock_file = (self.directory / "lock").open("a")
        try:
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX)
            self._lock_file = lock_file
            self._load_manifest()
        except BaseException:
            lock_file.close()
            self._lock_file = None
            raise

    def close(self) -> None:
        if self._lock_file is None:
            return
        fcntl.flock(self._lock_file.fileno(), fcntl.LOCK_UN)
        self._lock_file.close()
        self._lock_file = None

    def _facts(self, path: Path) -> dict[str, Any]:
        try:
            facts = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError) as error:
            raise self._entry_error(path) from error
        if not isinstance(facts, dict):
            raise self._entry_error(path)
        includes = facts.get("includes")
        if not isinstance(includes, list) or not all(isinstance(name, str) for name in includes):
            raise self._entry_error(path)
        probes = facts.get("include_probes", [])
        if not isinstance(probes, list) or not all(isinstance(name, str) for name in probes):
            raise self._entry_error(path)
        if not isinstance(facts.get("functions"), dict):
            raise self._entry_error(path)
        facts.setdefault("include_probes", [])
        return facts

    def facts_key(self, source: str, facts: dict[str, Any]) -> str:
        """What a later layer keys a unit's facts by: its cache key, or, for a unit the cache keeps no
        entry for (``reusable: false``, or no cache at all), the SHA-256 of the facts themselves."""
        entry = self.manifest.get(source)
        if entry is not None:
            return str(entry["key"])
        return "facts:" + hashlib.sha256(json.dumps(facts, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    def lookup(
        self, source: str, arguments: list[str], vocabulary_digest: str, contents: Contents
    ) -> dict[str, Any] | None:
        if self.directory is None:
            return None
        self._acquire()
        entry = self.manifest.get(source)
        if entry is None:
            return None
        path = self.directory / "facts" / f"{entry['key']}.json"
        if not path.exists():
            raise self._entry_error(path)
        if (
            unit_key(
                source,
                arguments,
                vocabulary_digest,
                entry["includes"],
                contents,
                include_probes=entry["include_probes"],
            )
            != entry["key"]
        ):
            return None
        return self._facts(path)

    def store(
        self, source: str, arguments: list[str], vocabulary_digest: str, facts: dict[str, Any], contents: Contents
    ) -> None:
        if self.directory is None:
            return
        self._acquire()
        if facts.get("reusable") is False:
            # A header spelling the facts could not expand: what the preprocessor searched is unknown,
            # so no later run may take this unit from the cache (C5).
            self.manifest.pop(source, None)
            return
        includes = facts.get("includes")
        probes = facts.get("include_probes", [])
        if (
            not isinstance(includes, list)
            or not all(isinstance(name, str) for name in includes)
            or not isinstance(probes, list)
            or not all(isinstance(name, str) for name in probes)
        ):
            raise CacheError(f"facts for {source} are malformed")
        key = unit_key(source, arguments, vocabulary_digest, includes, contents, include_probes=probes)
        path = self.directory / "facts" / f"{key}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            descriptor, temporary_name = tempfile.mkstemp(prefix=f".{key}.", suffix=".tmp", dir=path.parent)
            temporary = Path(temporary_name)
            try:
                with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                    stream.write(json.dumps(facts, sort_keys=True, separators=(",", ":")))
                os.replace(temporary, path)
            except BaseException:
                temporary.unlink(missing_ok=True)
                raise
        self.manifest[source] = {"key": key, "includes": includes, "include_probes": probes}

    def save(self) -> None:
        if self.directory is None:
            return
        self._acquire()
        descriptor, temporary_name = tempfile.mkstemp(prefix=".manifest.", suffix=".tmp", dir=self.directory)
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                stream.write(json.dumps(self.manifest, sort_keys=True, indent=1))
            os.replace(temporary, self.directory / "manifest.json")
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
        finally:
            self.close()
