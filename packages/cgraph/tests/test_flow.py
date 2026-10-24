"""Code the compiler removes before it emits any, and field displacements from the machine code."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from conftest import FIXTURE, add_contract_functions, build, read, text_address

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
    offsets = flow["checks"]["structure_layer"]["field_offsets"]
    assert offsets["matched"] == offsets["candidates"] and set(offsets["by_method"]) == {"memory_displacement"}


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


def test_a_function_generic_selects_for_a_call_is_called_not_taken(
    flow_facts: tuple[config.Config, dict[str, Any]],
) -> None:
    """``_Generic(x, a: f, b: g)(x)`` calls the selected function; no association is an address."""
    _, facts = flow_facts
    events = fact_events(facts, "flow_measure")
    assert [call["callee"] for call in events["calls"]] == ["box_measure"]
    assert events["function_pointers"] == []


def test_the_type_of_a_generic_selection_names_the_association_text_cannot(
    flow_facts: tuple[config.Config, dict[str, Any]],
) -> None:
    """``measure_case`` pastes each association together; the selection's type is the selected one's."""
    _, facts = flow_facts
    calls = fact_events(facts, "flow_measure_any")["calls"]
    assert [(call["callee"], call.get("generic")) for call in calls] == [("box_measure", None)]


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


def test_a_unit_owns_the_external_definitions_of_a_header_only_it_includes(
    flow_facts: tuple[config.Config, dict[str, Any]],
) -> None:
    """flow_part.h is flow.c's own fragment, as tree_stall.h is kernel/rcu/tree.c's."""
    _, facts = flow_facts
    functions = facts["functions"]
    assert functions["flow_header_external"]["owned"] and functions["flow_guarded"]["owned"]
    assert not functions["flow_header_notify"]["owned"] and not functions["box_get_default"]["owned"]
    # External linkage, but an inline definition: every includer's, emitted by none.
    assert not functions["box_inline_default"]["owned"]


def test_a_function_named_by_a_file_scope_initializer_or_a_store_is_in_the_facts(
    flow_facts: tuple[config.Config, dict[str, Any]],
) -> None:
    _, facts = flow_facts
    assert {"flow_header_notify", "box_get_default", "box_get_none"} <= set(facts["functions"])
    pointers = fact_events(facts, "flow_set_hook")["function_pointers"]
    assert [pointer["target"] for pointer in pointers] == ["box_get_none"]


# (contract id, the image symbol whose address it gives, the part function that address must reach):
# code of src/flow.c that no part function's machine code calls.
UNCALLED_UNIT_CODE = [
    # An external definition in a header only flow.c includes: tree_stall.h's rcu_fwd_progress_check.
    ("Prog/src/flow_part/FlowHeaderExternal", "flow_header_external", "function:flow_header_external"),
    # A static of that header that only its table names: rcu_panic in tree_stall.h's notifier block.
    ("Prog/src/flow_part/FlowHeaderNotify", "flow_header_notify", "function:flow_header_notify@src/flow_part.h"),
    # A header inline that only a table of flow.c names: generic_setlease in fs/libfs.c's operations.
    ("Prog/src/flow/BoxGetDefault", "box_get_default", "function:box_get_default@include/mini.h"),
    # A header inline that a function of flow.c stores: hrtimer_dummy_timeout in hrtimer.c.
    ("Prog/src/flow/BoxGetNone", "box_get_none", "function:box_get_none@include/mini.h"),
    # A header inline flow.c calls inlined, whose copy a pointer gives: ktime_get_real.
    ("Prog/src/flow/ClockNow", "clock_now", "function:clock_now@include/mini.h"),
]


@pytest.mark.parametrize(
    ("identity", "symbol", "expected"), UNCALLED_UNIT_CODE, ids=[item[0] for item in UNCALLED_UNIT_CODE]
)
def test_unit_code_that_no_part_function_calls_owns_the_contract_at_its_address(
    copy: Path, tmp_path: Path, identity: str, symbol: str, expected: str
) -> None:
    add_contract_functions(copy, {"id": identity, "syms": [symbol], "addr": text_address(copy, symbol)})
    out = tmp_path / "out"
    result = build(copy, "flow", out)
    assert result.passes, result.hard_checks
    (function,) = [item for item in read(out, "graph.json")["nodes"] if item["id"] == expected]
    assert function["part"] and function["contract"] == identity
    assert not [item for item in read(out, "exclusions.json")["items"] if item["target"] == identity]


def test_a_header_copy_compiled_into_another_unit_is_no_function_of_the_part(copy: Path, tmp_path: Path) -> None:
    """bags.c inlines clock_now; the image's only copy is flow.c's, outside part both."""
    add_contract_functions(
        copy, {"id": "Prog/src/flow/ClockNow", "syms": ["clock_now"], "addr": text_address(copy, "clock_now")}
    )
    out = tmp_path / "out"
    assert build(copy, "both", out).passes
    graph = read(out, "graph.json")
    assert not [
        item for item in graph["nodes"] if item["id"] == "function:clock_now@include/mini.h" and item.get("part")
    ]
    (item,) = [item for item in read(out, "exclusions.json")["items"] if item["target"] == "Prog/src/flow/ClockNow"]
    assert item["reason"] == "no_part_function_at_address"
