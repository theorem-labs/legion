from __future__ import annotations

import json
import shutil
from pathlib import Path
from typing import Any

import pytest

from cgraph import config, pipeline

FIXTURE = Path(__file__).parent / "fixtures" / "mini"


def build(root: Path, part: str, out: Path, cache: Path | None = None) -> pipeline.Result:
    return pipeline.run(config.load(root / "cgraph.toml"), part, out, cache)


def read(out: Path, name: str) -> Any:
    return json.loads((out / name).read_text())


@pytest.fixture(scope="session")
def built(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("both")
    result = build(FIXTURE, "both", out)
    assert result.passes, result.hard_checks
    return out


@pytest.fixture(scope="session")
def graph(built: Path) -> dict[str, Any]:
    data: dict[str, Any] = read(built, "graph.json")
    return data


@pytest.fixture
def copy(tmp_path: Path) -> Path:
    """A writable copy of the fixture program."""
    target = tmp_path / "mini"
    shutil.copytree(FIXTURE, target)
    return target


def edges(graph: dict[str, Any], kind: str, source: str | None = None) -> list[dict[str, Any]]:
    return [edge for edge in graph["edges"] if edge["kind"] == kind and (source is None or edge["source"] == source)]


def node(graph: dict[str, Any], identity: str) -> dict[str, Any]:
    return next(item for item in graph["nodes"] if item["id"] == identity)
