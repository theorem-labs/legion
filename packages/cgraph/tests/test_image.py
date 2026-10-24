"""The image layer: calls, data addresses and offsets from the machine code."""

from __future__ import annotations

import dataclasses
import struct
from pathlib import Path

import pytest
from conftest import FIXTURE

from cgraph import config, imagecache
from cgraph import image as image_module
from cgraph.errors import InputError
from cgraph.image import FunctionFacts, Image, code_symbol, read_system_map, resolve_sized, source_name
from cgraph.imagecache import ImageCache


@pytest.fixture(scope="module")
def image() -> Image:
    loaded = config.load(FIXTURE / "cgraph.toml")
    return Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image)


def test_malformed_system_map_names_its_line(tmp_path: Path) -> None:
    path = tmp_path / "System.map"
    path.write_text("0000000000010000 T valid\nmalformed\n")
    with pytest.raises(InputError, match=r"System\.map .* line 2"):
        read_system_map(path)


def test_malformed_elf_is_an_input_error(tmp_path: Path) -> None:
    image_path = tmp_path / "not-an-elf"
    image_path.write_bytes(b"not an ELF")
    system_map = tmp_path / "System.map"
    system_map.write_text("0000000000010000 T function\n")
    with pytest.raises(InputError, match=r"cannot parse ELF"):
        Image(image_path, system_map, config.ImageRules())


class _RefusingDisassembler:
    """A decoder whose use fails the test: a warm image cache must not disassemble anything."""

    detail = True

    def disasm(self, *_arguments: object) -> list[object]:
        raise AssertionError("warm image facts must be read from the cache")


def _all_facts(image: Image) -> dict[tuple[str, int], FunctionFacts]:
    return {
        (symbol.name, symbol.address): image.facts(symbol) for copies in image.functions.values() for symbol in copies
    }


def _cache_bytes(cache: Path) -> dict[str, bytes]:
    images = cache / "image"
    return {str(path.relative_to(images)): path.read_bytes() for path in sorted(images.rglob("*.json"))}


def test_image_cache_reuses_every_function_without_decoding(tmp_path: Path) -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    cache = tmp_path / "cache"
    cold = Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image, cache)
    try:
        expected = _all_facts(cold)
    finally:
        cold.close()
    cold_bytes = _cache_bytes(cache)
    assert any(name.startswith("callees/") for name in cold_bytes)
    assert any("/" not in name for name in cold_bytes)

    warm = Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image, cache)
    try:
        warm.decoder = _RefusingDisassembler()
        assert _all_facts(warm) == expected
    finally:
        warm.close()
    assert _cache_bytes(cache) == cold_bytes


def _decodes(
    cache: Path, name: str, arity: dict[str, int], results: dict[str, int], monkeypatch: pytest.MonkeyPatch
) -> int:
    """How many times ``Image.facts`` disassembles ``name`` itself with these C signatures (deciding
    whether a callee returns disassembles the callee, which is not a decode of ``name``)."""
    loaded = config.load(FIXTURE / "cgraph.toml")
    image = Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image, cache)
    (symbol,) = image.functions[name]
    count = 0
    disassemble = image.decoder.disasm

    def counting(code: bytes, address: int, *rest: object) -> object:
        nonlocal count
        count += address == symbol.address
        return disassemble(code, address, *rest)

    monkeypatch.setattr(image.decoder, "disasm", counting)
    image.arity, image.results = arity, results
    try:
        image.facts(symbol)
    finally:
        image.close()
    return count


def test_image_cache_keys_a_function_on_its_own_results_and_its_direct_callees_arity(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """bag_count calls walk and run_get directly: their arity decides which argument registers a call
    passes, so it decides bag_count's facts. obj_free's arity and box_new's results do not."""
    cache = tmp_path / "cache"
    arity = {"walk": 2, "run_get": 1, "obj_free": 1}
    results = {"bag_count": 1, "box_new": 1}
    assert _decodes(cache, "bag_count", arity, results, monkeypatch) == 1
    assert _decodes(cache, "bag_count", arity, results, monkeypatch) == 0
    assert _decodes(cache, "bag_count", {**arity, "obj_free": 3}, {**results, "box_new": 2}, monkeypatch) == 0
    assert _decodes(cache, "bag_count", {**arity, "walk": 3}, results, monkeypatch) == 1
    assert _decodes(cache, "bag_count", arity, {**results, "bag_count": 0}, monkeypatch) == 1
    # A function C does not describe takes the defaults: eight arguments and two result registers.
    assert _decodes(cache, "bag_count", {**arity, "walk": 8}, {**results, "bag_count": 2}, monkeypatch) == 1
    assert _decodes(cache, "bag_count", {"run_get": 1}, {}, monkeypatch) == 0


def test_image_cache_key_covers_each_decoder_input(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    image_path = FIXTURE / "image" / "mini.elf"
    system_map = FIXTURE / "image" / "System.map"
    source = tmp_path / "image.py"
    source.write_text("image decoder")

    def key(
        image: Path = image_path,
        map_path: Path = system_map,
        rules: config.ImageRules = loaded.image,
        decoder_source: Path = source,
        name: str = "function",
        address: int = 1,
        size: int = 2,
        results: int = 1,
        arities: dict[str, int] | None = None,
    ) -> str:
        cache = ImageCache(tmp_path / "cache", image, map_path, rules, decoder_source)
        return cache.facts_key(cache.symbol_key(name, address, size), results, arities or {"callee": 1})

    changed_image = tmp_path / "image"
    changed_image.write_bytes(image_path.read_bytes() + b"changed")
    changed_map = tmp_path / "System.map"
    changed_map.write_text(system_map.read_text() + "0000000000020000 T changed\n")
    changed_source = tmp_path / "changed-image.py"
    changed_source.write_text("changed decoder")
    keys = {
        key(),
        key(image=changed_image),
        key(map_path=changed_map),
        key(rules=config.ImageRules(thread_pointer="other")),
        key(decoder_source=changed_source),
        key(name="other"),
        key(address=2),
        key(size=3),
        key(results=2),
        key(arities={"callee": 2}),
        key(arities={"other": 1}),
        key(rules=dataclasses.replace(loaded.image, trap_table=_bug_table())),
    }
    monkeypatch.setattr(imagecache.capstone, "__version__", "changed")
    keys.add(key())
    monkeypatch.undo()
    monkeypatch.setattr(imagecache.elftools, "__version__", "changed")
    keys.add(key())
    assert len(keys) == 14


def test_disabled_image_cache_does_not_read_inputs(tmp_path: Path) -> None:
    cache = ImageCache(
        None, tmp_path / "missing-image", tmp_path / "missing-map", config.ImageRules(), tmp_path / "missing-source"
    )
    assert not cache.enabled
    assert cache.callees("function") is None


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
    """A load at pair_a reads pair_a's bytes, which pair_alias names too. It reads nothing of
    flow_header_ops, which ends there: no load reads the end of an object."""
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


RA, SP, S1, A2, A3 = 1, 2, 9, 12, 13
RET = 0x00008067


def _jal(rd: int, offset: int) -> int:
    bits = offset & 0x1FFFFF
    return (((bits >> 20) & 1) << 31 | ((bits >> 1) & 0x3FF) << 21 | ((bits >> 11) & 1) << 20
            | ((bits >> 12) & 0xFF) << 12 | rd << 7 | 0x6F)  # fmt: skip


def test_call_to_non_text_symbol_is_recorded_as_an_exclusion(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    target = image.object_symbols["pair_a"][0].address
    facts = image.decode("synthetic", struct.pack("<I", _jal(ZERO, target - start)), start)
    assert facts.exclusions == [
        {
            "kind": "image_call",
            "reason": "target_not_text",
            "function": "synthetic",
            "target": f"0x{target:x}",
            "detail": f"pair_a, pair_alias at 0x{target:x}",
            "provenance": [{"source": "image", "address": f"0x{start:x}"}],
        }
    ]


def _store(rs2: int, base: int, offset: int) -> int:
    return ((offset >> 5) & 0x7F) << 25 | rs2 << 20 | base << 15 | 3 << 12 | (offset & 0x1F) << 7 | 0x23


def test_a_lui_number_is_not_an_address(image: Image) -> None:
    """``lui a0, hi; addi a0, a0, lo`` equal to pair_a's address is a number (an ioctl command)."""
    address = image.object_symbols["pair_a"][0].address
    high, low = (address + 0x800) >> 12, address - (((address + 0x800) >> 12) << 12)
    code = struct.pack("<4I", _u(0x37, A0, high), _addi(A0, A0, low), _load(A1, A0, 0), RET)
    assert image.decode("synthetic", code, 0x11000).globals == []


def test_a_callee_saved_base_keeps_its_address_across_a_call(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<5I", _u(0x17, S1, high), _addi(S1, S1, low), _jal(RA, 0x100), _load(A1, S1, 8), RET)
    assert _operations(image.decode("synthetic", code, start)) == {("pair_b", "load_or_store")}


def test_a_compressed_add_derives_the_address_and_a_store_lets_it_escape(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    compressed_add = (0b1001 << 12) | (A2 << 7) | (A3 << 2) | 0b10
    code = struct.pack("<IIHII", _u(0x17, A3, high), _addi(A3, A3, low), compressed_add, _store(A2, SP, 0), RET)
    assert ("pair_a", "addi_address") in _operations(image.decode("synthetic", code, start))


def test_code_after_ebreak_keeps_its_registers(image: Image) -> None:
    """A WARN's ebreak resumes: the load after it still has the address formed before."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<5I", _u(0x17, S1, high), _addi(S1, S1, low), 0x00100073, _load(A1, S1, 8), RET)
    assert _operations(image.decode("synthetic", code, start)) == {("pair_b", "load_or_store")}


def test_an_address_in_an_argument_the_callee_does_not_take_does_not_escape(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    callee = image.functions["box_weight"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    jal = _jal(RA, callee - (start + 8))
    code = struct.pack("<5I", _u(0x17, A1, high), _addi(A1, A1, low), jal, _load(A2, S1, 0), RET)
    image.arity = {"box_weight": 0}
    try:
        assert ("pair_a", "addi_address") not in _operations(image.decode("synthetic", code, start))
        image.arity = {"box_weight": 2}
        assert ("pair_a", "addi_address") in _operations(image.decode("synthetic", code, start))
    finally:
        image.arity = {}


def test_a_void_function_returns_no_register(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<3I", _u(0x17, A0, high), _addi(A0, A0, low), RET)
    image.results = {"synthetic": 0}
    try:
        assert image.decode("synthetic", code, start).globals == []
    finally:
        image.results = {}
    assert ("pair_a", "addi_address") in _operations(image.decode("synthetic", code, start))


def test_an_address_plus_a_field_offset_that_escapes_names_the_object_there(image: Image) -> None:
    """``a5 = &pair_a; a0 = a5 + 8; ret a0``: the returned address is pair_b, not pair_a."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _addi(A0, A5, 8), RET)
    assert _operations(image.decode("synthetic", code, start)) == {("pair_b", "addi_address")}


S0, S3 = 8, 19


def _address_in(image: Image, start: int, register: int, address: int) -> tuple[int, int]:
    """``auipc``/``addi`` of ``register`` at ``start``, so that it holds ``address``."""
    return _split(address - start)


def _bltu(rs1: int, rs2: int, offset: int) -> int:
    """bltu rs1, rs2, offset."""
    return _branch(rs1, rs2, offset) | 6 << 12


def _compared(image: Image, address: int) -> FunctionFacts:
    """``a5 = address``, then ``bltu a1, a5``: the address is a loop's bound, and nothing else."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(address - start)
    code = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _bltu(A1, A5, 8), RET)
    return image.decode("synthetic", code, start)


def test_an_address_one_past_an_object_also_names_that_object(image: Image) -> None:
    """``&pair_a + 8`` compared as a loop's bound is pair_b's address: the two cannot be told apart,
    so both name it."""
    pair_a = image.object_symbols["pair_a"][0]
    (entry,) = _compared(image, pair_a.address + pair_a.size).globals
    assert (entry["global"], entry["offset"], entry["aliases"]) == ("pair_b", 0, ["pair_a"])


def test_an_address_one_past_the_last_object_names_that_object(image: Image) -> None:
    """``&heap[sizeof heap]``, the end pointer of a loop: no object starts there, and the address
    lies outside every section, but it still names heap."""
    heap = image.object_symbols["heap"][0]
    assert image.section_of(heap.address + heap.size) is None
    (entry,) = _compared(image, heap.address + heap.size).globals
    assert (entry["global"], entry["offset"]) == ("heap", heap.size)


def test_an_object_the_image_hides_keeps_the_end_address_for_itself(image: Image) -> None:
    """A jump table or a constant pool (.LJTI..., .Lswitch.table...) often starts where an object
    ends, and System.map leaves it out. The address is the table's, so the object does not take it."""
    heap = image.object_symbols["heap"][0]
    image.hidden_starts.add(heap.address + heap.size)
    try:
        assert _compared(image, heap.address + heap.size).globals == []
    finally:
        image.hidden_starts.discard(heap.address + heap.size)


def test_a_zero_size_symbol_outside_every_section_is_recorded(image: Image) -> None:
    """_etext, _edata and _end sit at a section's end, which belongs to no section. The C compares
    an address with them, so the image must name them."""
    start = image.functions["anchor_pair"][0].address
    marker = image.object_symbols["heap"][0].address + image.object_symbols["heap"][0].size + 0x100
    assert image.section_of(marker) is None
    high, low = _split(marker - start)
    code = struct.pack("<3I", _u(0x17, A0, high), _addi(A0, A0, low), RET)
    image.zero_size_at[marker] = ["_etext"]
    try:
        (entry,) = image.decode("synthetic", code, start).globals
    finally:
        del image.zero_size_at[marker]
    assert (entry["global"], entry["offset"], entry["section"]) == ("_etext", 0, "")


def test_the_base_of_a_merged_globals_block_is_not_an_access_to_its_first_member(image: Image) -> None:
    """LLVM merges several globals into one .L_MergedGlobals block and addresses the block once.
    That address is the first member's, but the instruction accesses no member: each member is
    reached by a displacement from it, which is recorded where it happens."""
    start = image.functions["anchor_pair"][0].address
    block = image.object_symbols["box_count"][0].address
    assert image.merged_blocks.get(block)
    high, low = _split(block - start)
    code = struct.pack("<3I", _u(0x17, A0, high), _addi(A0, A0, low), RET)
    facts = image.decode("synthetic", code, start)
    assert facts.globals == []
    assert [(item["kind"], item["reason"], int(item["target"], 16)) for item in facts.exclusions] == [
        ("image_access", "merged_globals_block_base", block)
    ]
    # The names the block's start has, which a C access to the first member uses.
    assert facts.exclusions[0]["detail"] == {"block_bytes": image.merged_blocks[block], "names": ["box_count"]}


def test_a_displacement_from_a_merged_globals_base_still_names_its_member(image: Image) -> None:
    start = image.functions["anchor_pair"][0].address
    block = image.object_symbols["box_count"][0].address
    table = image.object_symbols["table_lock"][0]
    high, low = _split(block - start)
    code = struct.pack("<4I", _u(0x17, A0, high), _addi(A0, A0, low), _load(A1, A0, table.address - block), RET)
    assert ("table_lock", "load_or_store") in _operations(image.decode("synthetic", code, start))


def test_a_code_marker_at_a_function_start_is_the_function_not_a_global(image: Image) -> None:
    """__entry_text_end sits at handle_exception's first instruction. Where the C takes the
    function's address, naming the marker as well invents an access to it."""
    start = image.functions["anchor_pair"][0].address
    callee = image.functions["box_weight"][0].address
    high, low = _split(callee - start)
    code = struct.pack("<3I", _u(0x17, A0, high), _addi(A0, A0, low), RET)
    image.zero_size_at[callee] = ["__entry_text_end"]
    try:
        facts = image.decode("synthetic", code, start)
    finally:
        del image.zero_size_at[callee]
    assert [item["target"] for item in facts.function_addresses] == ["box_weight"]
    assert facts.globals == []


def test_an_address_spilled_to_the_frame_and_reloaded_keeps_its_object(image: Image) -> None:
    """``sd s1, -24(s0)`` then ``ld s1, -24(s0)`` on both paths into a block: every path gives the
    register the same address, so the load after the join addresses pair_b."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack(
        "<8I", _u(0x17, S1, high), _addi(S1, S1, low), _store(S1, S0, -24), _branch(A0, ZERO, 12),
        _addi(S1, ZERO, 0), _load(S1, S0, -24), _load(A1, S1, 8), RET,
    )  # fmt: skip
    assert ("pair_b", "load_or_store") in _operations(image.decode("synthetic", code, start))


def test_an_index_added_to_a_known_address_records_the_object(image: Image) -> None:
    """``a5 = &pair_a; a0 = a5 + a0``, returned: the sum is an address inside pair_a, whatever the
    index is, and no load of this function shows it."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack("<4I", _u(0x17, A5, high), _addi(A5, A5, low), _add(A0, A5, A0), RET)
    assert ("pair_a", "indexed_address") in _operations(image.decode("synthetic", code, start))


def test_a_field_offset_from_an_indexed_base_is_a_memory_offset(image: Image) -> None:
    """``a5 = &box_table + index; a5 += 2047; ld a1, 481(a5)``: the field at 2528 bytes from the
    object, split over the register and the load."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["box_table"][0].address - start)
    code = struct.pack(
        "<6I", _u(0x17, A5, high), _addi(A5, A5, low), _add(A5, A5, A0), _addi(A5, A5, 2047),
        _load(A1, A5, 481), RET,
    )  # fmt: skip
    assert 2528 in image.decode("synthetic", code, start).memory_offsets


def test_a_small_offset_split_over_a_register_and_a_load_is_a_memory_offset(image: Image) -> None:
    """``addi a5, a0, 100; ld a1, 8(a5)``: the field at 108 bytes from whatever a0 points at."""
    start = image.functions["anchor_pair"][0].address
    code = struct.pack("<3I", _addi(A5, A0, 100), _load(A1, A5, 8), RET)
    assert 108 in image.decode("synthetic", code, start).memory_offsets


def test_an_assembly_routine_with_a_size_is_code() -> None:
    """The kernel's handle_exception and ret_from_fork_kernel_asm are assembly: the symbol table
    gives them a size but no function type, and a zero-size marker sits at the first instruction of
    each. Without this they are neither a function nor an object, and the marker takes the address."""
    assert code_symbol("STT_FUNC", 26, executable=True)
    assert code_symbol("STT_NOTYPE", 26, executable=True)
    assert not code_symbol("STT_NOTYPE", 0, executable=True)
    assert not code_symbol("STT_NOTYPE", 26, executable=False)
    assert not code_symbol("STT_OBJECT", 26, executable=False)


def test_an_offset_the_code_forms_is_recorded_where_it_is_formed(image: Image) -> None:
    """``lui a3, 1; addi a3, a3, 0x48; add a0, a0, a3`` is the address of the field at 4168 bytes.
    An index added after it loses the offset, so recording it only at the load would lose it."""
    start = image.functions["anchor_pair"][0].address
    code = struct.pack(
        "<6I", _u(0x37, A3, 1), _addi(A3, A3, 0x48), _add(A0, A0, A3), _add(A0, A0, T0),
        _load(A1, A0, 0), RET,
    )  # fmt: skip
    assert 4168 in image.decode("synthetic", code, start).memory_offsets


def test_a_constant_a_shift_forms_is_an_offset(image: Image) -> None:
    """``c.li a2, 3; c.slli a2, 11; add a0, a0, a2``: the element at 6144 bytes. The compiler forms
    the size of an array element this way, and the field's offset is lost without the shift."""
    start = image.functions["anchor_pair"][0].address
    code = struct.pack("<HHIII", 0x460D, 0x062E, _add(A0, A0, A2), _load(A1, A0, 0), RET)
    assert 6144 in image.decode("synthetic", code, start).memory_offsets


def _two_paths(image: Image, second: bytes) -> FunctionFacts:
    """``s1 = &pair_a``; on the fall-through path ``second`` sets s1 again; both paths then meet at
    ``ld a1, 0(s1)``."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = (
        struct.pack("<3I", _u(0x17, S1, high), _addi(S1, S1, low), _branch(A0, ZERO, 4 + len(second)))
        + second
        + struct.pack("<2I", _load(A1, S1, 0), RET)
    )
    return image.decode("synthetic", code, start)


def test_an_address_each_path_forms_differently_names_every_object(image: Image) -> None:
    """pair_a on one path and pair_b on the other: the load reads one of them on each path, so the
    function accesses both, each on some path."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_b"][0].address - (start + 12))
    facts = _two_paths(image, struct.pack("<2I", _u(0x17, S1, high), _addi(S1, S1, low)))
    assert {("pair_a", "load_or_store_some_path"), ("pair_b", "load_or_store_some_path")} <= _operations(facts)


def test_an_address_some_path_leaves_unknown_names_nothing(image: Image) -> None:
    """pair_a on one path, a pointer loaded from memory on the other: the load's object is unknown
    on that path, so no object is claimed."""
    facts = _two_paths(image, struct.pack("<I", _load(S1, A0, 0)))
    assert not {operation for _, operation in _operations(facts)} & {"load_or_store", "load_or_store_some_path"}


def _after_a_call(image: Image, callee: str, far: bool) -> FunctionFacts:
    """``s1 = &pair_a``; on one path a call to ``callee``, then (in layout, after the call) code that
    sets ``s1 = &pair_b``; both paths meet at ``ld a1, 0(s1)``. ``far`` calls with auipc+jalr."""
    start = image.functions["anchor_pair"][0].address
    target = image.functions[callee][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    upper, lower = _split(target - (start + 12))
    call = (
        struct.pack("<2I", _u(0x17, RA, upper), _i(0x67, RA, 0, RA, lower))
        if far
        else struct.pack("<I", _jal(RA, target - (start + 12)))
    )
    after = 12 + len(call)
    high_b, low_b = _split(image.object_symbols["pair_b"][0].address - (start + after))
    code = (
        struct.pack("<3I", _u(0x17, S1, high), _addi(S1, S1, low), _branch(A0, ZERO, len(call) + 12))
        + call
        + struct.pack("<4I", _u(0x17, S1, high_b), _addi(S1, S1, low_b), _load(A1, S1, 0), RET)
    )
    return image.decode("synthetic", code, start)


@pytest.mark.parametrize("far", [False, True])
def test_code_after_a_call_that_never_returns_is_not_reached_from_it(image: Image, far: bool) -> None:
    """_start loops forever and has no return: nothing after a call to it runs, so the load reads
    pair_a on the only path that reaches it."""
    facts = _after_a_call(image, "_start", far)
    assert ("pair_a", "load_or_store") in _operations(facts)
    assert not {name for name, _ in _operations(facts)} & {"pair_b"}


def test_code_after_a_call_that_returns_is_reached_from_it(image: Image) -> None:
    """box_weight returns, so both paths reach the load: it reads pair_a or pair_b."""
    facts = _after_a_call(image, "box_weight", far=False)
    assert {("pair_a", "load_or_store_some_path"), ("pair_b", "load_or_store_some_path")} <= _operations(facts)


def test_a_constant_each_path_gives_selects_one_object_per_path(image: Image) -> None:
    """``c.li a2, 0`` on one path and ``c.li a2, 8`` on the other, then ``add a5, s1, a2`` with
    s1 = &pair_a: the load reads pair_a or pair_b, as ``in_slab = cond ? &a : &b`` compiles."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack(
        "<IIHIHIII", _u(0x17, S1, high), _addi(S1, S1, low), 0x4601, _branch(A0, ZERO, 6), 0x4621,
        _add(A5, S1, A2), _load(A1, A5, 0), RET,
    )  # fmt: skip
    facts = image.decode("synthetic", code, start)
    assert {("pair_a", "load_or_store_some_path"), ("pair_b", "load_or_store_some_path")} <= _operations(facts)


EBREAK = 0x00100073


def _bug_table() -> config.TrapTable:
    """The kernel's __bug_table with relative pointers: the trap as an offset from the entry, then a
    u16 of flags, whose bit 0 (BUGFLAG_WARNING) means the trap resumes."""
    return config.TrapTable(
        section="__bug_table",
        entry_size=8,
        address_offset=0,
        address_size=4,
        relative=True,
        flags_offset=4,
        flags_size=2,
        resume_flags=1,
    )


def _bug_entries(section: int, entries: list[tuple[int, int]]) -> bytes:
    return b"".join(
        struct.pack("<iHxx", trap - (section + 8 * index), flags) for index, (trap, flags) in enumerate(entries)
    )


def test_a_trap_table_tells_each_trap_whether_it_resumes() -> None:
    section = 0xFFFFFFFF80900000
    bug, warn = 0xFFFFFFFF80212946, 0xFFFFFFFF80900010  # one before the table, one after it
    data = _bug_entries(section, [(bug, 0), (warn, 0x909)])
    assert image_module.read_trap_table(data, section, _bug_table()) == {bug: False, warn: True}


def test_an_absolute_trap_table_is_read() -> None:
    table = dataclasses.replace(_bug_table(), entry_size=16, address_size=8, relative=False, flags_offset=8)
    data = struct.pack("<QH6x", 0x1000, 0) + struct.pack("<QH6x", 0x2000, 1)
    assert image_module.read_trap_table(data, 0x5000, table) == {0x1000: False, 0x2000: True}


def test_a_trap_table_of_partial_entries_is_an_input_error() -> None:
    with pytest.raises(InputError, match=r"__bug_table holds 12 bytes, not a whole number of 8-byte entries"):
        image_module.read_trap_table(bytes(12), 0x5000, _bug_table())


def test_a_trap_table_the_image_lacks_is_an_input_error() -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    rules = dataclasses.replace(loaded.image, trap_table=_bug_table())
    with pytest.raises(InputError, match=r"has no section __bug_table, which \[image\] trap_table names"):
        Image(loaded.inputs.image, loaded.inputs.system_map, rules)


def _after_a_trap(image: Image, flags: int) -> FunctionFacts:
    """``s1 = &pair_a``; on the fall-through path an ``ebreak`` the trap table lists with ``flags``,
    then ``s1 = &pair_b``; both paths meet at ``ld a1, 0(s1)``."""
    start = image.functions["anchor_pair"][0].address
    section = start + 0x1000
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    high_b, low_b = _split(image.object_symbols["pair_b"][0].address - (start + 16))
    code = struct.pack(
        "<8I", _u(0x17, S1, high), _addi(S1, S1, low), _branch(A0, ZERO, 16), EBREAK,
        _u(0x17, S1, high_b), _addi(S1, S1, low_b), _load(A1, S1, 0), RET,
    )  # fmt: skip
    image.traps = image_module.read_trap_table(_bug_entries(section, [(start + 12, flags)]), section, _bug_table())
    try:
        return image.decode("synthetic", code, start)
    finally:
        image.traps = {}


def test_code_after_a_trap_that_does_not_resume_is_not_reached_from_it(image: Image) -> None:
    """A BUG: its entry lacks the resume flag, so the load reads pair_a on the only path to it."""
    facts = _after_a_trap(image, flags=0)
    assert ("pair_a", "load_or_store") in _operations(facts)
    assert "pair_b" not in {name for name, _ in _operations(facts)}


def test_code_after_a_trap_that_resumes_is_reached_from_it(image: Image) -> None:
    """A WARN (flags 0x909, the resume bit set) continues, so the load reads pair_a or pair_b."""
    facts = _after_a_trap(image, flags=0x909)
    assert {("pair_a", "load_or_store_some_path"), ("pair_b", "load_or_store_some_path")} <= _operations(facts)


def _at(image: Image, register: int, address: int, *tail: int) -> FunctionFacts:
    """``register = address`` (auipc, addi), then the ``tail`` instructions and ``ret``."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(address - start)
    code = struct.pack(f"<{3 + len(tail)}I", _u(0x17, register, high), _addi(register, register, low), *tail, RET)
    return image.decode("synthetic", code, start)


def test_an_index_added_to_a_table_and_only_loaded_through_is_no_access_of_its_own(image: Image) -> None:
    """``a5 = &pair_a + a0; ld a1, 0(a5)``, a lookup or jump table: the load is the access, at its
    own instruction. The add only forms the address the load reads."""
    facts = _at(image, A5, image.object_symbols["pair_a"][0].address, _add(A5, A5, A0), _load(A1, A5, 0))
    assert _operations(facts) == {("pair_a", "indexed_access")}


def test_no_load_reads_the_end_of_an_object(image: Image) -> None:
    """A load at the address one past heap reads whatever lies there (a constant pool, a string),
    never heap: in C, the end of an object is compared, not dereferenced."""
    heap = image.object_symbols["heap"][0]
    assert _at(image, A5, heap.address + heap.size, _load(A1, A5, 0)).globals == []


def test_an_address_at_an_objects_end_passed_to_a_call_is_not_its_end_pointer(image: Image) -> None:
    """A string literal right after an object has the object's end address. Passed to a call, it
    is not a loop bound, so it does not name the object."""
    start = image.functions["anchor_pair"][0].address
    heap = image.object_symbols["heap"][0]
    call = _jal(RA, image.functions["box_weight"][0].address - (start + 8))
    assert _at(image, A0, heap.address + heap.size, call).globals == []


def test_a_bound_at_an_objects_start_also_names_the_object_ending_there(image: Image) -> None:
    """pair_a starts where flow_header_ops ends: compared as a bound, the address may be the end of
    flow_header_ops, so it names both."""
    (entry,) = [item for item in _compared(image, image.object_symbols["pair_a"][0].address).globals
                if item["global"] == "pair_a"]  # fmt: skip
    assert entry["aliases"] == ["flow_header_ops", "pair_alias"]


def test_an_indexed_access_before_its_anchor_stays_in_the_anchors_object(image: Image) -> None:
    """``a5 = &pair_b + len; sd zero, -8(a5)``, as ``buf[len - 1] = 0``: the index is at least one,
    so the store is inside pair_b, not in pair_a before it."""
    facts = _at(image, A5, image.object_symbols["pair_b"][0].address, _add(A5, A5, A0), _store(ZERO, A5, -8))
    assert _operations(facts) == {("pair_b", "indexed_access")}


def test_a_code_marker_at_a_function_start_is_kept_as_a_name_of_the_address(image: Image) -> None:
    """_sinittext sits at the first instruction of the first init function. The C compares an
    address with _sinittext; the image cannot tell the marker from the function, so the marker's
    names go with the function's address for the C to match."""
    callee = image.functions["box_weight"][0].address
    image.zero_size_at[callee] = ["__init_begin", "_sinittext"]
    try:
        facts = _compared(image, callee)
    finally:
        del image.zero_size_at[callee]
    assert [item["target"] for item in facts.function_addresses] == ["box_weight"]
    assert facts.globals == []
    assert [(item["reason"], item["detail"]) for item in facts.exclusions] == [
        ("code_marker_at_function_start", {"function": "box_weight", "names": ["__init_begin", "_sinittext"]})
    ]


def test_a_load_in_a_loop_reads_the_first_elements_field_on_some_path(image: Image) -> None:
    """``s1 = &box_table + 16; loop: sd zero, -8(s1); s1 += 32; bne s1, a0, loop``, as a walk over an
    array of 32-byte elements: the store writes box_table's field at 8 on the loop's first pass."""
    facts = _at(image, S1, image.object_symbols["box_table"][0].address + 16,
                _store(ZERO, S1, -8), _addi(S1, S1, 32), _branch(S1, A0, -8) | 1 << 12)  # fmt: skip
    entries = [
        (item["global"], item["offset"]) for item in facts.globals if item["operation"] == "load_or_store_some_path"
    ]
    assert entries == [("box_table", 8)]


def test_a_loop_register_biased_to_a_member_gives_the_member_s_field_on_the_first_pass(image: Image) -> None:
    """``s1 = a0 + 0x40; loop: ld a1, 0x28(s1); s1 += 32; bne s1, a2, loop`` (rcu_exp_sel_wait_wake's
    loop over rcu_node, poll_freewait's over its entries): the head loses s1's constant, but the first
    pass reads the field at 64 + 40 from a0."""
    start = image.functions["anchor_pair"][0].address
    code = struct.pack(
        "<5I", _addi(S1, A0, 0x40), _load(A1, S1, 0x28), _addi(S1, S1, 32), _branch(S1, A2, -8) | 1 << 12, RET
    )
    assert 104 in image.decode("synthetic", code, start).memory_offsets


def test_an_access_inside_an_object_names_the_zero_size_symbols_at_its_start_apart(image: Image) -> None:
    """``ld a1, 8(s1)`` with s1 = &box_table, where a zero-size table start (``__start_rtab``) sits at
    box_table's address: the entry gives it as a start name for field offsets, never as an alias."""
    start = image.object_symbols["box_table"][0].address
    image.zero_size_at[start] = ["__start_rtab"]
    try:
        facts = _at(image, S1, start, _load(A1, S1, 8))
    finally:
        del image.zero_size_at[start]
    entries = [(item["global"], item.get("start_names"), item.get("aliases")) for item in facts.globals]
    assert ("box_table", ["__start_rtab"], None) in entries


def _frame_prologue() -> list[int]:
    """addi sp, sp, -64; sd s0, 48(sp); addi s0, sp, 64: s0 is the frame pointer, at sp on entry."""
    return [_addi(SP, SP, -64), _store(S0, SP, 48), _addi(S0, SP, 64)]


def _add(rd: int, rs1: int, rs2: int) -> int:
    return rs2 << 20 | rs1 << 15 | rd << 7 | 0x33


def test_frame_arithmetic_is_no_field_offset_where_s0_is_the_frame_pointer(image: Image) -> None:
    """After the prologue, ld a0, -24(s0) and addi a1, s0, -40 address the frame: neither adds a field
    offset. Without the prologue s0 is a value, and the same load reads its field at -24."""
    start = image.functions["anchor_pair"][0].address
    body = [_load(A0, S0, -24), _addi(A1, S0, -40), _load(A2, A1, 16), RET]
    framed = image.decode("synthetic", struct.pack(f"<{len(body) + 3}I", *_frame_prologue(), *body), start)
    assert not {-24, -40, -24 + 16, 48, 64, -64} & (framed.memory_offsets | framed.addi_constants)
    assert (-24, 8) in framed.frame_accesses and -40 in framed.frame_addresses
    plain = image.decode("synthetic", struct.pack("<2I", _load(A0, S0, -24), RET), start)
    assert -24 in plain.memory_offsets


def test_a_frame_address_in_another_register_reaches_the_frame(image: Image) -> None:
    """addi s1, s0, -40 is &batch.items; ld a0, 8(s1) reads the frame at -32, and a2 + s1 is
    batch.items[i], whose store is past a variable index from -40."""
    start = image.functions["anchor_pair"][0].address
    body = [_addi(S1, S0, -40), _load(A0, S1, 8), _add(A2, A2, S1), _store(A0, A2, 0), RET]
    facts = image.decode("synthetic", struct.pack(f"<{len(body) + 3}I", *_frame_prologue(), *body), start)
    assert (-32, 8) in facts.frame_accesses
    assert facts.frame_indexed == {(-40, 8)}
    assert not {8, -32} & facts.memory_offsets


def _framed(image: Image, body: list[int]) -> FunctionFacts:
    start = image.functions["anchor_pair"][0].address
    return image.decode("synthetic", struct.pack(f"<{len(body) + 3}I", *_frame_prologue(), *body), start)


def test_a_register_holds_a_frame_address_only_where_every_path_gives_it_one(image: Image) -> None:
    """do_sys_poll: s1 = &head (s0 - 40) enters the loop, which sets s1 to a heap list: at the loop's
    lw 12(s1) s1 is no frame address, so 12 is a field offset."""
    facts = _framed(
        image, [_addi(S1, S0, -40), _i(0x03, A0, 2, S1, 12), _load(S1, S1, 0), _branch(S1, ZERO, -8) | 1 << 12, RET]
    )
    assert 12 in facts.memory_offsets
    assert (-28, 4) not in facts.frame_accesses


def test_a_register_holding_either_of_two_frame_addresses_reaches_both(image: Image) -> None:
    """sgs = local_group ? &local_sgs : &tmp_sgs: the store through sgs is at either slot, and the
    displacement is no field offset. A register the instruction only writes (li a1) is no frame's."""
    body = [
        _branch(A1, ZERO, 12),
        _addi(S1, S0, -40),
        _jump(8),
        _addi(S1, S0, -48),
        _store(ZERO, S1, 8),
        _addi(A1, S0, -16),
        _addi(A1, ZERO, 0x30),
        RET,
    ]
    facts = _framed(image, body)
    assert {(-32, 8), (-40, 8)} <= facts.frame_accesses
    assert 8 not in facts.memory_offsets
    assert 0x30 in facts.addi_constants


def test_a_jump_within_the_function_keeps_the_frame_address_it_passes_on(image: Image) -> None:
    """force_sigsegv: both paths set a0 = &info and one jumps to the shared jal force_sig_info_to_task:
    a j inside the function is no call, so a0 still holds -40 at the jal."""
    body = [_branch(A1, ZERO, 12), _addi(A0, S0, -40), _jump(8), _addi(A0, S0, -40), _jal(RA, 0x100), RET]
    facts = _framed(image, body)
    assert {(position, offset) for _, position, offset in facts.frame_arguments} == {(0, -40)}


def test_a_store_over_part_of_a_frame_slot_ends_the_address_spilled_there(image: Image) -> None:
    """``sd s1, -24(s0)``, then ``sw zero, -20(s0)`` writes half of it: the reload is no address."""
    facts = _at(image, S1, image.object_symbols["pair_a"][0].address, _store(S1, S0, -24),
                _store(ZERO, S0, -20) & ~(1 << 12), _load(S1, S0, -24), _load(A1, S1, 8))  # fmt: skip
    assert "pair_b" not in {name for name, _ in _operations(facts)}


def test_a_loop_bound_inside_the_next_object_names_the_object_the_loop_walks(image: Image) -> None:
    """``p = &pair_a; end = &pair_a + 10; loop: p += 2; bne p, end``, as a strength-reduced loop
    over a field of each element: the bound lies inside pair_b, but in C the two pointers a loop
    compares point into one object, and p points into pair_a. (The loop's head loses p, so p's own
    address is an access of its own too.)"""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack(
        "<7I", _u(0x17, A5, high), _addi(A5, A5, low), _addi(S1, A5, 0), _addi(A3, A5, 10),
        _addi(S1, S1, 2), _branch(S1, A3, -4) | 1 << 12, RET,
    )  # fmt: skip
    entries = [(item["global"], item["offset"]) for item in image.decode("synthetic", code, start).globals]
    assert entries == [("pair_a", 0), ("pair_a", 10)]


def test_an_indexed_address_derived_from_after_a_join_that_lost_it_is_an_access(image: Image) -> None:
    """``a5 = &pair_a + a0`` on one path, a pointer loaded on the other; after the join
    ``s2 = a5 + 8; ld a2, 0(s2)``, as ``rq = cond ? this_rq() : dl_se->rq``: the load carries no
    object, so only the add shows the access."""
    start = image.functions["anchor_pair"][0].address
    high, low = _split(image.object_symbols["pair_a"][0].address - start)
    code = struct.pack(
        "<8I", _u(0x17, A5, high), _addi(A5, A5, low), _add(A5, A5, A0), _branch(A1, ZERO, 8),
        _load(A5, S1, 8), _addi(18, A5, 8), _load(A2, 18, 0), RET,
    )  # fmt: skip
    assert ("pair_a", "indexed_address") in _operations(image.decode("synthetic", code, start))


def test_a_far_call_passes_only_the_arguments_its_callee_takes(image: Image) -> None:
    """``a1 = &heap[sizeof heap]``, compared, then ``auipc ra; jalr ra`` to box_weight, which takes
    one argument: a1 does not reach it, so the address stays a loop's bound, as with ``jal``."""
    start = image.functions["anchor_pair"][0].address
    heap = image.object_symbols["heap"][0]
    high, low = _split(heap.address + heap.size - start)
    upper, lower = _split(image.functions["box_weight"][0].address - (start + 12))
    code = struct.pack(
        "<6I", _u(0x17, A1, high), _addi(A1, A1, low), _bltu(A0, A1, 4),
        _u(0x17, RA, upper), _i(0x67, RA, 0, RA, lower), RET,
    )  # fmt: skip
    saved = dict(image.arity)
    image.arity["box_weight"] = 1
    try:
        facts = image.decode("synthetic", code, start)
    finally:
        image.arity.clear()
        image.arity.update(saved)
    assert [(item["global"], item["offset"]) for item in facts.globals] == [("heap", heap.size)]


def test_a_base_plus_a_constant_spilled_to_the_frame_keeps_its_offset(image: Image) -> None:
    """``s2 = a0 + 2047; sd s2, 8(sp)``, s2 reused, ``ld s2, 8(sp); amoor.d zero, a1, (s2 + 352)``, as
    shrink_node's ``&pgdat->flags`` (pgdat + 4104) across a call: the field is 2399 bytes from a0."""
    start = image.functions["anchor_pair"][0].address
    amoor = (0b01000 << 27) | (A1 << 20) | (A0 << 15) | (3 << 12) | (ZERO << 7) | 0x2F  # amoor.d zero, a1, (a0)
    code = struct.pack(
        "<7I", _addi(18, A0, 2047), _store(18, SP, 8), _addi(18, ZERO, 0), _load(18, SP, 8),
        _addi(A0, 18, 352), amoor, RET,
    )  # fmt: skip
    assert 2399 in image.decode("synthetic", code, start).memory_offsets


def test_an_indexed_address_spilled_to_the_frame_keeps_its_object_and_offset(image: Image) -> None:
    """``s5 = &box_table + a0 + 200; sd s5, 8(sp)``, s5 reused, ``ld s5, 8(sp); sd zero, 40(s5)``, as
    sched_balance_softirq's ``this_rq()->next_balance`` (runqueues + cpu offset + 2448): the store
    is box_table's, at 240 bytes from it."""
    facts = _at(
        image, 21, image.object_symbols["box_table"][0].address, _add(21, 21, A0), _addi(21, 21, 200),
        _store(21, SP, 8), _addi(21, ZERO, 0), _load(21, SP, 8), _store(ZERO, 21, 40),
    )  # fmt: skip
    assert ("box_table", "indexed_access") in _operations(facts)
    assert 240 in facts.memory_offsets
