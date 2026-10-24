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
    assert (argument["param"], argument["displacement"]) == (0, 8)
    # The address of a global's storage is no offset from a parameter.
    assert "displacement" not in call_argument(fct_facts, "fct_pass_held_global", "fct_takes_inner")


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
