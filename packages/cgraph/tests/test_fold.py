"""Code whose facts depend on what the code around it does with it (src/fold.c), mostly helpers the
image inlines as link folds them into their callers."""

from __future__ import annotations

from typing import Any

import pytest
from conftest import FIXTURE, build, read

from cgraph import clangfacts, commands, config


@pytest.fixture(scope="module")
def fold_facts() -> dict[str, Any]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    source = "src/fold.c"
    return clangfacts.extract(
        source,
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source),
        loaded.vocabulary,
    )


@pytest.fixture(scope="module")
def fold_build(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    out = tmp_path_factory.mktemp("fold")
    result = build(FIXTURE, "fold", out)
    return {"result": result, **{name: read(out, f"{name}.json") for name in ("graph", "checks", "exclusions")}}


def test_the_fold_part_passes_its_hard_checks(fold_build: dict[str, Any]) -> None:
    assert fold_build["result"].passes, fold_build["result"].hard_checks


def folded_fields(build: dict[str, Any], function: str) -> set[str]:
    """The fields of a function's machine code once link folds the helpers the image inlines."""
    accesses = build["graph"]["analysis"]["field_accesses"].get(f"function:{function}", [])
    return {item["field"] for item in accesses}


def test_every_field_offset_of_the_fold_part_matches_the_image(fold_build: dict[str, Any]) -> None:
    misses = fold_build["checks"]["structure_layer"]["field_offsets"]["misses"]
    assert [(item["function"], item["struct"], item["field"], item["offset"]) for item in misses] == []


@pytest.mark.parametrize(("function", "fields"), [("fd_find", {"ns"}), ("fd_check_all", {"v", "low"})])
def test_a_loop_that_may_leave_the_function_is_code(
    fold_facts: dict[str, Any], fold_build: dict[str, Any], function: str, fields: set[str]
) -> None:
    """__aggregate_find's list_for_each_entry returns its cursor; validate_prctl_map_addr's loop jumps
    out. llvm-objdump: fd_find loads n->next and ns (-8 from the list node); fd_check_all loads low
    and v[i]."""
    own = {item["field"] for item in fold_facts["functions"][function]["events"]["field_accesses"]}
    assert fields <= own
    assert fields <= folded_fields(fold_build, function)


def test_the_label_a_loop_jumps_to_is_code(fold_facts: dict[str, Any], fold_build: dict[str, Any]) -> None:
    """fd_check_all's goto out in the loop is code, so out: and its obj_free are (llvm-objdump: the
    blt to the label, then tail obj_free)."""
    assert "obj_free" in {call["callee"] for call in fold_facts["functions"]["fd_check_all"]["events"]["calls"]}
    graph = fold_build["graph"]
    edges = {(edge["kind"], edge["target"]) for edge in graph["edges"] if edge["source"] == "function:fd_check_all"}
    assert ("call", "function:obj_free") in edges


def call_of(facts: dict[str, Any], function: str, callee: str) -> dict[str, Any]:
    (call,) = [call for call in facts["functions"][function]["events"]["calls"] if call["callee"] == callee]
    return call


@pytest.mark.parametrize(
    ("function", "callee", "kept", "left_out"),
    [
        # llvm-objdump: ld zero, 0x0(a0); ret.
        ("fd_unused_order", "fd_folio_order", {"flags"}, {"order"}),
        # lw a0, 0x0(a0); li a1, 0x1; blt a1, a0, +4; ebreak; ret.
        ("fd_unused_clock", "fd_rq_clock", {"flags"}, {"clock"}),
        # lw a1, 0x0(a0); beqz a1, +4; ret; lw zero, 0x4(a0); ret.
        ("fd_unused_entity", "fd_dl_rq_of_se", {"server", "cpu"}, {"rq", "rqs", "dl"}),
    ],
)
def test_a_call_whose_value_is_unused_keeps_only_what_the_callee_s_effects_need(
    fold_facts: dict[str, Any],
    fold_build: dict[str, Any],
    function: str,
    callee: str,
    kept: set[str],
    left_out: set[str],
) -> None:
    assert call_of(fold_facts, function, callee).get("value_unused") is True
    fields = folded_fields(fold_build, function)
    assert kept <= fields and not fields & left_out


def test_a_call_whose_value_is_used_keeps_the_reads_that_make_it(
    fold_facts: dict[str, Any], fold_build: dict[str, Any]
) -> None:
    """llvm-objdump of fd_used_order: ld a1, 0x0(a0); andi; beqz; lbu a0, 0x8(a0)."""
    assert "value_unused" not in call_of(fold_facts, "fd_used_order", "fd_folio_order")
    assert folded_fields(fold_build, "fd_used_order") == {"flags", "order"}


@pytest.mark.parametrize(
    ("function", "fields"),
    [
        # ld a0, 0x40(a0): ns, from the parameter; m at 32 is never formed.
        ("fd_through_helper", {"ns"}),
        # ld a0, -0x8(a0): ns, 72 below the list node.
        ("fd_from_list", {"ns"}),
        # ld a0, 0x8(a0); lw a0, 0x18(a0): the donor, then its cpu.
        ("fd_donor_cpu", {"donor", "cpu"}),
        # ld a0, 0x8(a0); ld a0, 0x40(a0): the owner, then its ns.
        ("fd_returned", {"owner", "ns"}),
    ],
)
def test_a_member_address_a_container_of_takes_back_is_not_formed(
    fold_build: dict[str, Any], function: str, fields: set[str]
) -> None:
    assert folded_fields(fold_build, function) == fields


@pytest.mark.parametrize(
    ("function", "fields"),
    [
        # ld a0, 0x20(a0): m.a[0], at the member's address.
        ("fd_member_read", {"m", "a"}),
        # ld a0, 0x8(a0); ld a0, 0x28(a0): the owner, then m.a[1].
        ("fd_returned_read", {"owner", "m", "a"}),
        # ld a0, 0x28(a0): a container_of of member a, not of b, so &t->b is kept (b.a[1] is inside it).
        ("fd_not_taken_back", {"b", "a"}),
    ],
)
def test_a_member_address_read_through_stays(fold_build: dict[str, Any], function: str, fields: set[str]) -> None:
    assert folded_fields(fold_build, function) == fields


@pytest.mark.parametrize(("function", "record"), [("fd_index_kept", ("f", 16, 40)), ("fd_index_folded", ("h", 8, 32))])
def test_an_index_s_constant_may_stay_in_the_index_register(
    fold_build: dict[str, Any], function: str, record: tuple[str, int, int]
) -> None:
    """t->act[k - 1].f: sd 0x28 where the register keeps k - 1 for a shift; t->act[k - 1].h: sd 0x8
    where the constant folds into the store (llvm-objdump). The record offers both, and each matches."""
    (item,) = fold_build["graph"]["analysis"]["field_accesses"][f"function:{function}"]
    assert (item["field"], item["displacement"], item["index_displacement"]) == record
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert f"function:{function}" not in {entry["function"] for entry in offsets["misses"]}


def test_a_pointer_a_loop_steps_points_where_its_start_does_on_the_first_pass(fold_build: dict[str, Any]) -> None:
    """fd_zone_sum: addi a1, a2, -0x800; addi a1, a1, -0x5f8 (the list node less 3712 plus 136), ld
    a3, 0x0(a1), then addi a1, a1, 0x4c0 (llvm-objdump)."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_zone_sum"]
    (item,) = [item for item in accesses if item["field"] == "managed"]
    assert (item["field"], item["base"], item["displacement"], item["inner_displacement"]) == (
        "managed",
        "container_of",
        -3576,
        136,
    )


def test_a_value_shifted_out_is_not_read(fold_facts: dict[str, Any], fold_build: dict[str, Any]) -> None:
    """fd_position: ld a0, 0x0(a0); ret (llvm-objdump): a4 is never loaded."""
    own = {item["field"] for item in fold_facts["functions"]["fd_position"]["events"]["field_accesses"]}
    assert own == {"a3"}
    assert folded_fields(fold_build, "fd_position") == {"a3"}


def test_an_if_without_effect_under_a_label_is_not_code(fold_build: dict[str, Any]) -> None:
    """fd_label: beqz a1, +4; ret; sd zero, 0x8(a0); ret (llvm-objdump): hot is never read."""
    assert folded_fields(fold_build, "fd_label") == {"n"}


def test_a_variable_less_its_copy_is_not_read(fold_build: dict[str, Any]) -> None:
    """fd_copy: mv a0, a1; ret (llvm-objdump): index - start is 0, and index's other stores are not
    compiled."""
    assert folded_fields(fold_build, "fd_copy") == set()


@pytest.mark.parametrize(("function", "fields"), [("fd_set_high", {"type"}), ("fd_set_low", {"disabled", "type"})])
def test_an_operand_the_arguments_make_unneeded_is_not_read(
    fold_build: dict[str, Any], function: str, fields: set[str]
) -> None:
    """fd_set_type(b, 3): b->disabled && 3 < 3 is 0 (llvm-objdump: li a1, 0x3; sw a1, 0x4(a0)); with 1
    the code reads disabled."""
    assert folded_fields(fold_build, function) == fields


def test_a_value_whose_only_effects_the_image_decides_is_left_to_the_image(
    fold_facts: dict[str, Any], fold_build: dict[str, Any]
) -> None:
    """fd_cpu_dead: li a0, 0x0; ret (llvm-objdump). fd_empty's asm is only beside its
    __builtin_constant_p path, so the call and the mask are the image's to decide."""
    events = fold_facts["functions"]["fd_cpu_dead"]["events"]
    assert {call["callee"]: call.get("conditional") for call in events["calls"]} == {"fd_empty": True}
    assert {(item["field"], item.get("conditional")) for item in events["field_accesses"]} == {("bits", True)}
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_cpu_dead" not in {entry["function"] for entry in offsets["misses"]}


@pytest.mark.parametrize("function", ["fd_mkclean", "fd_mkclean_read", "fd_mkclean_called", "fd_migrate"])
def test_a_value_stored_only_into_a_local_nothing_reads_is_left_to_the_image(
    fold_build: dict[str, Any], function: str
) -> None:
    """fd_mkclean: ld a0, 0x0(a0); tail obj_free (llvm-objdump): range is never read, so the value
    fd_address_end gives it, and that helper's read of nr, are compiled only where the image calls the
    callee that stores it: they are left to the image, which here has no read of nr. fd_mkclean_called
    calls fd_range_fenced out of line, and jal fd_address_end before it: the image's call is the
    argument's. fd_mkclean_read returns range.end, which needs nr: ld a1, 0x8(a0). fd_migrate reads
    range.end only to store it back into range (try_to_migrate_one), so nr is left to the image."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"][f"function:{function}"]
    nr = {bool(item.get("conditional")) for item in accesses if item["field"] == "nr"}
    expected = {"fd_mkclean": {True}, "fd_mkclean_read": {False}, "fd_mkclean_called": set(), "fd_migrate": {True}}
    assert nr == expected[function]
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert f"function:{function}" not in {entry["function"] for entry in offsets["misses"]}
    edges = {
        (edge["kind"], edge["target"])
        for edge in fold_build["graph"]["edges"]
        if edge["source"] == f"function:{function}"
    }
    assert (("call", "function:fd_address_end@src/fold.c") in edges) == (function == "fd_mkclean_called")
    # The image's call of fd_address_end is the C's: no call only the image makes.
    pairs = fold_build["checks"]["call_layer"]["pairs"]
    assert f"function:{function}" not in {pair["source"] for pair in pairs["only_image"] + pairs["only_c"]}


def test_a_member_address_a_generic_gives_may_stay_in_a_register(fold_build: dict[str, Any]) -> None:
    """fd_owner_head: &fd_to_common(u)->root.head is 256 + 16 from u, or 16 from the register that
    holds &u->ns (do_listns_userns' addi s1, s6, 0xf0)."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_owner_head"]
    (item,) = [item for item in accesses if item["field"] == "head"]
    assert (item["displacement"], item.get("inner_displacement")) == (272, 16)
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_owner_head" not in {entry["function"] for entry in offsets["misses"]}


def test_an_unused_value_at_a_site_inlined_beside_a_call_keeps_only_what_the_image_shows(
    fold_facts: dict[str, Any], fold_build: dict[str, Any]
) -> None:
    """fd_partial calls fd_part (jal fd_part) and inlines it for the statement whose value is unused:
    the volatile read and jal ext_check, not box_size. The image calls the helper, so link keeps the
    call and takes from the helper's events only the calls the image makes that the caller's C does
    not; box_size is no call of fd_partial."""
    calls = [call for call in fold_facts["functions"]["fd_partial"]["events"]["calls"] if call["callee"] == "fd_part"]
    assert sorted(bool(call.get("value_unused")) for call in calls) == [False, True]
    edges = {
        (edge["kind"], edge["target"])
        for edge in fold_build["graph"]["edges"]
        if edge["source"] == "function:fd_partial"
    }
    assert {("call", "function:fd_part@src/fold.c"), ("call", "function:ext_check")} <= edges
    assert not any("box_size" in target for _, target in edges)


def test_a_store_no_pass_of_a_loop_that_runs_once_reads_is_not_computed(fold_build: dict[str, Any]) -> None:
    """fd_enqueue (llvm-objdump): on_rq and slice through se, never q->curr: each slice =
    fd_min_slice(q) is read by no pass of a for (; se; se = NULL) loop."""
    assert folded_fields(fold_build, "fd_enqueue") == {"on_rq", "slice"}


def test_a_store_a_break_carries_to_the_next_loop_stays(fold_build: dict[str, Any]) -> None:
    """fd_enqueue_break: the break after slice = fd_min_slice(q) takes it to the second loop."""
    assert {"curr", "on", "sl"} <= folded_fields(fold_build, "fd_enqueue_break")


def test_a_pointer_a_loop_steps_from_a_pointer_stays_a_pointer(fold_build: dict[str, Any]) -> None:
    """fd_table_sum walks from its parameter: managed is 136 from the stepped register, which is no
    longer the parameter after the first pass."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_table_sum"]
    (item,) = [item for item in accesses if item["field"] == "managed"]
    assert (item["base"], item["displacement"], item.get("param")) == ("pointer", 136, None)


@pytest.mark.parametrize("function", ["fd_print_offline", "fd_print_each", "fd_make_vfsuid"])
def test_a_condition_whose_outcomes_store_alike_is_not_read(fold_build: dict[str, Any], function: str) -> None:
    """fd_print_offline and fd_print_each: len += 0 changes nothing, so neither the if nor the loop is
    code (llvm-objdump: li a0, 0x0; ret). fd_make_vfsuid: both branches store k.val, so ns->level is
    not read."""
    assert not folded_fields(fold_build, function) & {"n", "total", "count", "level"}


def test_a_loop_whose_effects_the_image_decides_is_left_to_the_image(fold_build: dict[str, Any]) -> None:
    """fd_report_mult: j obj_free (llvm-objdump). The loop's lo and hi reads are the image's to decide."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_report_mult"]
    assert {(item["field"], bool(item.get("conditional"))) for item in accesses} == {("lo", True), ("hi", True)}
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_report_mult" not in {entry["function"] for entry in offsets["misses"]}


def test_a_prefetched_address_is_left_to_the_image(fold_build: dict[str, Any]) -> None:
    """fd_prefetch: ret (llvm-objdump). What the prefetches are given is the image's to decide."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_prefetch"]
    assert {(item["field"], bool(item.get("conditional"))) for item in accesses} == {("curr", True), ("sl", True)}
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_prefetch" not in {entry["function"] for entry in offsets["misses"]}


def test_a_loop_whose_only_effects_are_in_its_init_keeps_only_those(fold_build: dict[str, Any]) -> None:
    """fd_report_leaves: lw level; bnez ebreak; j obj_free (llvm-objdump): the init's WARN stays, the
    loop and the locals its init stores go."""
    assert folded_fields(fold_build, "fd_report_leaves") == {"level"}


def test_a_loop_past_its_init_whose_effects_the_image_decides_is_left_to_the_image(fold_build: dict[str, Any]) -> None:
    """fd_report_leaves_image (llvm-objdump: lw level; ebreak; j obj_free): every field is the image's
    to decide, and none misses."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_report_leaves_image"]
    assert {(item["field"], bool(item.get("conditional"))) for item in accesses} == {
        ("level", True),
        ("lo", True),
        ("hi", True),
    }
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_report_leaves_image" not in {entry["function"] for entry in offsets["misses"]}


def test_a_copy_of_no_bytes_is_not_code(fold_build: dict[str, Any]) -> None:
    """fd_area_copy: ld a2, 0x0(a1); sd a2, 0x0(a0); ret (llvm-objdump): the copy of the empty uffd
    is gone with its arguments."""
    assert folded_fields(fold_build, "fd_area_copy") == {"start"}


@pytest.mark.parametrize(
    ("function", "fields"), [("fd_add_class", {"remove"}), ("fd_add_class_kept", {"stats", "code", "remove"})]
)
def test_a_store_control_cannot_carry_to_a_read_is_not_code(
    fold_build: dict[str, Any], function: str, fields: set[str]
) -> None:
    """fd_add_class (llvm-objdump): box_size, then remove, never stats or enc: error = 0 reaches only
    return 0. In fd_add_class_kept a goto after the store takes its value, t->code, to return error."""
    assert folded_fields(fold_build, function) == fields


@pytest.mark.parametrize(
    ("function", "fields"),
    [("fd_hide_tag", set()), ("fd_remap_prepare", set()), ("fd_remap_changed", {"pfn"})],
)
def test_a_store_of_what_is_there_is_not_code(fold_build: dict[str, Any], function: str, fields: set[str]) -> None:
    """fd_hide_tag: ret; fd_remap_prepare: j obj_free (llvm-objdump). Each store keeps what the field
    holds, so it, its reads and the switch around it are gone. fd_remap_changed stores 0 between, so
    its loads and stores of pfn stay."""
    assert folded_fields(fold_build, function) == fields


def test_two_returns_of_one_value_decide_nothing(fold_build: dict[str, Any]) -> None:
    """fd_from_vfsuid (llvm-objdump): box_size and its compare, never ns->level: both returns give
    (fd_kuid_t){ uid }."""
    assert folded_fields(fold_build, "fd_from_vfsuid") == set()


def test_operands_past_the_returned_value_decide_nothing(fold_build: dict[str, Any]) -> None:
    """fd_dma_configure (llvm-objdump): box_size, then ret alone; drv and managed are the image's to
    decide, as both ways give 0 once ret is 0."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_dma_configure"]
    assert {(item["field"], bool(item.get("conditional"))) for item in accesses} == {("drv", True), ("managed", True)}
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_dma_configure" not in {entry["function"] for entry in offsets["misses"]}


def test_an_assignment_does_not_reach_its_own_right_side(fold_build: dict[str, Any]) -> None:
    """fd_class_to_subsys (llvm-objdump: addi a0, s0, -0x8; jal obj_free): sp = fd_subsys_get(sp)
    reads the walk's container_of, so &sp->subsys.kobj is the list node less 8."""
    accesses = fold_build["graph"]["analysis"]["field_accesses"]["function:fd_class_to_subsys"]
    (item,) = [item for item in accesses if item["field"] == "kobj"]
    assert (item["base"], item["displacement"]) == ("container_of", -8)
    offsets = fold_build["checks"]["structure_layer"]["field_offsets"]
    assert "function:fd_class_to_subsys" not in {entry["function"] for entry in offsets["misses"]}
