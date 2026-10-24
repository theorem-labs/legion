"""Code the compiler removes before it emits any, and field displacements from the machine code."""

from __future__ import annotations

import collections
from pathlib import Path
from typing import Any

import clang.cindex as ci
import pytest
from conftest import FIXTURE, add_contract_functions, build, read, text_address

from cgraph import clangfacts, commands, config, contracts, frames, image, link


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
    # The empty note_bag has no effect, so its call is not compiled either.
    assert reasons == {("global:all_bags", "ignored_argument"), ("function:note_bag@src/flow.c", "discarded")}


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


@pytest.fixture(scope="module")
def fct_facts() -> dict[str, Any]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    source = "src/facts.c"
    return clangfacts.extract(
        source,
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source),
        loaded.vocabulary,
    )


@pytest.fixture(scope="module")
def fct_build(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    # The part's layer floors are not asserted here: each test below asserts one construct's facts.
    out = tmp_path_factory.mktemp("facts")
    build(FIXTURE, "facts", out)
    return {name: read(out, f"{name}.json") for name in ("graph", "checks", "exclusions")}


# The field accesses of each function as the machine code adds them (src/facts.c and its llvm-objdump):
# (struct, field, displacement from the base register, what the base register holds).
FIELD_DISPLACEMENTS = {
    # lw a0, 0x14(a0): d = &o->in folds into o.
    "fct_derived_local": {("fct_inner", "b", 20, "pointer")},
    # One access at in + b; o->in on its own is addressed nowhere.
    "fct_chain": {("fct_inner", "b", 20, "pointer")},
    # lw a0, 0x1c(a0): tail in the unnamed struct fct_index at 24.
    "fct_tagged_anonymous": {("fct_index", "tail", 28, "pointer")},
    # ld/sd 0x38(a0): stats + 3 * 8.
    "fct_constant_element": {("fct_outer", "stats", 56, "pointer")},
    # lw a0, 0x6c(a0) after adding k * 16: slots + b.
    "fct_indexed_element": {("fct_inner", "b", 108, "pointer")},
    # lw a0, 0x10(a0): (&o->in)->a is o->in.a.
    "fct_address_base": {("fct_inner", "a", 16, "pointer")},
    # lw a0, 0x14(a0): the inline helper returns &o->in.
    "fct_helper_return": {("fct_inner", "b", 20, "pointer")},
    # ld a0, -0xa0(a0): x from the link member container_of starts at; the type check is no access.
    "fct_container_local": {("fct_outer", "x", -160, "container_of")},
    "fct_container_helper": {("fct_outer", "x", -160, "container_of")},
    # ld a3, -0xa0(a2); ld a2, 0x0(a2): every assignment of o is container_of(..., link).
    "fct_container_loop": {("fct_outer", "x", -160, "container_of"), ("fct_outer", "link", 0, "container_of")},
    # auipc; lw 0x21c: g = &fct_global.in is the global's address plus in + b.
    "fct_global_field": {("fct_inner", "b", 20, "global")},
    # lw s0, 0x10(a0) before the if; after it, lw a0, 0xc(a0) from whichever pointer d holds.
    "fct_reassigned_local": {("fct_inner", "a", 16, "pointer"), ("fct_inner", "b", 12, "pointer")},
    # lw a0, 0x14(a0): the loop's in = NULL is never dereferenced.
    "fct_null_walk": {("fct_inner", "b", 20, "pointer")},
    # lw a0, 0x10(a0) through the parameter; then o is fct_global, whose in.b the image reads.
    "fct_parameter_before_write": {("fct_inner", "a", 16, "pointer"), ("fct_inner", "b", 20, "global")},
    # The arm that is not NULL, through an inline helper: x from the link member.
    "fct_conditional_container": {("fct_outer", "x", -160, "container_of")},
    # A helper that returns NULL sets in to no pointer the loop dereferences.
    "fct_null_helper": {("fct_inner", "b", 20, "pointer")},
    # The _Generic association the pointer's type selects.
    "fct_generic_container": {("fct_outer", "x", -160, "container_of")},
}


@pytest.mark.parametrize("function", sorted(FIELD_DISPLACEMENTS))
def test_a_field_access_has_the_displacement_the_machine_code_adds(fct_facts: dict[str, Any], function: str) -> None:
    events = fact_events(fct_facts, function)
    found = {(item["struct"], item["field"], item["displacement"], item["base"]) for item in events["field_accesses"]}
    assert found == FIELD_DISPLACEMENTS[function]


def test_a_field_access_names_the_parameter_its_base_pointer_is(fct_facts: dict[str, Any]) -> None:
    """Through a derived local (d = &o->in) and an inline helper, the base is still parameter 0."""
    for function in ("fct_derived_local", "fct_helper_return", "fct_container_helper"):
        assert {item.get("param") for item in fact_events(fct_facts, function)["field_accesses"]} == {0}
    # A global's field has no parameter.
    assert {item.get("param") for item in fact_events(fct_facts, "fct_global_field")["field_accesses"]} == {None}
    # A parameter is its own value until the function sets it; a null pointer is never dereferenced.
    found = {
        (item["field"], item.get("param"))
        for item in fact_events(fct_facts, "fct_parameter_before_write")["field_accesses"]
    }
    assert found == {("a", 0), ("b", None)}
    assert {item.get("param") for item in fact_events(fct_facts, "fct_null_walk")["field_accesses"]} == {0}


def test_every_non_container_field_offset_of_the_unit_matches_the_image(fct_build: dict[str, Any]) -> None:
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    misses = {(item["function"], item["struct"], item["field"]) for item in offsets["misses"]}
    container = {
        f"function:{name}"
        for name, fields in FIELD_DISPLACEMENTS.items()
        if any(f[3] == "container_of" for f in fields)
    }
    # &o->x is o itself (llvm-objdump: mv s0, a0; jal obj_free): no instruction carries offset 0, so the
    # image cannot show this field; the facts mark it `address` and the checks' rule decides it. Before
    # 6bb98770's image change it matched `sd s0, 0x0(sp)`, a stack store, by coincidence.
    no_instruction = {("function:fct_member_address", "fct_outer", "x")}
    assert {miss for miss in misses if miss[0] not in container} <= no_instruction
    assert offsets["matched"] >= 8


def test_a_cleanup_macro_alias_names_the_function_the_image_calls(fct_build: dict[str, Any]) -> None:
    """__free(box_release) runs __free_box_release, which include/linux/path.h-style #defines to box_release."""
    edges = edges_from(fct_build["graph"], "function:fct_cleanup_alias")
    assert ("call", "function:box_release") in edges
    assert not [edge for edge in edges if "__free_box_release" in edge[1]]


def test_a_weak_default_decides_no_branch(fct_build: dict[str, Any]) -> None:
    assert ("call", "function:obj_free") in edges_from(fct_build["graph"], "function:fct_after_weak")


def test_a_definition_that_begins_with_a_macro_is_still_weak(fct_facts: dict[str, Any]) -> None:
    """kernel/sched/clock.c's sched_clock: the text of the definition begins in notrace's header."""
    functions = fct_facts["functions"]
    assert functions["fct_traced_clock"]["weak"] is True
    assert functions["fct_weak_level"]["weak"] is True
    assert functions["fct_direct_weak"]["weak"] is True and functions["fct_listed_weak"]["weak"] is True
    assert functions["fct_traced_clock_user"]["weak"] is False


@pytest.mark.parametrize("function", ["fct_after_bug", "fct_after_die"])
def test_code_after_a_bug_or_a_noreturn_call_is_not_code(fct_facts: dict[str, Any], function: str) -> None:
    events = fact_events(fct_facts, function)
    assert ("obj_free", "after_exit") in {(item.get("call"), item["reason"]) for item in events["unevaluated"]}
    assert "obj_free" not in {call["callee"] for call in events["calls"]}


def test_a_discarded_value_is_never_read(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_discarded")
    assert events["accesses"] == [] and events["field_accesses"] == []
    assert {(item["unit"]["field"], item["reason"]) for item in events["unevaluated"]} == {
        ("size", "discarded"),
        ("first", "discarded"),
    }


def test_an_argument_a_macro_defined_inline_stub_ignores_is_not_an_access(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_ignored_macro")
    assert events["accesses"] == []
    assert {(item.get("call") or item["unit"]["name"], item["reason"]) for item in events["unevaluated"]} == {
        ("all_bags", "ignored_argument"),
        # The stub has no effect, so its call is not compiled either.
        ("fct_note", "discarded"),
    }


def test_the_right_operand_of_a_builtin_constant_and_is_left_to_the_image(fct_build: dict[str, Any]) -> None:
    assert edges_from(fct_build["graph"], "function:fct_builtin_and") == []
    decided = {
        (item["function"], item["target"])
        for item in fct_build["exclusions"]["items"]
        if item["reason"] == "image_decided"
    }
    assert ("function:fct_builtin_and", "global:fct_slot") in decided


def test_a_member_whose_address_is_taken_is_marked(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_member_address")
    found = {
        (item["struct"], item["field"], item["displacement"], item.get("address")) for item in events["field_accesses"]
    }
    assert found == {
        ("fct_outer", "x", 0, True),
        ("fct_outer", "in", 8, True),
        ("fct_outer", "stats", 32, True),
    }
    # A read is no address.
    assert not any(item.get("address") for item in fact_events(fct_facts, "fct_chain")["field_accesses"])


def test_a_global_initializer_records_the_addresses_it_stores(fct_facts: dict[str, Any]) -> None:
    found = {
        (item["global"]["name"], item["field"], item["offset"], item["target"]["name"], item["target_offset"])
        for item in fct_facts["global_initializers"]
        if item["global"]["name"] == "fct_info"
    }
    assert found == {
        ("fct_info", "code", 0, "fct_code", 0),
        ("fct_info", "start", 8, "fct_start", 0),
        ("fct_info", "inner", 16, "fct_global", 20),
    }


def call_argument(facts: dict[str, Any], function: str, callee: str) -> dict[str, Any]:
    (call,) = [call for call in fact_events(facts, function)["calls"] if call["callee"] == callee]
    (argument,) = call["arguments"]
    return argument


def test_a_member_address_argument_gives_its_displacement_from_the_parameter(fct_facts: dict[str, Any]) -> None:
    argument = call_argument(fct_facts, "fct_pass_member", "fct_takes_inner")
    assert (argument["param"], argument["displacement"], argument["base"]) == (0, 8, "pointer")
    # The address of a global's storage is no offset from a parameter: it is that far into the global.
    held = call_argument(fct_facts, "fct_pass_held_global", "fct_takes_inner")
    assert (held["param"], held["displacement"], held["base"]) == (None, 8, "global")


def test_a_parameter_passed_on_has_displacement_zero_and_a_loaded_pointer_none(fct_facts: dict[str, Any]) -> None:
    """Link rebases a folded callee only through an argument with a displacement: the parameter itself
    and a local equal to it are the callee's pointer; n->next is a value loaded through it."""
    arguments = [call["arguments"][0] for call in fact_events(fct_facts, "fct_pass_parameter")["calls"]]
    assert [(item.get("param"), item.get("displacement")) for item in arguments[:2]] == [(0, 0), (0, 0)]
    assert "displacement" not in arguments[2]


def test_a_parameter_the_function_writes_is_no_longer_its_parameter(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_reassigned_parameter")
    found = {(item["field"], item.get("param")) for item in events["field_accesses"]}
    assert found == {("x", None), ("in", None), ("link", None)}
    argument = call_argument(fct_facts, "fct_reassigned_parameter", "fct_takes_inner")
    assert (argument["param"], argument["displacement"]) == (None, 8)


@pytest.mark.parametrize("function", ["fct_pass_held_global", "fct_pass_arrow_of_address"])
def test_an_argument_that_is_a_globals_storage_names_the_global(fct_facts: dict[str, Any], function: str) -> None:
    unit = call_argument(fct_facts, function, "fct_takes_inner")["unit"]
    assert (unit["kind"], unit["name"], unit["field"]) == ("global", "fct_global", "in")


def test_the_last_statement_of_a_statement_expression_is_its_value(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_statement_value")
    assert {(item["unit"]["name"], item["access"]) for item in events["accesses"]} == {("fct_offsets", "read")}
    assert not [item for item in events["unevaluated"] if item["unit"]["name"] == "fct_offsets"]


def test_a_switch_on_a_constant_compiles_only_the_selected_case(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_constant_switch")
    assert {item["displacement"] for item in events["field_accesses"]} == {32}
    assert ("box_weight", "dead_branch") in {(item.get("call"), item["reason"]) for item in events["unevaluated"]}
    assert "box_weight" not in {call["callee"] for call in events["calls"]}


def test_an_indexed_element_through_a_helper_gives_both_displacements(fct_facts: dict[str, Any]) -> None:
    """The image folds slots + b into lw 0x6c (llvm-objdump); a register holding the element adds 12."""
    found = {
        (item["field"], item["displacement"], item["element_displacement"], item["base"])
        for item in fact_events(fct_facts, "fct_indexed_helper")["field_accesses"]
    }
    assert found == {("b", 108, 12, "pointer"), ("a", 104, 8, "pointer")}
    constant = fact_events(fct_facts, "fct_constant_element")["field_accesses"]
    assert not any("element_displacement" in item for item in constant)


def test_a_switch_on_a_helper_with_several_returns_keeps_every_case(fct_facts: dict[str, Any]) -> None:
    events = fact_events(fct_facts, "fct_helper_switch")
    assert {item["displacement"] for item in events["field_accesses"]} == {48, 40}
    assert not [item for item in events["unevaluated"] if item["reason"] == "dead_branch"]


@pytest.fixture(scope="module")
def dead_facts() -> dict[str, Any]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    source = "src/dead.c"
    return clangfacts.extract(
        source,
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source),
        loaded.vocabulary,
    )


@pytest.fixture(scope="module")
def dead_build(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    out = tmp_path_factory.mktemp("dead")
    result = build(FIXTURE, "dead", out)
    return {"result": result, **{name: read(out, f"{name}.json") for name in ("graph", "checks", "exclusions")}}


def callees(facts: dict[str, Any], function: str) -> set[str]:
    return {call["callee"] for call in fact_events(facts, function)["calls"]}


def unevaluated(facts: dict[str, Any], function: str) -> set[tuple[str | None, str]]:
    """(the call or the state's name, the reason) of each item the facts leave out."""
    return {
        (item.get("call") or (item.get("unit") or {}).get("name"), item["reason"])
        for item in fact_events(facts, function)["unevaluated"]
    }


def test_a_label_reached_only_by_a_dead_goto_is_not_code(dead_facts: dict[str, Any]) -> None:
    """__slab_free's slab_empty: with CONFIG_SLUB_TINY (llvm-objdump: li a0, 0; ret)."""
    assert callees(dead_facts, "dd_dead_label") == set()
    assert ("obj_free", "after_exit") in unevaluated(dead_facts, "dd_dead_label")
    # A label a live goto reaches, before or after the goto, is code.
    assert callees(dead_facts, "dd_live_label") == {"obj_free", "box_weight"}


@pytest.mark.parametrize(
    ("function", "left_out"),
    [
        # (void)(dd_lockp(b)): lockdep_assert_held(__rq_lockp(rq)).
        ("dd_void_call", {("dd_lockp", "discarded")}),
        # ((void)(dd_current()->size), 0): cpu_to_node(raw_smp_processor_id()).
        ("dd_comma_call", {("dd_current", "discarded"), ("box", "discarded")}),
        # dd_remove_file(b, &dd_attr_dev);: device_remove_file, external, only reads.
        ("dd_del", {("dd_remove_file", "discarded"), ("dd_attr_dev", "discarded")}),
        # dd_get_ns(ns); and dd_quiet(b);: get_user_ns(new->user_ns); and clk_pm_runtime_put(core);.
        ("dd_statement_calls", {("dd_get_ns", "discarded"), ("dd_quiet", "discarded")}),
        # tl = this_cpu_ptr(...) read only by a stub, and flags &= mask read only by a stub.
        ("dd_dead_store", {("dd_offsets", "discarded"), ("dd_mask", "discarded")}),
    ],
)
def test_a_value_nothing_reads_is_not_computed(
    dead_facts: dict[str, Any], function: str, left_out: set[tuple[str, str]]
) -> None:
    """llvm-objdump: each of these functions is a bare ret (or li a0, 0; ret)."""
    events = fact_events(dead_facts, function)
    assert (events["calls"], events["accesses"], events["field_accesses"]) == ([], [], [])
    assert left_out <= unevaluated(dead_facts, function)


def test_an_effect_or_a_loop_that_may_not_end_stays_code(dead_facts: dict[str, Any]) -> None:
    """llvm-objdump of dd_kept: jal obj_alloc, then the increment of dd_flag; dd_count's loop is gone."""
    assert callees(dead_facts, "dd_kept") == {"obj_alloc", "dd_wait"}
    assert ("dd_count", "discarded") in unevaluated(dead_facts, "dd_kept")
    assert ("dd_flag", "write") in {
        (item["unit"]["name"], item["access"]) for item in fact_events(dead_facts, "dd_kept")["accesses"]
    }


@pytest.mark.parametrize(
    ("function", "left_out"),
    [
        # dd_from passes its namespace only to dd_from_inner, which reads it only in sizeof.
        ("dd_transitive", {("dd_current_ns", "ignored_argument")}),
        # dd_stats reads its parameter only after `if (!dd_stats_enabled()) return;`.
        ("dd_dead_use", {("dd_boxes", "ignored_argument")}),
    ],
)
def test_an_argument_no_live_code_of_the_callee_reads_is_not_computed(
    dead_facts: dict[str, Any], function: str, left_out: set[tuple[str, str]]
) -> None:
    events = fact_events(dead_facts, function)
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert left_out <= unevaluated(dead_facts, function)


def test_an_argument_an_external_callee_with_an_effect_ignores_is_left_to_the_image(
    dead_facts: dict[str, Any], dead_build: dict[str, Any]
) -> None:
    """dd_reset_file stores, so its call stays; the image inlines it and forms no &dd_attr_dev."""
    (access,) = fact_events(dead_facts, "dd_reset")["accesses"]
    assert (access["unit"]["name"], access.get("conditional")) == ("dd_attr_dev", True)
    assert ("access", "global:dd_attr_dev") not in edges_from(dead_build["graph"], "function:dd_reset")
    decided = {
        (item["function"], item["target"], item["reason"])
        for item in dead_build["exclusions"]["items"]
        if item["function"] == "function:dd_reset"
    }
    assert ("function:dd_reset", "global:dd_attr_dev", "image_decided") in decided


def test_a_symbol_named_only_in_typeof_is_no_access(dead_facts: dict[str, Any]) -> None:
    """runtime_const_ptr(__dentry_cache): the address is patched into the instruction (j obj_free)."""
    accesses = fact_events(dead_facts, "dd_runtime_free")["accesses"]
    assert "dd_cache" not in {item["unit"]["name"] for item in accesses}
    assert ("dd_cache", "type") in unevaluated(dead_facts, "dd_runtime_free")


def test_a_field_read_under_a_builtin_choice_is_marked_conditional(dead_facts: dict[str, Any]) -> None:
    """rht_key_get_hash: the image of dd_builtin_field is mv a0, a1; ret."""
    (access,) = fact_events(dead_facts, "dd_builtin_field")["field_accesses"]
    assert (access["field"], access.get("conditional")) == ("size", True)


def test_the_dead_part_has_no_access_only_the_c_has(dead_build: dict[str, Any]) -> None:
    assert dead_build["result"].passes, dead_build["result"].hard_checks
    assert dead_build["checks"]["access_layer"]["pairs"]["only_c"] == []


def test_an_asm_with_an_output_and_without_volatile_goes_with_its_unread_output(dead_facts: dict[str, Any]) -> None:
    """RELOC_HIDE under this_cpu_ptr: llvm-objdump shows dd_asm_store as a bare ret."""
    events = fact_events(dead_facts, "dd_asm_store")
    assert (events["calls"], events["accesses"]) == ([], [])
    assert ("dd_offsets", "discarded") in unevaluated(dead_facts, "dd_asm_store")


def test_a_volatile_asm_or_one_without_an_output_stays(fct_facts: dict[str, Any]) -> None:
    """Each asm keeps its input; the value of the second statement expression is discarded."""
    accesses = fact_events(fct_facts, "fct_asm_kept")["accesses"]
    assert [(item["unit"]["name"], item["provenance"][0]["line"]) for item in accesses] == [
        ("fct_offsets_asm", 380),
        ("fct_offsets_asm", 381),
    ]
    assert ("fct_offsets_asm", "discarded") in {
        ((item.get("unit") or {}).get("name"), item["reason"])
        for item in fact_events(fct_facts, "fct_asm_kept")["unevaluated"]
    }


def test_a_store_through_an_array_parameter_is_an_effect(dead_facts: dict[str, Any]) -> None:
    """fast_mix(unsigned long s[4], ...): s is a pointer, so the call is code."""
    assert callees(dead_facts, "dd_array_parameter") == {"dd_mix"}
    assert ("dd_mix", "discarded") not in unevaluated(dead_facts, "dd_array_parameter")


def test_a_parameter_of_array_or_struct_type_the_callee_never_reads_is_ignored(dead_facts: dict[str, Any]) -> None:
    """free_cpumask_var(vec->mask) and is_mergeable_vm_userfaultfd_ctx(vma, vmg->uffd_ctx)."""
    events = fact_events(dead_facts, "dd_typed_parameters")
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert {("dd_holder", "ignored_argument")} <= unevaluated(dead_facts, "dd_typed_parameters")


def test_of_a_discarded_value_only_what_has_an_effect_is_computed(dead_facts: dict[str, Any]) -> None:
    """psi_sched_switch(..., !task_on_rq_queued(prev) || prev->se.sched_delayed): the volatile read
    stays (llvm-objdump: ld zero, 0x38(a0)), the read of the right operand of || does not."""
    # dd_switch is inline and has no call in the image: link folds it.
    assert callees(dead_facts, "dd_partial") == {"dd_switch", "dd_queued"}
    assert fact_events(dead_facts, "dd_partial")["field_accesses"] == []
    # An argument to a parameter the inline callee ignores: its leftover is an ignored argument.
    assert ("box", "ignored_argument") in unevaluated(dead_facts, "dd_partial")
    # The volatile read is the called helper's.
    assert {item["field"] for item in fact_events(dead_facts, "dd_queued")["field_accesses"]} == {"size"}


def test_an_argument_with_a_call_to_a_static_callee_that_ignores_it_is_left_to_the_image(
    dead_facts: dict[str, Any],
) -> None:
    """mm_init(mm, current, current_user_ns()): dd_init is static, not inline, and ignores ns. The call
    is marked, and link folds the helper's address of dd_init_ns under the mark."""
    events = fact_events(dead_facts, "dd_static_callee")
    (call,) = [call for call in events["calls"] if call["callee"] == "dd_current_ns"]
    assert call.get("conditional") is True
    assert callees(dead_facts, "dd_static_callee") == {"dd_init", "dd_current_ns"}


def test_a_generic_on_typedef_names_with_a_typeof_control_is_selected(dead_facts: dict[str, Any]) -> None:
    """local_lock(&radix_tree_preloads.lock): the local_lock_t association is selected, so the
    trylock store and __tl are not code (llvm-objdump: dd_generic_lock is a bare ret)."""
    events = fact_events(dead_facts, "dd_generic_lock")
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert ("dd_trylock", "generic_unselected") in unevaluated(dead_facts, "dd_generic_lock")


def test_a_local_read_only_where_the_image_decides_has_an_initial_value_the_image_decides(
    dead_facts: dict[str, Any],
) -> None:
    """free_area_init_core's nid = pgdat->node_id, read only by zone_init_internals, which ignores it."""
    (field,) = fact_events(dead_facts, "dd_conditional_local")["field_accesses"]
    # h->ctx.id: one access, of the top member, at ctx + id.
    assert (field["struct"], field["field"], field["displacement"], field.get("conditional")) == (
        "dd_ns",
        "id",
        16,
        True,
    )


def test_a_compound_assignment_whose_value_is_used_reads_its_variable(dead_facts: dict[str, Any]) -> None:
    """pcpu_dump_alloc_info's while (v /= 10): v is read, so v = num_possible_cpus() is code."""
    events = fact_events(dead_facts, "dd_digits")
    (field,) = events["field_accesses"]
    assert (field["field"], field["displacement"], field.get("conditional")) == ("id", 16, None)
    assert not [item for item in events["unevaluated"] if item["reason"] == "discarded"]


def test_an_argument_names_the_base_its_address_has(fct_facts: dict[str, Any]) -> None:
    """&local is the caller's own object; container_of(n, ...) is that far below the parameter."""
    (call,) = [call for call in fact_events(fct_facts, "fct_local_object")["calls"] if call["callee"] == "fct_fill"]
    local = call["arguments"][0]
    assert (local["base"], local["displacement"], local["param"]) == ("local", 0, None)
    container = call_argument(fct_facts, "fct_container_argument", "fct_x_of")
    assert (container["base"], container["displacement"], container["param"]) == ("container_of", -160, 0)


def test_pointer_arithmetic_on_a_member_array_keeps_its_base(fct_facts: dict[str, Any]) -> None:
    """o->slots + 1 is addi a0, a0, 0x70; o->slots + k - 1 is addi a0, s0, 0x50 after s0 = o + 16 * k."""
    calls = [call for call in fact_events(fct_facts, "fct_pass_step")["calls"] if call["callee"] == "fct_takes_inner"]
    constant, variable = (call["arguments"][0] for call in calls)
    assert (constant["param"], constant["displacement"], constant.get("element")) == (0, 112, None)
    assert (variable["param"], variable["displacement"], variable["element"]) == (0, 80, -16)
    # k is parameter 1: a caller that passes a constant adds 16 bytes per step.
    assert variable["index"] == [{"param": 1, "scale": 16}] and "index" not in constant


def test_a_global_is_named_through_an_array_element_and_a_helper_that_returns_it(fct_facts: dict[str, Any]) -> None:
    element = call_argument(fct_facts, "fct_pass_array_element", "fct_takes_inner")
    assert (element["unit"]["kind"], element["unit"]["name"], element["unit"]["field"]) == (
        "global",
        "fct_globals",
        "in",
    )
    assert (element["base"], element["displacement"]) == ("global", 184)
    held = call_argument(fct_facts, "fct_pass_helper_global", "obj_free")["unit"]
    assert (held["kind"], held["name"], held["field"]) == ("global", "fct_global", None)


def test_a_folded_helper_s_fields_take_the_base_the_caller_passed(fct_build: dict[str, Any]) -> None:
    """fct_fill(&local, v) is registers (addi a1, a0, 0x1; mulw): its fields are the caller's local
    object. fct_x_of(container_of(n, ...)) is ld a0, -0xa0(a0)."""
    accesses = fct_build["graph"]["analysis"]["field_accesses"]
    assert {(item["field"], item["base"]) for item in accesses["function:fct_local_object"]} == {
        ("a", "local"),
        ("b", "local"),
    }
    (container,) = accesses["function:fct_container_argument"]
    assert (container["field"], container["base"], container["displacement"]) == ("x", "container_of", -160)
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    assert not [item for item in offsets["misses"] if item["function"] == "function:fct_container_argument"]


def test_a_walked_parameter_keeps_its_base_and_the_member_address_it_steps_by(fct_facts: dict[str, Any]) -> None:
    """o = container_of(o->link.next, ...) in the walk: o->x is the parameter's x the first time, and
    the machine code may keep &o->link, 160 above it, as it steps."""
    (x,) = [item for item in fact_events(fct_facts, "fct_walk_entries")["field_accesses"] if item["field"] == "x"]
    assert (x["displacement"], x["param"], x["inner_displacement"]) == (0, 0, -160)


def test_a_lock_named_through_an_inline_helper_has_the_member_s_displacement(fct_facts: dict[str, Any]) -> None:
    """spin_lock(fct_check(&l->lock)) locks l + 8, as the image's addi a0, a0, 0x8."""
    found = {
        (item["field"], item["displacement"], item["base"])
        for item in fact_events(fct_facts, "fct_lock_checked")["field_accesses"]
    }
    assert found == {("lock", 8, "pointer")}


def test_a_local_object_whose_address_escapes_is_found_in_its_frame_slot(fct_build: dict[str, Any]) -> None:
    """fct_takes_inner(&local): the frame holds local; its fields are stored and loaded at the slot."""
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    assert not [item for item in offsets["misses"] if item["function"] == "function:fct_frame_object"]
    assert offsets["by_method"].get("frame_slot_access", 0) >= 3
    # c.addi sp, -0x20; c.swsp a0, 0x10(sp); c.swsp a1, 0x14(sp); c.addi4spn a0, sp, 8; c.ldsp a0, 8(sp):
    # offsets from sp on entry. The save and restore of ra at 0x18(sp) is the frame's own.
    image_facts = fct_build["graph"]["analysis"]["image"]
    assert image_facts["frame_accesses"]["function:fct_frame_object"] == [[-24, 8], [-16, 4], [-12, 4]]
    assert image_facts["frame_addresses"]["function:fct_frame_object"] == [-24]
    # c.addi4spn a0, sp, 8 before jal fct_takes_inner: the frame address the C's &local is.
    assert image_facts["frame_arguments"]["function:fct_frame_object"] == [["fct_takes_inner", 0, -24]]
    (passed,) = fct_build["graph"]["analysis"]["object_arguments"]["function:fct_frame_object"]
    assert passed[:2] == ["fct_takes_inner", 0] and passed[3] == 0
    assert not [
        item
        for item in fct_build["exclusions"]["items"]
        if item["function"] == "function:fct_frame_object" and item["kind"] == "field_access"
    ]


def test_every_field_access_the_check_cannot_hold_is_recorded(fct_build: dict[str, Any]) -> None:
    """A local object in registers, a zero-size member: each is an exclusions.json item, and the
    check's count of each reason is the number of items."""
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    items = [item for item in fct_build["exclusions"]["items"] if item["kind"] == "field_access"]
    assert offsets["excluded"] == dict(collections.Counter(item["reason"] for item in items))
    assert fct_build["checks"]["exclusions"]["field_access"] == offsets["excluded"]
    found = {(item["function"], item["reason"], item["detail"]["struct"], item["detail"]["field"]) for item in items}
    assert ("function:fct_local_object", "local_object_without_frame_slot", "fct_inner", "a") in found
    assert ("function:fct_zero_size", "zero_size_member", "fct_flags", "cookie") in found


def test_only_a_call_s_frame_argument_places_a_local_object() -> None:
    """Of the slots the calls give, the one that holds the object's bytes and the most fields; the
    fields alone place nothing; a slot above the frame's top, or too small for the object, is none."""
    accesses = {(-48, 8), (-40, 4), (-24, 8)}
    assert frames.fit({(0, 8)}, 8, accesses, {-24}) == -24
    assert frames.fit({(0, 8), (8, 4)}, 16, accesses, {-48, -24}) == -48
    assert frames.fit({(0, 8), (8, 4)}, 16, accesses, set()) is None
    assert frames.fit({(0, 8), (8, 4)}, 64, accesses, {-48}) is None
    assert frames.fit({(0, 8)}, 8, accesses, {8}) is None


def test_an_array_member_at_a_variable_index_is_held_where_the_frame_indexes_or_forms_it() -> None:
    """fbatch.folios[i] at slot -320 + 8: a store past an index added to -312, or a register formed
    at -312 that the code walks (kmem_dump_obj's kp_stack[i]); a plain field needs its own access."""
    item = {"frame_slot": -320, "displacement": 8, "size": 120, "element_displacement": 0}
    assert frames.method(item, set(), set(), {-312}) == "frame_slot_indexed_access"
    assert frames.method(item, set(), {-312}) == "frame_slot_address"
    plain = {"frame_slot": -320, "displacement": 8, "size": 8}
    assert frames.method(plain, set(), {-312}, {-312}) == "no_access_at_frame_slot"


def test_a_cleanup_of_a_struct_local_gets_the_local_s_address(fct_facts: dict[str, Any]) -> None:
    """fct_release(&held) runs when held's scope ends: its fields through the parameter are held's."""
    (call,) = [
        call for call in fact_events(fct_facts, "fct_cleanup_object")["calls"] if call["callee"] == "fct_release"
    ]
    (argument,) = call["arguments"]
    assert (argument["base"], argument["displacement"], argument["object"].split("@")[0]) == ("local", 0, "held")


def test_a_lock_guard_s_fields_are_the_guard_local_its_constructor_returns(fct_build: dict[str, Any]) -> None:
    """guard = class_fct_rq_constructor(p) returns its own _t, whose &_t.rf goes to fct_rq_lock; the
    destructor writes guard.rf.clock: one local object, which the frame holds where the call passes it.
    The constructor's own _t calls no destructor."""
    analysis = fct_build["graph"]["analysis"]
    accesses = analysis["field_accesses"]["function:fct_guarded"]
    assert {(item["field"], item["base"], item["object"].split("@")[0]) for item in accesses} == {
        ("flags", "local", "guard"),
        ("clock", "local", "guard"),
    }
    # &guard.rf to the lock call (the constructor's _t, renamed) and to the unlock call (the destructor).
    assert sorted(item[:2] for item in analysis["object_arguments"]["function:fct_guarded"]) == [
        ["fct_rq_lock", 1],
        ["fct_rq_unlock", 1],
    ]
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    assert not [item for item in offsets["misses"] if item["function"] == "function:fct_guarded"]


def _fields_of(facts: dict[str, Any], function: str) -> set[tuple[Any, ...]]:
    return {
        (item["field"], item["displacement"], item["base"], item.get("inner_displacement"))
        for item in fact_events(facts, function)["field_accesses"]
    }


def test_an_assignment_in_a_for_header_is_what_reaches_the_body(fct_facts: dict[str, Any]) -> None:
    """for (p = &init; (p = next(p)) != &init;): p->flags is 32 - 40 below next's tasks pointer. A
    second walk's init, not the first walk's values, reaches its body: o->in.a is 16 - 160."""
    assert ("flags", -8, "container_of", None) in _fields_of(fct_facts, "fct_each_task")
    walked = _fields_of(fct_facts, "fct_two_walks")
    assert ("x", -160, "container_of", None) in walked
    assert ("a", -144, "container_of", None) in walked


def test_a_statement_expression_s_null_select_gives_its_container(fct_facts: dict[str, Any]) -> None:
    """({ ...; is_link(n) ? container_of(n, ...) : NULL; }): o->x is 160 below n, or 0 from the selected
    register where nothing tests it first."""
    assert _fields_of(fct_facts, "fct_select_container") == {("x", -160, "container_of", 0)}


def test_a_helper_that_returns_null_or_a_container_gives_the_register_it_selects(fct_facts: dict[str, Any]) -> None:
    assert _fields_of(fct_facts, "fct_nullable_helper") == {("x", -160, "container_of", 0)}


def test_a_folded_callee_s_field_is_that_far_from_the_register_a_call_returns(fct_build: dict[str, Any]) -> None:
    """fct_res_free(fct_remove_res()): node.b is 12 into fct_res, which is 16 below the returned &r->data."""
    (item,) = fct_build["graph"]["analysis"]["field_accesses"]["function:fct_destroy"]
    assert (item["field"], item["base"], item["inner_displacement"]) == ("b", "container_of", -4)


def test_a_constant_helper_result_and_an_unwritten_static_decide_branches(fct_facts: dict[str, Any]) -> None:
    """fct_init_always always returns 0 (its -12 is under a false test); fct_never_set is always 0."""
    assert "x" not in {field for field, *_ in _fields_of(fct_facts, "fct_alloc_object")}
    assert "x" not in {field for field, *_ in _fields_of(fct_facts, "fct_maybe_use")}
    assert "fct_cur" in {call["callee"] for call in fact_events(fct_facts, "fct_maybe_use")["calls"]}


def test_gnu_conditional_with_an_omitted_middle_is_no_conversion_of_its_last_operand(fct_facts: dict[str, Any]) -> None:
    """fct_show(mask ?: fct_default_mask): the argument is the mask where it is set, not the static's 0."""
    argument = call_argument(fct_facts, "fct_report", "fct_show")
    assert (argument.get("expr") or {}).get("c") is None


def test_a_helper_first_folded_too_deep_still_folds_for_a_shallow_use(fct_facts: dict[str, Any]) -> None:
    """fct_d1 reaches fct_mapping four helpers deep before fct_off_later tests fct_file: !fct_file(o)
    still holds, so the free after the return is dead."""
    assert "obj_free" not in {call["callee"] for call in fact_events(fct_facts, "fct_off_later")["calls"]}


def test_a_pointer_to_either_of_two_locals_names_the_choice_and_is_held_at_its_slots(fct_build: dict[str, Any]) -> None:
    """p = k ? &a : &b; p->b = k: the store is through a register holding either frame address."""
    (item,) = fct_build["graph"]["analysis"]["field_accesses"]["function:fct_choice"]
    assert (item["base"], item["displacement"]) == ("local", 12)
    assert sorted(name.split("@")[0] for name in item["object"].split("|")) == ["a", "b"]
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    assert not [miss for miss in offsets["misses"] if miss["function"] == "function:fct_choice"]


def test_a_slot_is_scored_by_the_fields_its_accesses_cover() -> None:
    """force_sigsegv: si_signo and si_errno merged into one sd at -72, si_code a sw at -64; memset's
    interior address -64 is another call's slot, which holds one field exactly."""
    fields = {(0, 4), (4, 4), (8, 4)}
    assert frames.fit(fields, 48, {(-72, 8), (-64, 4)}, {-72, -64}) == -72


def test_each_declarator_of_a_declaration_has_its_own_cleanup_and_a_lock_names_its_pointer_field(
    fct_facts: dict[str, Any], fct_build: dict[str, Any]
) -> None:
    """thread_group_cputime's scoped_seqlock_read: the context pointer's cleanup is the empty
    fct_ss_cleanup_ctx, not the state's, so nothing reads fct_ss through (struct fct_ss *)&l->lock;
    spin_lock(fct_check(_s.lockp)) reads the local's lockp field, not an address it cannot tell."""
    events = fact_events(fct_facts, "fct_scoped_read")
    destructors = [call["callee"] for call in events["calls"] if call["callee"].startswith("fct_ss_cleanup")]
    assert sorted(destructors) == ["fct_ss_cleanup", "fct_ss_cleanup_ctx"]
    found = {(item["field"], item["base"]) for item in events["field_accesses"] if item["struct"] == "fct_ss"}
    assert ("lockp", "local") in found and ("lockp", "unknown") not in found
    folded = fct_build["graph"]["analysis"]["field_accesses"]["function:fct_scoped_read"]
    assert {item["base"] for item in folded if item["struct"] == "fct_ss"} == {"local"}


def test_a_call_whose_copy_dropped_a_parameter_places_its_objects_one_register_earlier(
    fct_build: dict[str, Any],
) -> None:
    """fct_queue(mask, 7, &ms, &done): the image's static copy dropped fn, so &ms is in a1 and &done
    in a2. The C positions 2 and 3 are matched to registers 1 and 2: ms's fields are held at its slot."""
    analysis = fct_build["graph"]["analysis"]
    given = analysis["image"]["frame_arguments"]["function:fct_shifted"]
    assert sorted(position for callee, position, _ in given if callee == "fct_queue") == [1, 2]
    offsets = fct_build["checks"]["structure_layer"]["field_offsets"]
    assert not [miss for miss in offsets["misses"] if miss["function"] == "function:fct_shifted"]
    excluded = [item for item in fct_build["exclusions"]["items"] if item["function"] == "function:fct_shifted"]
    assert not [item for item in excluded if item["detail"]["struct"] == "fct_ms"]


def test_the_shift_of_a_call_s_arguments_is_the_one_every_object_argument_agrees_on() -> None:
    """Positions 2 and 3 in the C, registers 1 and 2 in the image: a shift of 1. With position 3 in a
    register, no shift; with no shift every position fits, none is taken."""
    analysis = {"object_arguments": {"f": [["q", 2, "ms", 0], ["q", 3, "done", 0]]}}
    image = {"frame_arguments": {"f": [["q", 1, -128], ["q", 2, -88]]}}
    assert frames.evidence("ms", "f", analysis, image) == {-128}
    assert frames.evidence("done", "f", analysis, image) == {-88}
    image = {"frame_arguments": {"f": [["q", 2, -128], ["q", 3, -88]]}}
    assert frames.evidence("ms", "f", analysis, image) == {-128}
    image = {"frame_arguments": {"f": [["q", 2, -128]]}}
    assert frames.evidence("ms", "f", analysis, image) == {-128}


def test_arguments_past_the_named_parameters_of_a_callee_that_never_reads_them_are_not_computed(
    dead_facts: dict[str, Any],
) -> None:
    """cpu_capacity_show's sysfs_emit(buf, "%lu\\n", topology_get_cpu_scale(...)) without CONFIG_SYSFS."""
    events = fact_events(dead_facts, "dd_variadic")
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert ("box", "ignored_argument") in unevaluated(dead_facts, "dd_variadic")
    # A callee that reads its arguments with va_arg keeps them.
    assert {item["field"] for item in fact_events(dead_facts, "dd_variadic_used")["field_accesses"]} == {"size"}


def test_an_if_whose_branches_have_no_effect_is_not_code(dead_facts: dict[str, Any]) -> None:
    """__device_attach's if (dev->parent) pm_runtime_get_sync(dev->parent) without CONFIG_PM."""
    events = fact_events(dead_facts, "dd_if_without_effect")
    assert (events["calls"], events["accesses"], events["field_accesses"]) == ([], [], [])
    assert ("box", "discarded") in unevaluated(dead_facts, "dd_if_without_effect")
    # A branch with an effect keeps the condition.
    assert {item["field"] for item in fact_events(dead_facts, "dd_if_with_effect")["field_accesses"]} == {"first"}
    assert callees(dead_facts, "dd_if_with_effect") == {"obj_free"}


def test_a_value_masked_with_zero_is_never_read(dead_facts: dict[str, Any]) -> None:
    """IS_DAX(inode) without CONFIG_FS_DAX: inode->i_flags & 0 (llvm-objdump: li a0, 0; ret)."""
    events = fact_events(dead_facts, "dd_masked")
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert ("box", "discarded") in unevaluated(dead_facts, "dd_masked")


@pytest.mark.parametrize("function", ["dd_dead_effect", "dd_overflow"])
def test_a_helper_whose_effects_are_not_compiled_has_none(dead_facts: dict[str, Any], function: str) -> None:
    """An asm under IS_ENABLED of an unset option, and __builtin_mul_overflow into the helper's local,
    are no effects (llvm-objdump: both callers are a bare ret)."""
    events = fact_events(dead_facts, function)
    assert (events["calls"], events["field_accesses"]) == ([], [])
    assert {item.get("call") for item in events["unevaluated"] if item["reason"] == "discarded"} != set()


def test_a_helper_with_a_live_asm_volatile_is_kept(dead_facts: dict[str, Any]) -> None:
    assert callees(dead_facts, "dd_live_effect") == {"dd_read_fenced"}


@pytest.mark.parametrize("function", ["dd_absorbed_or", "dd_absorbed_and"])
def test_an_operand_the_other_operand_of_and_or_or_decides_is_not_read(
    dead_facts: dict[str, Any], function: str
) -> None:
    """can_demote's !pgdat || !numa_demotion_enabled without NUMA (llvm-objdump: li a0, 0; ret)."""
    events = fact_events(dead_facts, function)
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert ("box", "discarded") in unevaluated(dead_facts, function)


def test_an_operand_that_decides_whether_the_other_one_runs_is_read(dead_facts: dict[str, Any]) -> None:
    """pagetable_pte_ctor's mm != &init_mm && !ptlock_init(ptdesc): ptlock_init always gives true but
    initializes the lock, so the left operand decides whether it runs (llvm-objdump: ld, beqz, sd)."""
    events = fact_events(dead_facts, "dd_absorbed_effect")
    assert [(item["field"], item["param"]) for item in events["field_accesses"]] == [("first", 1)]
    assert ("box", "discarded") not in unevaluated(dead_facts, "dd_absorbed_effect")


def fields_read(facts: dict[str, Any], function: str) -> set[tuple[str, int | None]]:
    """(the field, the parameter it is reached from) of each field access of the function."""
    return {(item["field"], item.get("param")) for item in fact_events(facts, function)["field_accesses"]}


def test_the_return_address_only_computes_a_value(dead_facts: dict[str, Any]) -> None:
    """kmalloc_nolock_noprof's s = kmalloc_slab(size, NULL, alloc_gfp, _RET_IP_), read only by a stub
    (llvm-objdump: li a0, 0; ret)."""
    assert fields_read(dead_facts, "dd_return_address") == set()
    assert ("dd_slab", "discarded") in unevaluated(dead_facts, "dd_return_address")


def test_a_struct_local_no_live_code_reads_is_not_computed(dead_facts: dict[str, Any]) -> None:
    """rcu_start_exp_gp_kworker's param = { .sched_priority = kthread_prio }, used only under
    IS_ENABLED(CONFIG_RCU_EXP_KTHREAD) (llvm-objdump: ret)."""
    assert fact_events(dead_facts, "dd_struct_unread")["accesses"] == []
    assert ("dd_flag", "discarded") in unevaluated(dead_facts, "dd_struct_unread")


def test_a_static_local_initializer_is_data(dead_facts: dict[str, Any]) -> None:
    """string_get_size's units_str = { units_10, units_2 }: the image's code addresses only units_str;
    the table's addresses are a data edge of units_str."""
    names = {item["unit"]["name"] for item in fact_events(dead_facts, "dd_unit")["accesses"]}
    assert names == {"dd_unit.units_str"}
    edges = {
        (item["global"]["name"], item["offset"], item["target"]["name"])
        for item in dead_facts["global_initializers"]
        if item["global"]["name"] == "dd_unit.units_str"
    }
    assert edges == {("dd_unit.units_str", 0, "dd_unit.units_10"), ("dd_unit.units_str", 8, "dd_unit.units_2")}


def test_a_loop_that_stores_only_its_own_cursor_is_not_compiled(dead_facts: dict[str, Any]) -> None:
    """flush_all_rcu_sheaves' list_for_each_entry under CONFIG_SLUB_TINY (llvm-objdump: j obj_free)."""
    events = fact_events(dead_facts, "dd_loop_without_effect")
    assert (events["field_accesses"], [item["access"] for item in events["accesses"]]) == ([], ["free"])
    assert [call["callee"] for call in events["calls"]] == ["obj_free"]
    # A loop whose cursor is read after it computes that value.
    assert fact_events(dead_facts, "dd_loop_cursor_read")["field_accesses"]


@pytest.mark.parametrize(
    "function", ["dd_same_outcome", "dd_same_outcome_void", "dd_same_outcome_call", "dd_same_outcome_local"]
)
def test_an_if_whose_two_outcomes_agree_is_not_code(dead_facts: dict[str, Any], function: str) -> None:
    """rcutree_online_cpu, clk_pm_runtime_put, clk_pm_runtime_get and vmstat_cpu_dead: the return
    after the condition and the code after the if give the same, without effect (llvm-objdump: li a0,
    0; ret, or ret)."""
    assert fields_read(dead_facts, function) == set()
    assert ("box", "discarded") in unevaluated(dead_facts, function)


def test_an_if_whose_outcomes_differ_keeps_its_condition(dead_facts: dict[str, Any]) -> None:
    assert fields_read(dead_facts, "dd_other_outcome") == {("size", 0)}


def test_a_branch_whose_only_jump_is_dead_has_no_effect(dead_facts: dict[str, Any]) -> None:
    """clk_core_is_enabled's if (core->rpm_enabled) { ...; if (!pm_runtime_active(dev)) { ret = false;
    goto done; } } without CONFIG_PM: pm_runtime_active gives true, so the goto is not code."""
    assert fields_read(dead_facts, "dd_dead_jump") == set()


@pytest.mark.parametrize(
    ("function", "read"), [("dd_overwritten", {("first", 1), ("hash", 1)}), ("dd_redefined", {("first", 1)})]
)
def test_a_store_overwritten_before_any_read_is_not_computed(
    dead_facts: dict[str, Any], function: str, read: set[tuple[str, int]]
) -> None:
    """ipi_mux_send_mask's icpu = this_cpu_ptr(ipi_mux_pcpu), stored again at the top of the loop, and
    wait_task_inactive's rq = task_rq(p), read only by task_on_cpu, which ignores it."""
    assert fields_read(dead_facts, function) == read


def test_a_store_a_later_read_may_see_keeps_its_read(dead_facts: dict[str, Any]) -> None:
    assert fields_read(dead_facts, "dd_not_redefined") == {("first", 0), ("first", 1), ("size", 1)}


def test_a_call_whose_loops_may_be_assumed_to_end_goes_with_its_unused_value(dead_facts: dict[str, Any]) -> None:
    """fwnode_links_check_suppliers' list_for_each_entry, written out and by a macro."""
    events = fact_events(dead_facts, "dd_unused_loop_call")
    assert (events["calls"], events["accesses"], events["field_accesses"]) == ([], [], [])
    assert {"dd_count_links", "dd_count_links_macro"} <= {
        item.get("call") for item in events["unevaluated"] if item["reason"] == "discarded"
    }


def test_a_store_nothing_reads_in_a_loop_body_or_an_if_arm_is_not_code(dead_facts: dict[str, Any]) -> None:
    """scoped_guard(mutex, &lock) val = ...: the store is the body of a for loop. An if whose only
    work is such a store has no effect either (llvm-objdump: dd_unused_in_body is a bare ret)."""
    events = fact_events(dead_facts, "dd_unused_in_body")
    assert (events["accesses"], events["field_accesses"]) == ([], [])
    assert ("box", "discarded") in unevaluated(dead_facts, "dd_unused_in_body")


def test_an_expression_statement_that_is_a_branch_is_discarded(dead_facts: dict[str, Any]) -> None:
    """llvm-objdump of dd_if_mixed: ld first; beqz; j obj_free. The other branch reads nothing."""
    events = fact_events(dead_facts, "dd_if_mixed")
    assert {item["field"] for item in events["field_accesses"]} == {"first"}
    assert callees(dead_facts, "dd_if_mixed") == {"obj_free"}
    assert ("dd_read_checked", "discarded") in unevaluated(dead_facts, "dd_if_mixed")


def test_an_answer_cut_at_the_depth_bound_is_kept_for_as_deep_a_horizon(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """folio_put -> put_page_testzero -> ... -> arch_atomic_fetch_sub: a chain deeper than the bound.
    An answer computed where the bound cut the chain below is kept and reused wherever it is asked at
    least as deep, so a link asked twice per walk above it is not analyzed again and again (mm/vmscan.c
    did not finish in 590 s, after 392135 walks, when such answers were dropped; 11 s, 707 walks, when
    kept)."""
    monkeypatch.setattr(clangfacts, "_EFFECT_DEPTH", 3)
    lines = ["static inline long dd_link3(long n) { return n; }"]
    for index in (2, 1, 0):
        lines.append(f"static inline long dd_link{index}(long n) {{ return dd_link{index + 1}(n) + 1; }}")
    (tmp_path / "chain.c").write_text("\n".join(lines) + "\n")
    loaded = config.load(FIXTURE / "cgraph.toml")
    arguments = ["--target=riscv64-unknown-elf", "-std=gnu11", "-ffreestanding", "-nostdinc"]
    tu = ci.Index.create().parse(str(tmp_path / "chain.c"), args=arguments)
    extractor = clangfacts._Extractor(tu, "chain.c", tmp_path, loaded.vocabulary)
    extractor.function_effect_free("dd_link0")
    # Asked at nesting 0, dd_link0 saw dd_link1 at 1 and dd_link2 at 2, which could not follow
    # dd_link3 past the bound of 3.
    assert extractor.effect_free_cache["dd_link0"] == (False, 0)
    assert extractor.effect_free_cache["dd_link1"] == (False, 1)
    assert extractor.effect_free_cache["dd_link2"] == (False, 2)
    extractor.nesting = 1
    assert extractor.reuse(extractor.effect_free_cache, "dd_link1") is False
    # Asked less deep than its horizon, the answer is computed again with more of the chain in view.
    extractor.nesting = 0
    assert extractor.reuse(extractor.effect_free_cache, "dd_link1") is None
    assert extractor.function_effect_free("dd_link1") is True
    assert extractor.effect_free_cache["dd_link1"] == (True, None)
    # An answer that reuses one kept for a horizon is kept for no wider horizon itself.
    extractor = clangfacts._Extractor(tu, "chain.c", tmp_path, loaded.vocabulary)
    extractor.nesting = 2
    extractor.function_effect_free("dd_link2")
    extractor.nesting = 1
    extractor.function_effect_free("dd_link1")
    assert extractor.effect_free_cache["dd_link1"] == (False, 1)
    extractor.nesting = 0
    assert extractor.function_effect_free("dd_link1") is True
