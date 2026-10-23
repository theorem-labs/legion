"""Code the compiler removes before it emits any, and field displacements from the machine code."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from conftest import FIXTURE, build, read

from cgraph import clangfacts, commands, config, contracts, image, link


@pytest.fixture(scope="module")
def flow(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    out = tmp_path_factory.mktemp("flow")
    result = build(FIXTURE, "flow", out)
    assert result.passes, result.hard_checks
    return {
        "graph": read(out, "graph.json"),
        "checks": read(out, "checks.json"),
        "exclusions": read(out, "exclusions.json"),
    }


@pytest.fixture(scope="module")
def flow_facts() -> tuple[config.Config, dict[str, Any]]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    source = "src/flow.c"
    return loaded, clangfacts.extract(
        source,
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source),
        loaded.vocabulary,
    )


def fact_events(facts: dict[str, Any], function: str) -> dict[str, list[dict[str, Any]]]:
    return facts["functions"][function]["events"]


def edges_from(graph: dict[str, Any], source: str) -> list[tuple[str, str]]:
    return [(edge["kind"], edge["target"]) for edge in graph["edges"] if edge["source"] == source]


def test_an_inline_helper_constant_and_a_decided_and_remove_their_branches(flow: dict[str, Any]) -> None:
    """feature_on() returns 0, so `on && b->size`, `if (feature_on())` and `0 && bag_weight()` are dead."""
    assert edges_from(flow["graph"], "function:flow_guarded") == []
    reasons = {
        (item["target"], item["reason"])
        for item in flow["exclusions"]["items"]
        if item["function"] == "function:flow_guarded"
    }
    assert ("struct:box", "dead_branch") in reasons


def test_code_after_a_return_is_not_code(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_after_return") == []


def test_the_image_decides_a_builtin_constant_choice(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_builtin") == []
    decided = {
        (item["function"], item["target"]) for item in flow["exclusions"]["items"] if item["reason"] == "image_decided"
    }
    assert ("function:flow_builtin", "function:box_weight") in decided


def test_a_container_of_field_is_matched_at_its_displacement(flow: dict[str, Any]) -> None:
    (item,) = flow["graph"]["analysis"]["field_accesses"]["function:flow_container"]
    assert (item["base"], item["displacement"]) == ("container_of", 24)
    assert flow["checks"]["structure_layer"]["field_offsets"]["by_method"] == {"memory_displacement": 2}


def test_layer_checks_classify_what_they_exclude(tmp_path: Path) -> None:
    out = tmp_path / "out"
    build(FIXTURE, "both", out)
    offsets = read(out, "checks.json")["structure_layer"]["field_offsets"]
    assert "excluded" in offsets and "misses" in offsets and "image_evidence" in offsets


def test_an_argument_the_inline_callee_ignores_is_not_an_access(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_ignored") == []
    reasons = {
        (item["target"], item["reason"])
        for item in flow["exclusions"]["items"]
        if item["function"] == "function:flow_ignored"
    }
    assert reasons == {("global:all_bags", "ignored_argument")}


def test_code_after_a_builtin_decided_exit_is_left_to_the_image(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_builtin_exit") == []
    decided = {
        (item["function"], item["target"]) for item in flow["exclusions"]["items"] if item["reason"] == "image_decided"
    }
    assert ("function:flow_builtin_exit", "function:box_weight") in decided


def test_a_function_passed_to_an_inlined_helper_is_folded(flow: dict[str, Any]) -> None:
    assert edges_from(flow["graph"], "function:flow_pointer") == [("access", "global:all_bags")]
    folds = [fold for fold in flow["graph"]["analysis"]["folds"] if fold["image_caller"] == "function:flow_pointer"]
    assert ("function:read_bags@src/flow.c", "pointer") in {(fold["helper"], fold.get("through")) for fold in folds}


def test_a_decided_or_marks_its_right_operand_dead(flow_facts: tuple[config.Config, dict[str, Any]]) -> None:
    _, facts = flow_facts
    assert {(item.get("call"), item["reason"]) for item in fact_events(facts, "flow_decided_or")["unevaluated"]} == {
        ("box_weight", "dead_branch")
    }
    assert {(item.get("call"), item["reason"]) for item in fact_events(facts, "flow_constant_or")["unevaluated"]} == {
        ("bag_weight", "dead_branch")
    }


def test_nested_macro_generic_keeps_only_the_selected_callee(flow_facts: tuple[config.Config, dict[str, Any]]) -> None:
    _, facts = flow_facts
    calls = fact_events(facts, "flow_nested_generic")["calls"]
    assert [(call["callee"], call.get("generic")) for call in calls] == [("box_weight", None)]


def test_code_after_goto_and_trap_is_not_evaluated(flow_facts: tuple[config.Config, dict[str, Any]]) -> None:
    _, facts = flow_facts
    for function in ("flow_after_goto", "flow_after_trap"):
        assert ("obj_free", "after_exit") in {
            (item.get("call"), item["reason"]) for item in fact_events(facts, function)["unevaluated"]
        }


def test_alignof_operand_is_not_evaluated(flow_facts: tuple[config.Config, dict[str, Any]]) -> None:
    _, facts = flow_facts
    reasons = {
        (item["unit"]["name"], item["reason"])
        for item in fact_events(facts, "flow_alignof")["unevaluated"]
        if item.get("unit") is not None
    }
    assert ("box_count", "alignof") in reasons


def test_anonymous_nested_member_belongs_to_its_outer_record(flow_facts: tuple[config.Config, dict[str, Any]]) -> None:
    _, facts = flow_facts
    events = fact_events(facts, "flow_anonymous_member")
    access = next(item for item in events["accesses"] if item.get("unit", {}).get("field") == "b")
    assert access["unit"]["name"] == "anonymous_holder"
    assert access["unit"]["offset"] == 4
    field_access = next(item for item in events["field_accesses"] if item["field"] == "b")
    assert (field_access["struct"], field_access["offset"], field_access["displacement"]) == (
        "anonymous_holder",
        4,
        4,
    )


def test_one_called_and_one_inlined_helper_call_is_marked_partial_inline(
    flow_facts: tuple[config.Config, dict[str, Any]],
) -> None:
    loaded, facts = flow_facts
    assert loaded.inputs.contract_graph is not None
    reader = image.Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image)
    try:
        linker = link.Linker(
            loaded,
            ["src/flow.c"],
            {"src/flow.c": facts},
            reader,
            contracts.ContractGraph.load(loaded.inputs.contract_graph, loaded.contract_function_prefix),
        )
        definition = linker.visible("src/flow.c", "flow_partial_inline")
        assert definition is not None
        partial = linker.effective(definition.tu, definition.name, definition).accesses
    finally:
        reader.close()
    assert any(item.get("partial_inline") and item.get("unit", {}).get("name") == "all_bags" for item in partial)


def test_percpu_section_global_is_per_cpu_in_the_graph(flow: dict[str, Any]) -> None:
    node = next(item for item in flow["graph"]["nodes"] if item["id"] == "percpu:flow_percpu")
    assert node["kind"] == "percpu"
    assert ("access", "percpu:flow_percpu") in edges_from(flow["graph"], "function:flow_percpu_global")


def test_percpu_struct_is_marked_per_cpu_in_the_graph(flow: dict[str, Any]) -> None:
    node = next(item for item in flow["graph"]["nodes"] if item["name"] == "percpu_box")
    assert node["per_cpu"] is True
    assert ("access", node["id"]) in edges_from(flow["graph"], "function:flow_percpu_struct")
