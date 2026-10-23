"""The image layer: calls, data addresses and offsets from the machine code."""

from __future__ import annotations

import struct

import pytest
from conftest import FIXTURE

from cgraph import config
from cgraph.image import FunctionFacts, Image, resolve_sized, source_name


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
    high, low = _split(callee.address - caller.address)
    code = struct.pack("<II", _u(0x17, 1, high), _i(0x67, 1, 0, 1, low))
    found = image.decode("synthetic", code, caller.address)
    assert [(call["target"], call["tail"]) for call in found.calls] == [(callee_name, False)]


def _split(offset: int) -> tuple[int, int]:
    high = (offset + 0x800) >> 12
    return high, offset - (high << 12)


def _u(opcode: int, rd: int, upper: int) -> int:
    return ((upper & 0xFFFFF) << 12) | (rd << 7) | opcode


def _i(opcode: int, rd: int, funct3: int, rs1: int, immediate: int) -> int:
    return ((immediate & 0xFFF) << 20) | (rs1 << 15) | (funct3 << 12) | (rd << 7) | opcode


def _branch(rs1: int, rs2: int, offset: int) -> int:
    """beq rs1, rs2, offset."""
    bits = offset & 0x1FFF
    return (((bits >> 12) & 1) << 31 | ((bits >> 5) & 0x3F) << 25 | rs2 << 20 | rs1 << 15
            | ((bits >> 1) & 0xF) << 8 | ((bits >> 11) & 1) << 7 | 0x63)  # fmt: skip


def _jump(offset: int) -> int:
    """jal zero, offset."""
    bits = offset & 0x1FFFFF
    return (((bits >> 20) & 1) << 31 | ((bits >> 1) & 0x3FF) << 21 | ((bits >> 11) & 1) << 20
            | ((bits >> 12) & 0xFF) << 12 | 0x6F)  # fmt: skip


A0, A1, A5, T0, ZERO = 10, 11, 15, 5, 0


def _load(rd: int, base: int, offset: int) -> int:
    return _i(0x03, rd, 3, base, offset)


def _addi(rd: int, rs1: int, immediate: int) -> int:
    return _i(0x13, rd, 0, rs1, immediate)


def test_lui_add_displacement_beyond_twelve_bits(image: Image) -> None:
    """``lui t0, 1; add t0, a0, t0; ld a1, 16(t0)``: a field at 4112 bytes from a0."""
    add = (T0 << 20) | (A0 << 15) | (T0 << 7) | 0x33
    code = struct.pack("<III", _u(0x37, T0, 1), add, _load(A1, T0, 16))
    assert 4112 in image.decode("synthetic", code, 0x11000).memory_offsets


def test_an_address_formed_before_a_jump_is_followed_at_the_join(image: Image) -> None:
    """The address of pair_a reaches the load at the join from both paths, one through a ``j``."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack(
        "<7I", _u(0x17, A5, high), _addi(A5, A5, low), _branch(A0, ZERO, 12), _addi(A0, A0, 1), _jump(8),
        _addi(A0, A0, 2), _load(A1, A5, 8),
    )  # fmt: skip
    loads = {item["global"] for item in image.decode("synthetic", code, start).globals
             if item["operation"] == "load_or_store"}  # fmt: skip
    assert loads == {"pair_b"}


def test_an_address_formed_on_one_path_only_is_not_followed(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - (start + 4))
    code = struct.pack("<4I", _branch(A0, ZERO, 12), _u(0x17, A5, high), _addi(A5, A5, low), _load(A1, A5, 8))
    loads = [item for item in image.decode("synthetic", code, start).globals if item["operation"] == "load_or_store"]
    assert loads == []


def test_backward_auipc_data_address(image: Image) -> None:
    """bags_dup takes the address of ext_check, which lies before it: a negative auipc immediate."""
    (symbol,) = image.functions["bags_dup"]
    assert [item["target"] for item in image.facts(symbol).function_addresses] == ["ext_check"]


def test_compiler_suffix_gives_the_c_name() -> None:
    assert source_name("create_worker.__already_done.78") == "create_worker.__already_done"
    assert source_name("files_stat.0") == "files_stat"
    assert source_name("box_count") == "box_count"


TP = 4


def _amo(funct5: int, rd: int, rs2: int, rs1: int) -> int:
    return (funct5 << 27) | (rs2 << 20) | (rs1 << 15) | (3 << 12) | (rd << 7) | 0x2F


def _add(rd: int, rs1: int, rs2: int) -> int:
    return (rs2 << 20) | (rs1 << 15) | (rd << 7) | 0x33


def _operations(facts: FunctionFacts) -> set[tuple[str, str]]:
    return {(item["global"], item["operation"]) for item in facts.globals}


def test_the_thread_pointer_as_a_value_and_as_an_atomic_address(image: Image) -> None:
    code = struct.pack("<II", _addi(A0, TP, 0x368), _amo(0b01100, ZERO, A0, TP))
    assert _operations(image.decode("synthetic", code, 0x11000)) == {
        ("current_box", "thread_pointer_value"),
        ("current_box", "thread_pointer_access"),
    }


def test_a_known_address_plus_an_unknown_index(image: Image) -> None:
    """``&pair_a + a0`` then ``ld 8(...)``: pair_b, through an index the code computed."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _add(A5, A5, A0), _load(A1, A5, 8))
    assert ("pair_b", "indexed_access") in _operations(image.decode("synthetic", code, start))


def test_an_address_only_used_as_a_base_is_not_itself_an_access(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    based = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _load(A1, A5, 8), 0x00008067)
    assert _operations(image.decode("synthetic", based, start)) == {("pair_b", "load_or_store")}
    returned = struct.pack("<3I", _u(0x17, A0, high), _addi(A0, A0, low), 0x00008067)  # ret
    assert ("pair_a", "addi_address") in _operations(image.decode("synthetic", returned, start))


def test_a_zero_size_name_at_an_objects_address_is_an_alias(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _load(A1, A5, 0), 0x00008067)
    (entry,) = [item for item in image.decode("synthetic", code, start).globals if item["global"] == "pair_a"]
    assert entry["aliases"] == ["pair_alias"]


def test_a_displacement_split_into_addi_2047_and_a_load(image: Image) -> None:
    """``addi s3, a0, 2047; ld a1, 481(s3)``: the field at 2528 bytes from a0."""
    s3 = 19
    code = struct.pack("<3I", _addi(s3, A0, 2047), _load(A1, s3, 481), 0x00008067)
    offsets = image.decode("synthetic", code, 0x11000).memory_offsets
    assert 2528 in offsets and 481 in offsets
