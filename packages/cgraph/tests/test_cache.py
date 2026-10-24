"""Cache-key behavior independent of libclang extraction."""

from __future__ import annotations

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
