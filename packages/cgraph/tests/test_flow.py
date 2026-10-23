"""Code the compiler removes before it emits any, and field displacements from the machine code."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from conftest import FIXTURE, build, read


@pytest.fixture(scope="module")
def flow(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    out = tmp_path_factory.mktemp("flow")
    result = build(FIXTURE, "flow", out)
    assert result.passes, result.hard_checks
    return {"graph": read(out, "graph.json"), "checks": read(out, "checks.json")}


def edges_from(graph: dict[str, Any], source: str) -> list[tuple[str, str]]:
    return [(edge["kind"], edge["target"]) for edge in graph["edges"] if edge["source"] == source]


def test_an_inline_helper_constant_and_a_decided_and_remove_their_branches(flow: dict[str, Any]) -> None:
    """feature_on() returns 0, so `on && b->size`, `if (feature_on())` and `0 && bag_weight()` are dead."""
    assert edges_from(flow["graph"], "function:flow_guarded") == []
    reasons = {(item["unit"], item["reason"]) for item in flow["graph"]["analysis"]["unevaluated"]
               if item["function"] == "function:flow_guarded"}  # fmt: skip
    assert ("struct:box", "dead_branch") in reasons


def test_code_after_a_return_is_not_code(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_after_return") == []


def test_the_image_decides_a_builtin_constant_choice(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_builtin") == []
    decided = flow["graph"]["analysis"]["image_decided"]
    assert ("function:flow_builtin", "box_weight") in [(item["function"], item.get("call")) for item in decided]


def test_a_container_of_field_is_matched_at_its_displacement(flow: dict[str, Any]) -> None:
    (item,) = flow["graph"]["analysis"]["field_accesses"]["function:flow_container"]
    assert (item["base"], item["displacement"]) == ("container_of", 24)
    assert flow["checks"]["structure_layer"]["field_offsets"]["by_method"] == {"memory_displacement": 1}


def test_layer_checks_classify_what_they_exclude(tmp_path: Path) -> None:
    out = tmp_path / "out"
    build(FIXTURE, "both", out)
    offsets = read(out, "checks.json")["structure_layer"]["field_offsets"]
    assert "excluded" in offsets and "miss_examples" in offsets


def test_an_argument_the_inline_callee_ignores_is_not_an_access(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_ignored") == []
    reasons = {(item["unit"], item["reason"]) for item in flow["graph"]["analysis"]["unevaluated"]
               if item["function"] == "function:flow_ignored"}  # fmt: skip
    assert reasons == {("global:all_bags", "ignored_argument")}


def test_code_after_a_builtin_decided_exit_is_left_to_the_image(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_builtin_exit") == []
    decided = {(item["function"], item.get("call")) for item in flow["graph"]["analysis"]["image_decided"]}
    assert ("function:flow_builtin_exit", "box_weight") in decided


def test_a_function_passed_to_an_inlined_helper_is_folded(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_pointer") == [("access", "global:all_bags")]
    folds = [fold for fold in flow["graph"]["analysis"]["folds"] if fold["image_caller"] == "function:flow_pointer"]
    assert ("function:read_bags@src/flow.c", "pointer") in {(fold["helper"], fold.get("through")) for fold in folds}
