"""Contract C3: every input item that does not reach graph.json is in exclusions.json with its
reason. Each test builds a compiled program whose C holds the item, and reads the item back."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from conftest import read

from cgraph import config, pipeline

# State accesses the graph cannot hold (tests/fixtures/reasons).
REASONS = Path(__file__).parent / "fixtures" / "reasons"


@pytest.fixture(scope="module")
def built(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("reasons")
    pipeline.run(config.load(REASONS / "cgraph.toml"), "one", out)
    return out


def _accesses(out: Path, reason: str) -> list[dict[str, Any]]:
    return [
        item for item in read(out, "exclusions.json")["items"] if item["kind"] == "access" and item["reason"] == reason
    ]


def _lock(function: str, reason: str, line: int) -> dict[str, Any]:
    """The excluded state access of a lock or an unlock at ``line`` of src/reasons.c."""
    return {
        "kind": "access",
        "reason": reason,
        "function": function,
        "target": None,
        "detail": {"field": None, "parameter": 0},
        "provenance": [{"source": "c", "file": "src/reasons.c", "line": line}],
    }


def test_a_lock_that_is_the_root_s_own_parameter_is_parameter_at_root(built: Path) -> None:
    """``take_parameter`` locks and unlocks its parameter ``g``: no state of the program is it."""
    assert _accesses(built, "parameter_at_root") == [
        _lock("function:take_parameter", "parameter_at_root", 23),
        _lock("function:take_parameter", "parameter_at_root", 24),
    ]


def test_a_folded_callee_s_lock_the_caller_cannot_name_is_unbound_parameter(built: Path) -> None:
    """``take_fetched`` passes ``take_inner`` the value of a call: the lock take_inner takes through
    its parameter binds to nothing of the caller's. The provenance is the callee's lock and unlock."""
    assert _accesses(built, "unbound_parameter") == [
        _lock("function:take_fetched", "unbound_parameter", 30),
        _lock("function:take_fetched", "unbound_parameter", 31),
    ]


def test_a_folded_callee_s_lock_through_the_caller_s_member_is_an_edge(built: Path) -> None:
    """The contrast: ``take_loaded`` passes a member of its own parameter, which the graph holds."""
    edges = [
        (edge["target"], edge["field"], edge["access"])
        for edge in read(built, "graph.json")["edges"]
        if edge["kind"] == "access" and edge["source"] == "function:take_loaded"
    ]
    assert ("struct:holder@src/reasons.c", "outer", "lock") in edges
    assert ("struct:holder@src/reasons.c", "outer", "unlock") in edges
    assert not [item for item in read(built, "exclusions.json")["items"] if item["function"] == "function:take_loaded"]


def test_checks_count_each_reason_the_exclusions_list(built: Path) -> None:
    counts = read(built, "exclusions.json")["counts"]["access"]
    assert counts == {"parameter_at_root": 2, "unbound_parameter": 2}
    assert read(built, "checks.json")["exclusions"]["access"] == counts
