"""Cache-key behavior independent of libclang extraction."""

from __future__ import annotations

import ast
import importlib
import shutil
from pathlib import Path

import pytest

from cgraph import cache


def _contents(tmp_path: Path) -> cache.Contents:
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True)
    (tree / "include").mkdir()
    (tree / "src" / "unit.c").write_text("int unit;\n")
    (tree / "include" / "unit.h").write_text("extern int unit;\n")
    return cache.Contents(tree)


def _key(contents: cache.Contents) -> str:
    return cache.unit_key(
        "src/unit.c",
        ["clang", "-Iinclude"],
        "vocabulary",
        ["include/unit.h"],
        contents,
        include_probes=[],
    )


def test_unit_key_covers_libclang_and_python_versions(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    contents = _contents(tmp_path)
    monkeypatch.setattr(cache, "_TOOL", cache.tool_digest(b"cindex", b"libclang-a", "Python A", "machine"))
    first = _key(contents)
    assert _key(contents) == first

    monkeypatch.setattr(cache, "_TOOL", cache.tool_digest(b"cindex", b"libclang-b", "Python A", "machine"))
    assert _key(contents) != first

    monkeypatch.setattr(cache, "_TOOL", cache.tool_digest(b"cindex", b"libclang-b", "Python B", "machine"))
    assert _key(contents) != first


def test_unit_key_changes_when_an_include_probe_appears(tmp_path: Path) -> None:
    contents = _contents(tmp_path)
    probe = "include/override/unit.h"
    first = cache.unit_key("src/unit.c", ["clang"], "vocabulary", [], contents, include_probes=[probe])

    path = contents.c_tree / probe
    path.parent.mkdir()
    path.write_text("extern int overridden;\n")

    assert cache.unit_key("src/unit.c", ["clang"], "vocabulary", [], contents, include_probes=[probe]) != first


def _extractor_modules(package: Path) -> set[str]:
    """Every module of the package that extracting a unit's facts runs, read from the imports of
    ``clangfacts`` and of what it imports. The test reads them for itself, so a module added to the
    extractor and not to the key fails here."""
    found: set[str] = set()
    pending = ["clangfacts"]
    while pending:
        name = pending.pop()
        if name in found:
            continue
        found.add(name)
        for node in ast.walk(ast.parse(package.joinpath(*name.split(".")).with_suffix(".py").read_text())):
            if isinstance(node, ast.ImportFrom) and node.level == 1:
                pending += [alias.name for alias in node.names] if node.module is None else [node.module]
    return found


def test_the_unit_key_covers_every_module_the_extractor_runs(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """A copy of the package, one module of the extractor changed at a time: each change must give
    another tool digest, so no module of it can change a fact behind a key that stays."""
    package = tmp_path / "cgraph_under_test"
    shutil.copytree(Path(cache.__file__).parent, package, ignore=shutil.ignore_patterns("__pycache__"))
    monkeypatch.syspath_prepend(str(tmp_path))
    copied = importlib.import_module("cgraph_under_test.cache")
    before = copied._tool_digest()

    for name in sorted(_extractor_modules(package)):
        path = package / f"{name}.py"
        text = path.read_text()
        path.write_text(text + "\n# a change that changes a fact\n")
        copied._tool_digest.cache_clear()
        changed = copied._tool_digest()
        path.write_text(text)
        assert changed != before, f"the key does not cover {name}.py"

    copied._tool_digest.cache_clear()
    assert copied._tool_digest() == before
