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
