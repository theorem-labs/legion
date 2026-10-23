"""Stable on-disk facts for decoded image functions."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any

import capstone

from .config import ImageRules
from .errors import InputError


def _sha256(path: Path, description: str) -> str:
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as error:
        raise InputError(f"cannot read {description} {path}: {error}") from error


def _rules(rules: ImageRules) -> dict[str, Any]:
    return {
        "arch": "riscv64",
        "thread_pointer": rules.thread_pointer,
        "percpu_sections": list(rules.percpu_sections),
        "symbol_classes": {name: pattern.pattern for name, pattern in sorted(rules.symbol_classes.items())},
        "call_aliases": dict(sorted(rules.call_aliases.items())),
    }


class ImageCache:
    """Facts stored at ``<cache>/image/<key>.json`` for one image function."""

    def __init__(
        self, directory: Path | None, image: Path, system_map: Path, rules: ImageRules, image_source: Path
    ) -> None:
        if directory is None:
            self.directory = None
            self.inputs: dict[str, Any] = {}
            return
        self.directory = directory / "image"
        self.inputs = {
            "image": _sha256(image, "image"),
            "system_map": _sha256(system_map, "System.map"),
            "image_rules": _rules(rules),
            "image_source": _sha256(image_source, "image decoder source"),
            "capstone": capstone.__version__,
        }

    def key(self, name: str, address: int, size: int, arity: dict[str, int], results: dict[str, int]) -> str:
        payload = {
            **self.inputs,
            "symbol": {"name": name, "address": address, "size": size},
            "arity": dict(sorted(arity.items())),
            "results": dict(sorted(results.items())),
        }
        return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    def load(
        self, name: str, address: int, size: int, arity: dict[str, int], results: dict[str, int]
    ) -> dict[str, Any] | None:
        if self.directory is None:
            return None
        path = self.directory / f"{self.key(name, address, size, arity, results)}.json"
        if not path.exists():
            return None
        try:
            value = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError) as error:
            raise InputError(f"cannot parse image cache {path}: {error}; remove the cache directory") from error
        if not isinstance(value, dict):
            raise InputError(f"cannot parse image cache {path}: expected an object; remove the cache directory")
        return value

    def store(
        self, name: str, address: int, size: int, arity: dict[str, int], results: dict[str, int], facts: dict[str, Any]
    ) -> None:
        if self.directory is None:
            return
        path = self.directory / f"{self.key(name, address, size, arity, results)}.json"
        temporary: Path | None = None
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            if not path.exists():
                descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
                temporary = Path(temporary_name)
                with os.fdopen(descriptor, "w") as file:
                    json.dump(facts, file, sort_keys=True, separators=(",", ":"))
                try:
                    os.replace(temporary, path)
                except FileExistsError:
                    # Another cache holder decoded the same function first. Its atomic entry is valid.
                    temporary.unlink(missing_ok=True)
        except OSError as error:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
            raise InputError(f"cannot write image cache {path}: {error}") from error
