"""The image layer: calls, data addresses and offsets from the machine code."""

from __future__ import annotations

import struct

import pytest
from conftest import FIXTURE

from cgraph import config
from cgraph.image import FunctionFacts, Image, resolve_sized


@pytest.fixture(scope="module")
def image() -> Image:
    loaded = config.load(FIXTURE / "cgraph.toml")
    return Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image)


def globals_of(image: Image, name: str) -> set[str]:
    (symbol,) = image.functions[name]
    return {item["global"] for item in image.facts(symbol).globals}


def test_address_is_resolved_only_inside_a_sized_object() -> None:
    symbols = [(100, 8, "state", ".data")]
    assert resolve_sized(symbols, 107) == ("state", 100, ".data")
    assert resolve_sized(symbols, 108) is None


def test_a_zero_size_marker_does_not_hide_the_object_it_sits_in() -> None:
    symbols = [(100, 16, "table", ".data"), (104, 0, "marker", ".data")]
    assert resolve_sized(symbols, 110) == ("table", 100, ".data")


def test_shared_auipc_anchor_resolves_every_load(image: Image) -> None:
    assert globals_of(image, "anchor_pair") == {"pair_a", "pair_b"}


def test_merged_globals_behind_one_anchor(image: Image) -> None:
    assert {"box_count", "next_id.counter", "table_lock"} <= globals_of(image, "box_new")


def test_thread_pointer_loads_are_per_cpu_state(image: Image) -> None:
    (symbol,) = image.functions["bag_count"]
    entries = [item for item in image.facts(symbol).globals if item["global"] == "current_box"]
    assert entries and all(item["target_class"] == "per_cpu" for item in entries)


def test_direct_and_tail_calls(image: Image) -> None:
    (symbol,) = image.functions["box_check"]
    assert [(call["target"], call["tail"]) for call in image.facts(symbol).calls] == [("ext_check", True)]
    (symbol,) = image.functions["bag_count"]
    assert {call["target"] for call in image.facts(symbol).calls} == {"walk", "run_get"}


def test_function_address_passed_as_an_argument(image: Image) -> None:
    (symbol,) = image.functions["bag_count"]
    assert [item["target"] for item in image.facts(symbol).function_addresses] == ["visit"]


def test_addresses_outside_allocated_sections_are_not_reported(image: Image) -> None:
    for name in image.functions:
        for symbol in image.functions[name]:
            for item in image.facts(symbol).outside:
                assert image.section_of(int(item["address"], 16)) is not None


def test_local_symbols_know_their_file(image: Image) -> None:
    assert sorted(symbol.file for symbol in image.functions["helper_dup"]) == ["bags.c", "store.c"]
    assert [symbol.local for symbol in image.functions["box_new"]] == [False]


@pytest.mark.parametrize(("caller_name", "callee_name"), [("store_dup", "bags_dup"), ("bags_dup", "box_new")])
def test_auipc_jalr_is_a_call_forward_and_backward(image: Image, caller_name: str, callee_name: str) -> None:
    """``auipc ra, hi; jalr ra, lo(ra)``: the call a large image uses beyond the reach of ``jal``.

    The upper immediate is signed, so a callee before the caller needs a negative one."""
    (caller,) = image.functions[caller_name]
    (callee,) = image.functions[callee_name]
    offset = callee.address - caller.address
    high = (offset + 0x800) >> 12
    low = offset - (high << 12)
    auipc = ((high & 0xFFFFF) << 12) | (1 << 7) | 0x17
    jalr = ((low & 0xFFF) << 20) | (1 << 15) | (1 << 7) | 0x67
    code = struct.pack("<II", auipc, jalr)
    values: dict[int, int] = {}
    found = FunctionFacts()
    for instruction in image.decoder.disasm(code, caller.address):
        image._step("synthetic", instruction, values, found)
    assert [(call["target"], call["tail"]) for call in found.calls] == [(callee_name, False)]


def test_backward_auipc_data_address(image: Image) -> None:
    """bags_dup takes the address of ext_check, which lies before it: a negative auipc immediate."""
    (symbol,) = image.functions["bags_dup"]
    assert [item["target"] for item in image.facts(symbol).function_addresses] == ["ext_check"]
