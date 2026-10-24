"""The decoded facts of each image function, kept between runs.

Two kinds of entry, each written once and never changed:

- ``<cache>/image/callees/<symbol key>.json``: the text functions a function calls or jumps to
  directly. The symbol key hashes the image, ``System.map``, the ``[image]`` configuration, the
  decoder's source, the capstone and pyelftools versions, and the symbol's name, address and size,
  so these names are a fact of the image alone.
- ``<cache>/image/<facts key>.json``: the function's facts. The facts key adds the only C facts the
  decoder reads for it: the function's own result registers, and the number of parameters of each
  direct callee (which decides the argument registers a call passes).

A change to any other function's C signature leaves both keys as they are. An entry that cannot be
read or does not have its shape is an input error: the run stops and names the cache to remove.
"""

from __future__ import annotations

import dataclasses
import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any

import capstone
import elftools

from .config import ImageRules
from .errors import CacheError, InputError


def _sha256(path: Path, description: str) -> str:
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as error:
        raise InputError(f"cannot read {description} {path}: {error}") from error


def _canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def _rules(rules: ImageRules) -> dict[str, Any]:
    return {
        "arch": "riscv64",
        "thread_pointer": rules.thread_pointer,
        "percpu_sections": list(rules.percpu_sections),
        "symbol_classes": {name: pattern.pattern for name, pattern in sorted(rules.symbol_classes.items())},
        "call_aliases": dict(sorted(rules.call_aliases.items())),
        "trap_table": None if rules.trap_table is None else dataclasses.asdict(rules.trap_table),
    }


class ImageCache:
    def __init__(
        self, directory: Path | None, image: Path, system_map: Path, rules: ImageRules, image_source: Path
    ) -> None:
        self.directory = None if directory is None else directory / "image"
        self._prefix = b""
        if self.directory is None:
            return
        inputs = {
            "image": _sha256(image, "image"),
            "system_map": _sha256(system_map, "System.map"),
            "image_rules": _rules(rules),
            "image_source": _sha256(image_source, "image decoder source"),
            "cache_source": _sha256(Path(__file__), "image cache source"),
            "capstone": capstone.__version__,
            "pyelftools": elftools.__version__,
        }
        self._prefix = hashlib.sha256(_canonical(inputs)).hexdigest().encode()

    @property
    def enabled(self) -> bool:
        return self.directory is not None

    def symbol_key(self, name: str, address: int, size: int) -> str:
        return hashlib.sha256(self._prefix + _canonical([name, address, size])).hexdigest()

    def facts_key(self, symbol_key: str, results: int, arities: dict[str, int]) -> str:
        return hashlib.sha256(symbol_key.encode() + _canonical([results, sorted(arities.items())])).hexdigest()

    def callees(self, symbol_key: str) -> list[str] | None:
        if self.directory is None:
            return None
        path = self.directory / "callees" / f"{symbol_key}.json"
        value = self._read(path)
        if value is None:
            return None
        names = value.get("callees") if isinstance(value, dict) else None
        if not isinstance(names, list) or not all(isinstance(name, str) for name in names):
            raise self._malformed(path)
        return names

    def store_callees(self, symbol_key: str, callees: list[str]) -> None:
        if self.directory is not None:
            self._write(self.directory / "callees" / f"{symbol_key}.json", {"callees": callees})

    def load(self, facts_key: str) -> dict[str, Any] | None:
        if self.directory is None:
            return None
        path = self.directory / f"{facts_key}.json"
        value = self._read(path)
        if value is None:
            return None
        if not isinstance(value, dict):
            raise self._malformed(path)
        return value

    def store(self, facts_key: str, facts: dict[str, Any]) -> None:
        if self.directory is not None:
            self._write(self.directory / f"{facts_key}.json", facts)

    def malformed(self, facts_key: str) -> CacheError:
        assert self.directory is not None
        return self._malformed(self.directory / f"{facts_key}.json")

    def _malformed(self, path: Path) -> CacheError:
        return CacheError(f"image cache entry {path} is malformed; remove the cache directory {self.directory}")

    def _read(self, path: Path) -> Any:
        try:
            text = path.read_text()
        except FileNotFoundError:
            return None
        except OSError as error:
            raise self._malformed(path) from error
        try:
            return json.loads(text)
        except json.JSONDecodeError as error:
            raise self._malformed(path) from error

    def _write(self, path: Path, value: Any) -> None:
        if path.exists():
            return
        temporary: Path | None = None
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
            temporary = Path(temporary_name)
            with os.fdopen(descriptor, "wb") as file:
                file.write(_canonical(value))
            # Another holder of the cache may have written the same entry first: it holds the same bytes.
            os.replace(temporary, path)
        except OSError as error:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
            raise InputError(f"cannot write image cache entry {path}: {error}") from error
