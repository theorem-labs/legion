"""The RISC-V image: its symbols, and the calls and data addresses of each function's machine code.

Calls come from ``jal``, ``j`` and ``c.j`` targets, and from ``jalr``/``jr`` through a register an
``auipc`` sequence set. Data addresses come from registers that ``auipc``-based arithmetic sets
(one anchor may serve several loads, compressed loads included); an address counts only inside an
ELF object symbol's declared size and only when ``System.map`` names that symbol. A load or store
through the thread pointer is an access to the per-CPU state the configuration names.
"""

from __future__ import annotations

import bisect
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, TypeVar

from capstone import CS_ARCH_RISCV, CS_MODE_RISCV64, CS_MODE_RISCVC, CS_OP_IMM, CS_OP_MEM, CS_OP_REG, Cs
from elftools.common.exceptions import ELFError
from elftools.elf.constants import SH_FLAGS
from elftools.elf.elffile import ELFFile
from elftools.elf.sections import SymbolTableSection

from .config import ImageRules, TrapTable
from .errors import InputError
from .imagecache import ImageCache

_COMPRESSED_MEMORY = {"c.ld", "c.lw", "c.sd", "c.sw", "c.fld", "c.fsd"}
_DIRECT_CALLS = {"jal", "j", "c.j", "c.jal"}
_REGISTER_CALLS = {"jalr", "c.jalr", "jr", "c.jr"}
_ADDI = {"addi", "addiw", "c.addi", "c.addiw"}
_MASK = (1 << 64) - 1
_BRANCHES = {
    "beq", "bne", "blt", "bge", "bltu", "bgeu", "beqz", "bnez", "blez", "bgez", "bltz", "bgtz", "bgt", "ble",
    "bgtu", "bleu", "c.beqz", "c.bnez",
}  # fmt: skip
# Instructions after which control does not continue. A trap is not one: a WARN resumes after it.
_ENDS = {"ret", "jr", "c.jr", "unimp", "c.unimp", "mret", "sret"}
# Traps, which end the flow where the image's trap table says they do not resume.
_TRAPS = {"ebreak", "c.ebreak"}
# Constants beyond this many bytes are addresses or masks, not field offsets.
_OFFSET_LIMIT = 1 << 24
# An addi this large on a base the code does not know is the first half of a split displacement.
_SPLIT_OFFSET = 1024
# Displacements a single load or store can reach; a sum beyond them comes from a split offset.
_DISPLACEMENT_REACH = 2048
# What a call passes and a function returns where the C does not say: every argument register, and
# a0 and a1.
_DEFAULT_ARGUMENTS = 8
_DEFAULT_RESULTS = 2
# The operations that form an address without reading it; a load or store through it names the
# object at the address it reads.
_ADDRESS_OPERATIONS = frozenset({"addi_address", "indexed_address"})
# LLVM's name for a block of globals it merged into one object.
_MERGED_GLOBALS = ".L_MergedGlobals"
# The registers a frame slot is addressed through.
_FRAME_REGISTERS = frozenset({"sp", "s0"})
_SP = frozenset({"sp"})
# Stores and loads of a whole register, which spill an address to a frame slot and reload it.
_SPILLS = frozenset({"sd", "c.sd", "c.sdsp"})
_RELOADS = frozenset({"ld", "c.ld", "c.ldsp"})
# The symbols that mark a change from code to data, which name no object of their own.
_MAPPING_SYMBOL = "$"
# The most addresses a register may hold, one from each path into a block, before it is unknown.
_MAX_CHOICES = 4
# The bytes each load moves (and _STORE_WIDTHS each store), for the frame slots of local objects.
_LOAD_WIDTHS = {
    "lb": 1, "lbu": 1, "lh": 2, "lhu": 2, "lw": 4, "lwu": 4, "ld": 8, "c.lw": 4, "c.ld": 8, "c.lwsp": 4,
    "c.ldsp": 8, "flw": 4, "fld": 8, "c.fld": 8, "c.fldsp": 8,
}  # fmt: skip
# The registers a function saves in its frame on entry and restores before it returns.
_CALLEE_SAVED = frozenset({"ra", *(f"s{number}" for number in range(12))})
_ARGUMENTS = frozenset(f"a{number}" for number in range(8))
# The instructions at a function's entry that save its registers, before its frame pointer is set.
_PROLOGUE_LIMIT = 32
# A value _State.meet keeps where both paths give it.
_V = TypeVar("_V")


@dataclass
class _State:
    """Registers that hold a known value (``constants`` are those formed with ``lui``/``li``, which
    are numbers, not addresses); registers that hold an unknown base plus a known constant
    (``relative``); registers that hold a known address plus an unknown offset (``anchored``: a
    per-CPU offset or an index added to the address of an object), with the address each was formed
    from (``origin``); the frame slots holding what a register spilled there held, an address, a
    base plus a constant or an anchored address (``slots``, keyed by the frame register and its
    displacement: ``(kind, value, origin)``); and registers every path into a block gives a known
    address (``choices``) or a known number (``numbers``), but not the same one: the value on each
    path."""

    values: dict[int, int] = field(default_factory=dict)
    relative: dict[int, int] = field(default_factory=dict)
    anchored: dict[int, int] = field(default_factory=dict)
    constants: set[int] = field(default_factory=set)
    origin: dict[int, int] = field(default_factory=dict)
    slots: dict[tuple[int, int], tuple[str, int, int]] = field(default_factory=dict)
    choices: dict[int, frozenset[int]] = field(default_factory=dict)
    numbers: dict[int, frozenset[int]] = field(default_factory=dict)

    def copy(self) -> _State:
        return _State(
            dict(self.values),
            dict(self.relative),
            dict(self.anchored),
            set(self.constants),
            dict(self.origin),
            dict(self.slots),
            dict(self.choices),
            dict(self.numbers),
        )

    def meet(self, other: _State) -> _State:
        def same(mine: dict[Any, _V], theirs: dict[Any, _V]) -> dict[Any, _V]:
            return {key: value for key, value in mine.items() if theirs.get(key) == value}

        values = {
            register: value
            for register, value in same(self.values, other.values).items()
            if (register in self.constants) == (register in other.constants)
        }
        anchored = same(self.anchored, other.anchored)

        def per_path(mine: Any, theirs: Any, registers: set[int]) -> dict[int, frozenset[int]]:
            merged: dict[int, frozenset[int]] = {}
            for register in registers - set(values):
                left, right = mine(register), theirs(register)
                if left is not None and right is not None and len(left | right) <= _MAX_CHOICES:
                    merged[register] = left | right
            return merged

        candidates = (set(self.values) | set(self.choices) | set(self.numbers)) & (
            set(other.values) | set(other.choices) | set(other.numbers)
        )
        return _State(
            values,
            same(self.relative, other.relative),
            anchored,
            {register for register in values if register in self.constants},
            {register: value for register, value in same(self.origin, other.origin).items() if register in anchored},
            same(self.slots, other.slots),
            per_path(self.possible, other.possible, candidates),
            per_path(self.possible_numbers, other.possible_numbers, candidates),
        )

    def address(self, register: int) -> int | None:
        """The address a register holds: a known value that is not a ``lui``/``li`` number."""
        if register in self.values and register not in self.constants:
            return self.values[register]
        return None

    def possible(self, register: int) -> frozenset[int] | None:
        """Every address the register may hold, one per path; None where some path leaves it
        unknown or a number."""
        if register in self.choices:
            return self.choices[register]
        address = self.address(register)
        return None if address is None else frozenset({address})

    def possible_numbers(self, register: int) -> frozenset[int] | None:
        """Every number the register may hold, one per path; None where some path leaves it
        unknown or an address."""
        if register in self.numbers:
            return self.numbers[register]
        if register in self.values and register in self.constants:
            return frozenset({self.values[register]})
        return None


_ATOMIC_PREFIXES = ("amo", "lr.", "sc.")
# Compressed two-operand forms read their destination: c.add rd, rs2 is rd = rd + rs2.
_READS_DESTINATION = {
    "c.add", "c.addw", "c.sub", "c.subw", "c.and", "c.or", "c.xor", "c.addi", "c.addiw", "c.slli", "c.srli",
    "c.srai", "c.andi",
}  # fmt: skip
_NUMBERS = {"lui", "c.lui", "li", "c.li"}
# Instructions whose result is the address they read plus something: the address lives on in it.
_DERIVES = {"addi", "addiw", "c.addi", "c.addiw", "mv", "c.mv", "add", "c.add", "addw", "c.addw"}
# Comparisons of two registers and differences of two: in C, two pointers compared or subtracted
# point into one object, so an address used only here may be the end of an object (a loop's bound).
_BOUNDS = {
    "beq", "bne", "blt", "bge", "bltu", "bgeu", "bgt", "ble", "bgtu", "bleu",
    "slt", "sltu", "sgt", "sgtu", "sub", "subw", "c.sub", "c.subw",
}  # fmt: skip
# The bytes each store writes.
_STORE_WIDTHS = {
    "sb": 1, "sh": 2, "sw": 4, "sd": 8, "c.sw": 4, "c.sd": 8, "c.swsp": 4, "c.sdsp": 8,
    "fsw": 4, "fsd": 8, "c.fsd": 8, "c.fsdsp": 8,
}  # fmt: skip


def _two_registers(operands: list[Any]) -> bool:
    """Whether an instruction reads two registers (``bltu a0, a1``), not one and zero or a number."""
    return sum(1 for operand in operands if operand.type == CS_OP_REG) >= 2


def _holds(state: _State | None, register: int) -> bool:
    """Whether a state still knows what a register holds: an address, an address plus an index,
    one of several addresses, or an unknown base plus a constant."""
    return state is not None and (
        state.address(register) is not None
        or register in state.anchored
        or register in state.relative
        or register in state.choices
    )


def _retreating(successors: list[list[int]]) -> frozenset[tuple[int, int]]:
    """The edges of a depth-first search that lead back to an instruction still on its stack: a
    loop's back edges. Without them every path from the entry runs each loop's body once."""
    stage = [0] * len(successors)  # 0 unseen, 1 on the stack, 2 finished
    found: set[tuple[int, int]] = set()
    for root in range(len(successors)):
        if stage[root]:
            continue
        stage[root] = 1
        stack = [(root, iter(successors[root]))]
        while stack:
            node, following = stack[-1]
            for child in following:
                if stage[child] == 0:
                    stage[child] = 1
                    stack.append((child, iter(successors[child])))
                    break
                if stage[child] == 1:
                    found.add((node, child))
            else:
                stage[node] = 2
                stack.pop()
    return frozenset(found)


def _reads(instruction: Any, operands: list[Any]) -> tuple[set[int], set[int]]:
    """The registers an instruction reads as the base of a memory access, and those it reads as values."""
    references = _memory_references(instruction, operands)
    bases = {base for base, _ in references}
    registers = [operand.reg for operand in operands if operand.type == CS_OP_REG]
    mnemonic = instruction.mnemonic
    if mnemonic.startswith(_ATOMIC_PREFIXES):
        # amoadd.d rd, rs2, (rs1): rd is written, rs2 is a value, rs1 the base.
        values = set(registers[1:-1])
    elif mnemonic in _NO_DESTINATION or mnemonic in _READS_DESTINATION:
        values = set(registers)
    else:
        values = set(registers[1:])
    if mnemonic in _COMPRESSED_MEMORY and len(operands) == 3:
        values = {operands[0].reg} if mnemonic.startswith(("c.s", "c.fs")) else set()
    return bases, values - bases


def _is_number(instruction: Any, operands: list[Any], state: _State) -> bool:
    """Whether the value an instruction computes is a number (rooted in ``lui``/``li``), not an address."""
    mnemonic = instruction.mnemonic
    if mnemonic in _NUMBERS:
        return True
    sources = [operand.reg for operand in operands[1:] if operand.type == CS_OP_REG]
    if mnemonic in _READS_DESTINATION and operands and operands[0].type == CS_OP_REG:
        sources.append(operands[0].reg)
    known = [register for register in sources if register in state.values]
    return bool(known) and all(register in state.constants for register in known)


def _anchored_value(
    instruction: Any,
    operands: list[Any],
    state: _State,
    section_of: Any,
) -> tuple[int, int, int] | None:
    """A register that becomes a known address plus an unknown offset: ``add rd, address, offset``,
    ``add rd, anchored, offset``, or ``addi``/``mv`` of such a register. The result is the register,
    the address it holds, and the address it was formed from (the anchor's origin)."""
    values, anchored, origin = state.values, state.anchored, state.origin
    mnemonic = instruction.mnemonic
    kinds = [operand.type for operand in operands]
    if mnemonic in ("add", "c.add") and kinds and all(kind == CS_OP_REG for kind in kinds):
        registers = [operand.reg for operand in operands]
        left, right = (registers[1], registers[2]) if len(registers) == 3 else (registers[0], registers[1])
        for address, offset in ((left, right), (right, left)):
            known = state.address(address)
            if offset in state.numbers:
                # A number each path gives differently: the sum is one known address per path.
                continue
            if known is not None and offset not in values and section_of(known) is not None:
                return registers[0], known, known
            if address in anchored and offset not in values:
                return registers[0], anchored[address], origin.get(address, anchored[address])
        return None
    for source, value in (
        (operands[1].reg if kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM] else None, 2),
        (operands[0].reg if kinds == [CS_OP_REG, CS_OP_IMM] else None, 1),
    ):
        if source is None or source not in anchored:
            continue
        if mnemonic in ("addi", "addiw") and value == 2:
            return operands[0].reg, (anchored[source] + operands[2].imm) & _MASK, origin.get(source, anchored[source])
        if mnemonic in ("c.addi", "c.addiw") and value == 1:
            return operands[0].reg, (anchored[source] + operands[1].imm) & _MASK, origin.get(source, anchored[source])
    if mnemonic in ("mv", "c.mv") and kinds == [CS_OP_REG, CS_OP_REG] and operands[1].reg in anchored:
        source = operands[1].reg
        return operands[0].reg, anchored[source], origin.get(source, anchored[source])
    return None


def _chosen_value(instruction: Any, operands: list[Any], state: _State) -> tuple[int, frozenset[int]] | None:
    """A register that becomes one of several known addresses: ``mv`` of a register holding them,
    ``addi`` of one (each address plus the immediate), or ``add`` of a known address and a number
    each path gives differently (or of several addresses and one number)."""
    mnemonic = instruction.mnemonic
    kinds = [operand.type for operand in operands]
    if mnemonic in ("mv", "c.mv") and kinds == [CS_OP_REG, CS_OP_REG] and operands[1].reg in state.choices:
        return operands[0].reg, state.choices[operands[1].reg]
    if mnemonic in ("add", "c.add") and kinds and all(kind == CS_OP_REG for kind in kinds):
        registers = [operand.reg for operand in operands]
        left, right = (registers[1], registers[2]) if len(registers) == 3 else (registers[0], registers[1])
        for address, number in ((left, right), (right, left)):
            addresses, numbers = state.possible(address), state.possible_numbers(number)
            if addresses is None or numbers is None or (len(addresses) == 1 and len(numbers) == 1):
                continue
            sums = frozenset((first + second) & _MASK for first in addresses for second in numbers)
            if len(sums) <= _MAX_CHOICES:
                return registers[0], sums
        return None
    if mnemonic in ("addi", "addiw") and kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM]:
        source, immediate = operands[1].reg, operands[2].imm
    elif mnemonic in ("c.addi", "c.addiw") and kinds == [CS_OP_REG, CS_OP_IMM]:
        source, immediate = operands[0].reg, operands[1].imm
    else:
        return None
    if source not in state.choices:
        return None
    return operands[0].reg, frozenset((address + immediate) & _MASK for address in state.choices[source])


def _flow(decoder: Any, instructions: list[Any], never_returns: Any = None, halts: Any = None) -> list[list[int]]:
    """The successors of each instruction: the next one, a branch target, none after a return, an
    indirect jump, a jump to an address outside the function, a trap that does not resume
    (``halts(address)``; a BUG, where a WARN's trap resumes), or a call to a function that never
    returns (``never_returns(address)``; panic, rest_init), after which the compiler places code
    that only other paths reach."""
    index_of = {instruction.address: index for index, instruction in enumerate(instructions)}
    successors: list[list[int]] = []
    for index, instruction in enumerate(instructions):
        mnemonic = instruction.mnemonic
        operands = list(instruction.operands)
        following = [index + 1] if index + 1 < len(instructions) else []
        target = None
        if (mnemonic in _BRANCHES or mnemonic in {"j", "c.j"}) and operands and operands[-1].type == CS_OP_IMM:
            target = index_of.get((instruction.address + operands[-1].imm) & _MASK)
        if mnemonic in _BRANCHES:
            successors.append(following + ([target] if target is not None else []))
        elif mnemonic in {"j", "c.j"}:
            successors.append([target] if target is not None else [])
        elif (
            mnemonic in _ENDS
            or (mnemonic in {"jalr", "c.jalr"} and not _links(decoder, operands))
            or (mnemonic in _TRAPS and halts is not None and halts(instruction.address))
        ):
            successors.append([])
        elif never_returns is not None and (callee := _callee(decoder, instructions, index)) is not None:
            successors.append([] if never_returns(callee) else following)
        else:
            successors.append(following)
    return successors


def _callee(decoder: Any, instructions: list[Any], index: int) -> int | None:
    """The address a call at ``index`` goes to: a ``jal`` target, or an ``auipc`` and ``jalr`` pair
    (a call beyond the reach of ``jal``). None for anything else, a call through a register included."""
    instruction = instructions[index]
    operands = list(instruction.operands)
    if instruction.mnemonic in ("jal", "c.jal") and operands and operands[-1].type == CS_OP_IMM:
        return int(instruction.address + operands[-1].imm) & _MASK
    if instruction.mnemonic != "jalr" or not index or not _links(decoder, operands):
        return None
    registers = [operand.reg for operand in operands if operand.type == CS_OP_REG]
    memory = [operand for operand in operands if operand.type == CS_OP_MEM]
    base, offset = (
        (memory[0].mem.base, memory[0].mem.disp)
        if memory
        else (registers[-1], next((operand.imm for operand in operands if operand.type == CS_OP_IMM), 0))
    )
    previous = instructions[index - 1]
    before = list(previous.operands)
    if previous.mnemonic != "auipc" or not before or before[0].type != CS_OP_REG or before[0].reg != base:
        return None
    upper = before[1].imm & 0xFFFFF
    if upper & 0x80000:
        upper -= 0x100000
    return int(previous.address + (upper << 12) + offset) & _MASK


def _links(decoder: Any, operands: list[Any]) -> bool:
    """A ``jalr`` that writes a return address (a call), not one through ``zero`` (a jump)."""
    registers = [operand.reg for operand in operands if operand.type == CS_OP_REG]
    return len(registers) < 2 or decoder.reg_name(registers[0]) != "zero"


def _relative_value(
    instruction: Any, operands: list[Any], values: dict[int, int], relative: dict[int, int]
) -> tuple[int, int] | None:
    """A register that becomes an unknown base plus a constant: ``add rd, base, c`` where ``c`` was
    formed with ``lui`` (and ``addi``), or ``addi``/``mv`` of such a register."""
    mnemonic = instruction.mnemonic
    kinds = [operand.type for operand in operands]
    if mnemonic in ("add", "c.add") and kinds and all(kind == CS_OP_REG for kind in kinds):
        registers = [operand.reg for operand in operands]
        left, right = (registers[1], registers[2]) if len(registers) == 3 else (registers[0], registers[1])
        for constant, base in ((left, right), (right, left)):
            if constant in values and base not in values and abs(values[constant]) < _OFFSET_LIMIT:
                return registers[0], values[constant] + relative.get(base, 0)
        return None
    if mnemonic in ("addi", "addiw") and kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM] and operands[1].reg in relative:
        return operands[0].reg, relative[operands[1].reg] + operands[2].imm
    if (
        mnemonic in ("addi", "addiw")
        and kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM]
        and operands[1].reg not in values
        and operands[2].imm
    ):
        # addi s3, s2, 2047 before ld 481(s3): a field beyond the 12-bit displacement of one load,
        # split in two. A smaller constant is split the same way when the base is reused.
        return operands[0].reg, operands[2].imm
    if mnemonic in ("c.addi", "c.addiw") and kinds == [CS_OP_REG, CS_OP_IMM] and operands[0].reg in relative:
        return operands[0].reg, relative[operands[0].reg] + operands[1].imm
    if mnemonic in ("mv", "c.mv") and kinds == [CS_OP_REG, CS_OP_REG] and operands[1].reg in relative:
        return operands[0].reg, relative[operands[1].reg]
    return None


@dataclass(frozen=True)
class Symbol:
    address: int
    size: int
    name: str
    section: str
    local: bool
    file: str  # the basename of the ELF FILE symbol a local symbol follows; "" for globals


@dataclass
class FunctionFacts:
    calls: list[dict[str, Any]] = field(default_factory=list)
    globals: list[dict[str, Any]] = field(default_factory=list)
    function_addresses: list[dict[str, Any]] = field(default_factory=list)
    outside: list[dict[str, Any]] = field(default_factory=list)
    memory_offsets: set[int] = field(default_factory=set)
    addi_constants: set[int] = field(default_factory=set)
    exclusions: list[dict[str, Any]] = field(default_factory=list)
    # The frame: each load or store through sp, s0 or a register holding a frame address, as (offset
    # from the value sp had on entry; bytes moved), without the saves and restores of callee-saved
    # registers; the frame addresses the code forms (addi rd, sp|s0, k; mv rd, sp); those it passes to
    # a call (the call's address, the argument's position, the offset); and the loads and stores past a
    # variable index added to a frame address (fbatch.folios[i]), at the address's offset.
    frame_accesses: set[tuple[int, int]] = field(default_factory=set)
    frame_addresses: set[int] = field(default_factory=set)
    frame_arguments: set[tuple[int, int, int]] = field(default_factory=set)
    frame_indexed: set[tuple[int, int]] = field(default_factory=set)


_RECORD_FIELDS = ("calls", "globals", "function_addresses", "outside", "exclusions")
_NUMBER_FIELDS = ("memory_offsets", "addi_constants", "frame_addresses")
# Sets of integer tuples, by their length.
_TUPLE_FIELDS = {"frame_accesses": 2, "frame_arguments": 3, "frame_indexed": 2}


def _facts_from_cache(value: dict[str, Any]) -> FunctionFacts | None:
    """The facts a cache entry holds, or None when it does not have their shape."""
    if set(value) != {*_RECORD_FIELDS, *_NUMBER_FIELDS, *_TUPLE_FIELDS}:
        return None
    records = [value[name] for name in _RECORD_FIELDS]
    numbers = [value[name] for name in _NUMBER_FIELDS]
    if not all(isinstance(items, list) and all(isinstance(item, dict) for item in items) for items in records):
        return None
    if not all(
        isinstance(items, list) and all(isinstance(item, int) and not isinstance(item, bool) for item in items)
        for items in numbers
    ):
        return None
    tuples: dict[str, set[Any]] = {}
    for name, length in _TUPLE_FIELDS.items():
        items = value[name]
        if not isinstance(items, list) or not all(
            isinstance(item, list) and len(item) == length and all(isinstance(part, int) for part in item)
            for item in items
        ):
            return None
        tuples[name] = {tuple(item) for item in items}
    calls, globals_, function_addresses, outside, exclusions = records
    memory_offsets, addi_constants, frame_addresses = numbers
    return FunctionFacts(
        calls=calls,
        globals=globals_,
        function_addresses=function_addresses,
        outside=outside,
        memory_offsets=set(memory_offsets),
        addi_constants=set(addi_constants),
        exclusions=exclusions,
        frame_accesses=tuples["frame_accesses"],
        frame_addresses=set(frame_addresses),
        frame_arguments=tuples["frame_arguments"],
        frame_indexed=tuples["frame_indexed"],
    )


def _facts_for_cache(facts: FunctionFacts) -> dict[str, Any]:
    return {
        "calls": facts.calls,
        "globals": facts.globals,
        "function_addresses": facts.function_addresses,
        "outside": facts.outside,
        "memory_offsets": sorted(facts.memory_offsets),
        "addi_constants": sorted(facts.addi_constants),
        "exclusions": facts.exclusions,
        "frame_accesses": [list(item) for item in sorted(facts.frame_accesses)],
        "frame_addresses": sorted(facts.frame_addresses),
        "frame_arguments": [list(item) for item in sorted(facts.frame_arguments)],
        "frame_indexed": [list(item) for item in sorted(facts.frame_indexed)],
    }


def _hex(value: int) -> str:
    return f"0x{value:x}"


_COMPILER_SUFFIX = re.compile(r"\.\d+$")


def source_name(symbol: str) -> str:
    """The C name of a data symbol: the compiler appends ``.<n>`` when it renames a static copy
    (``func.var.3``) or splits a global (``files_stat.0``)."""
    return _COMPILER_SUFFIX.sub("", symbol)


def read_system_map(path: Path) -> list[tuple[int, str, str]]:
    try:
        lines = path.read_text().splitlines()
    except OSError as error:
        raise InputError(f"cannot read System.map {path}: {error}") from error
    entries: list[tuple[int, str, str]] = []
    for number, line in enumerate(lines, start=1):
        pieces = line.split()
        if len(pieces) != 3 or len(pieces[1]) != 1:
            raise InputError(f"System.map {path} line {number}: expected address type name")
        try:
            address = int(pieces[0], 16)
        except ValueError as error:
            raise InputError(f"System.map {path} line {number}: address is not hexadecimal") from error
        entries.append((address, pieces[1], pieces[2]))
    return sorted(entries)


def read_trap_table(data: bytes, section_address: int, table: TrapTable) -> dict[int, bool]:
    """Each trap the table at ``section_address`` lists, and whether it resumes. A trap listed twice
    resumes when either entry says so."""
    if len(data) % table.entry_size:
        raise InputError(
            f"trap table {table.section} holds {len(data)} bytes, not a whole number of {table.entry_size}-byte entries"
        )
    traps: dict[int, bool] = {}
    for start in range(0, len(data), table.entry_size):
        address_field = start + table.address_offset
        value = int.from_bytes(
            data[address_field : address_field + table.address_size], "little", signed=table.relative
        )
        address = (section_address + address_field + value) & _MASK if table.relative else value
        flags_field = start + table.flags_offset
        flags = int.from_bytes(data[flags_field : flags_field + table.flags_size], "little")
        traps[address] = traps.get(address, False) or bool(flags & table.resume_flags)
    return traps


def code_symbol(kind: str, size: int, *, executable: bool) -> bool:
    """Whether a symbol is a routine the C can call or take the address of. An assembly routine
    (handle_exception, ret_from_fork_kernel_asm) carries a size but no function type, and a
    zero-size marker often sits at its first instruction."""
    return executable and bool(size) and kind in ("STT_FUNC", "STT_NOTYPE")


def resolve_sized(symbols: list[tuple[int, int, str, str]], address: int) -> tuple[str, int, str] | None:
    """The symbol whose declared size covers ``address``: its name, its start and its section.

    Zero-size markers between a symbol's start and ``address`` do not hide the symbol.
    """
    index = bisect.bisect_right(symbols, (address, 1 << 64, "\uffff", "\uffff")) - 1
    for candidate in range(index, max(-1, index - 16), -1):
        start, size, name, section = symbols[candidate]
        if size and start <= address < start + size:
            return name, start, section
        if size:
            return None
    return None


class Image:
    def __init__(self, image: Path, system_map: Path, rules: ImageRules, cache_directory: Path | None = None) -> None:
        self.rules = rules
        self.map = read_system_map(system_map)
        named = {(address, name) for address, _, name in self.map}
        text_kinds = {"T", "t", "W", "w"}
        self.text_at: dict[int, str] = {}
        for address, kind, name in self.map:
            if kind in text_kinds:
                self.text_at.setdefault(address, name)
        self.text_starts = sorted(self.text_at)
        try:
            self._load_elf(image, named)
        except (OSError, ELFError) as error:
            handle = getattr(self, "handle", None)
            if handle is not None:
                handle.close()
            raise InputError(f"cannot parse ELF {image}: {error}") from error
        self.decoder = Cs(CS_ARCH_RISCV, CS_MODE_RISCV64 | CS_MODE_RISCVC)
        self.decoder.detail = True
        self.cache: dict[int, FunctionFacts] = {}
        self.section_data: dict[str, bytes] = {}
        # The number of parameters of each function the C defines, for the argument registers a
        # call passes, and the registers it returns in; set by the caller before facts are read.
        # Where unknown: _DEFAULT_ARGUMENTS arguments, _DEFAULT_RESULTS result registers.
        self.arity: dict[str, int] = {}
        self.results: dict[str, int] = {}
        # Whether the function at an address cannot return, decided once per callee.
        self.no_return: dict[int, bool] = {}
        # Each trap the image's trap table lists, and whether it resumes ([image] trap_table).
        self.traps: dict[int, bool] = {} if rules.trap_table is None else self._read_traps(image, rules.trap_table)
        self.image_cache = ImageCache(cache_directory, image, system_map, rules, Path(__file__))

    def _read_traps(self, image: Path, table: TrapTable) -> dict[int, bool]:
        section = self.elf.get_section_by_name(table.section)
        if section is None:
            self.handle.close()
            raise InputError(f"image {image} has no section {table.section}, which [image] trap_table names")
        try:
            data = section.data()
        except (OSError, ELFError) as error:
            self.handle.close()
            raise InputError(f"cannot read section {table.section} of {image}: {error}") from error
        return read_trap_table(data, int(section["sh_addr"]), table)

    def _halts(self, address: int) -> bool:
        """Whether the trap at ``address`` stops: the trap table lists it, without the resume flag."""
        return self.traps.get(address) is False

    def _load_elf(self, image: Path, named: set[tuple[int, str]]) -> None:
        self.handle = image.open("rb")
        self.elf = ELFFile(self.handle)
        self.sections: list[tuple[int, int, str, bool]] = []
        for section in self.elf.iter_sections():
            flags = int(section["sh_flags"])
            if flags & SH_FLAGS.SHF_ALLOC and int(section["sh_size"]):
                start = int(section["sh_addr"])
                self.sections.append(
                    (start, start + int(section["sh_size"]), section.name, bool(flags & SH_FLAGS.SHF_EXECINSTR))
                )
        self.sections.sort()
        self.functions: dict[str, list[Symbol]] = {}
        objects: list[tuple[int, int, str, str]] = []
        function_ranges: list[tuple[int, int, str, str]] = []
        self.object_symbols: dict[str, list[Symbol]] = {}
        markers: list[tuple[int, str]] = []
        routines: list[tuple[int, str]] = []
        # LLVM merges several globals of one unit into a .L_MergedGlobals block and addresses the
        # block once, so the block's start is also its first member's address. The block is a
        # compiler artifact, which System.map leaves out: it is read from the symbol table alone.
        self.merged_blocks: dict[int, int] = {}
        # The symbols System.map leaves out, by address: a jump table or a constant pool
        # (.LJTI..., .Lswitch.table...). An address they start at belongs to them, not to the
        # object that ends there. Mapping symbols ($d, $x) mark a data or code change, not an
        # object, so they name nothing.
        self.hidden_starts: set[int] = set()
        symtab = self.elf.get_section_by_name(".symtab")
        current_file = ""
        for symbol in symtab.iter_symbols() if isinstance(symtab, SymbolTableSection) else []:
            kind = symbol["st_info"]["type"]
            if kind == "STT_FILE":
                current_file = symbol.name
                continue
            address = int(symbol["st_value"])
            if symbol.name.startswith(_MERGED_GLOBALS) and int(symbol["st_size"]):
                self.merged_blocks[address] = int(symbol["st_size"])
            if (address, symbol.name) not in named:
                if symbol.name and not symbol.name.startswith(_MAPPING_SYMBOL):
                    self.hidden_starts.add(address)
                continue
            if not isinstance(symbol["st_shndx"], int):
                continue
            section = self.elf.get_section(symbol["st_shndx"])
            if section is None:
                continue
            local = symbol["st_info"]["bind"] == "STB_LOCAL"
            entry = Symbol(
                address, int(symbol["st_size"]), symbol.name, section.name, local, current_file if local else ""
            )
            executable = bool(int(section["sh_flags"]) & SH_FLAGS.SHF_EXECINSTR)
            if kind == "STT_FUNC":
                self.functions.setdefault(symbol.name, []).append(entry)
                function_ranges.append((entry.address, entry.size, entry.name, entry.section))
            elif code_symbol(kind, entry.size, executable=executable):
                # An assembly routine: code the C calls and takes the address of, with no function
                # type of its own.
                function_ranges.append((entry.address, entry.size, entry.name, entry.section))
                routines.append((entry.address, entry.name))
            elif kind in ("STT_OBJECT", "STT_NOTYPE") and not executable:
                objects.append((entry.address, entry.size, entry.name, entry.section))
                self.object_symbols.setdefault(symbol.name, []).append(entry)
            elif kind == "STT_NOTYPE" and not entry.size:
                # A marker in code (_stext, _sinittext, secondary_start_sbi) that C compares or passes.
                markers.append((entry.address, entry.name))
        for symbols in self.functions.values():
            symbols.sort(key=lambda item: item.address)
        # A call target is named by the code symbol that starts there; System.map's zero-size
        # markers at the same address (__sched_text_start, __entry_text_end) name it only where no
        # function or assembly routine does.
        for address, name in sorted(routines):
            self.text_at[address] = name
        for name, copies in sorted(self.functions.items()):
            for copy in copies:
                self.text_at[copy.address] = name
        self.objects = sorted(set(objects))
        self.function_ranges = sorted(set(function_ranges))
        self.sized_object_names = sorted({name for _, size, name, _ in self.objects if size})
        # The sized objects that end exactly at an address. The C forms such an address as the end
        # of that object (a loop bound, &array[count]); the image cannot tell it from the address
        # of whatever follows, so both name it.
        ends: dict[int, set[tuple[str, str, int]]] = {}
        for start, size, name, section_name in self.objects:
            if size:
                ends.setdefault(start + size, set()).add((source_name(name), section_name, start))
        self.object_ends = {address: sorted(names) for address, names in ends.items()}
        # Names an address has without storage of their own: zero-size symbols (jiffies, which the
        # linker script makes an alias of jiffies_64; an empty lock_class_key; a section marker).
        zero_size: dict[int, set[str]] = {}
        for address, size, name, _ in self.objects:
            if not size:
                zero_size.setdefault(address, set()).add(source_name(name))
        for address, name in markers:
            zero_size.setdefault(address, set()).add(source_name(name))
        # Names without a leading underscore first: vdso_start before __end_init_stack.
        self.zero_size_at = {
            address: sorted(names, key=lambda name: (name.startswith("_"), name))
            for address, names in zero_size.items()
        }
        # The registers holding a frame address before each instruction of the function being
        # decoded (_decode sets them from _frame): what they add is the frame's, no field offset.
        self.frame_rooted: dict[int, frozenset[str]] = {}
        # The C names of every symbol System.map lists, for telling a global the compiler removed.
        self.map_source_names = frozenset(source_name(name) for _, _, name in self.map)

    def close(self) -> None:
        self.handle.close()

    def section_of(self, address: int) -> tuple[int, int, str, bool] | None:
        index = bisect.bisect_right(self.sections, (address, 1 << 64, "\uffff", True)) - 1
        if index >= 0 and self.sections[index][0] <= address < self.sections[index][1]:
            return self.sections[index]
        return None

    def symbol_class(self, name: str, section: str) -> str:
        return self.rules.symbol_class(name, section)

    def _end(self, symbol: Symbol) -> int:
        if symbol.size:
            return symbol.address + symbol.size
        index = bisect.bisect_right(self.text_starts, symbol.address)
        return self.text_starts[index] if index < len(self.text_starts) else symbol.address + 4

    def _code(self, symbol: Symbol) -> bytes | None:
        """A function's machine code, or None where it lies outside executable sections."""
        section = self.section_of(symbol.address)
        if section is None or not section[3]:
            return None
        data = self.section_data.get(section[2])
        if data is None:
            elf_section = self.elf.get_section_by_name(section[2])
            data = elf_section.data() if elf_section is not None else b""
            self.section_data[section[2]] = data
        start = symbol.address - section[0]
        return data[start : start + max(0, self._end(symbol) - symbol.address)]

    def never_returns(self, address: int) -> bool:
        """Whether the function that starts at ``address`` cannot return to its caller: its code
        holds no return, no jump through a register and no jump out of itself, so it ends only in a
        loop, a trap, or a call that does not return either (rest_init, panic). Where the code is
        not fully decoded, or no sized function starts there, it may return."""
        known = self.no_return.get(address)
        if known is not None:
            return known
        result = False
        name = self.text_at.get(address, "")
        symbol = next((item for item in self.functions.get(name, []) if item.address == address and item.size), None)
        code = self._code(symbol) if symbol is not None else None
        if code:
            instructions = list(self.decoder.disasm(code, address))
            end = address + len(code)
            result = sum(item.size for item in instructions) == len(code) and not any(
                self._leaves(item, address, end) for item in instructions
            )
        self.no_return[address] = result
        return result

    def _leaves(self, instruction: Any, start: int, end: int) -> bool:
        """Whether an instruction returns or jumps out of the function at [start, end)."""
        mnemonic = instruction.mnemonic
        operands = list(instruction.operands)
        if mnemonic in ("ret", "jr", "c.jr", "mret", "sret"):
            return True
        if mnemonic in ("jalr", "c.jalr") and not _links(self.decoder, operands):
            return True
        if (mnemonic in _BRANCHES or mnemonic in ("j", "c.j")) and operands and operands[-1].type == CS_OP_IMM:
            target = (instruction.address + operands[-1].imm) & _MASK
            return not start <= target < end
        return False

    def facts(self, symbol: Symbol) -> FunctionFacts:
        cached = self.cache.get(symbol.address)
        if cached is not None:
            return cached
        facts = FunctionFacts()
        self.cache[symbol.address] = facts
        code = self._code(symbol)
        if code is None:
            return facts
        symbol_key = ""
        if self.image_cache.enabled:
            symbol_key = self.image_cache.symbol_key(symbol.name, symbol.address, symbol.size)
            callees = self.image_cache.callees(symbol_key)
            if callees is not None:
                facts_key = self._facts_key(symbol_key, symbol.name, callees)
                payload = self.image_cache.load(facts_key)
                if payload is not None:
                    loaded = _facts_from_cache(payload)
                    if loaded is None:
                        raise self.image_cache.malformed(facts_key)
                    self.cache[symbol.address] = loaded
                    return loaded
        instructions = list(self.decoder.disasm(code, symbol.address))
        facts = self._decode(symbol.name, instructions)
        self.cache[symbol.address] = facts
        if self.image_cache.enabled:
            callees = self._direct_callees(instructions)
            self.image_cache.store_callees(symbol_key, callees)
            self.image_cache.store(self._facts_key(symbol_key, symbol.name, callees), _facts_for_cache(facts))
        return facts

    def _facts_key(self, symbol_key: str, name: str, callees: list[str]) -> str:
        """The key of a function's facts: the only C facts its decoding reads are its own result
        registers and the parameter counts of the functions it calls or jumps to directly."""
        return self.image_cache.facts_key(
            symbol_key,
            self.results.get(name, _DEFAULT_RESULTS),
            {callee: self.arity.get(callee, _DEFAULT_ARGUMENTS) for callee in callees},
        )

    def _direct_callees(self, instructions: list[Any]) -> list[str]:
        """The text functions ``_arguments`` may read the arity of: every direct jump target and
        every callee of an ``auipc`` and ``jalr`` pair."""
        return sorted(
            {
                name
                for index in range(len(instructions))
                for address in [self._target(instructions, index)]
                if address is not None
                for name in [self.text_at.get(address)]
                if name is not None
            }
        )

    def _target(self, instructions: list[Any], index: int) -> int | None:
        """The address a direct call or jump at ``index`` goes to, or a far call's (``_callee``)."""
        instruction = instructions[index]
        operands = list(instruction.operands)
        if instruction.mnemonic in _DIRECT_CALLS and operands and operands[-1].type == CS_OP_IMM:
            return int(instruction.address + operands[-1].imm) & _MASK
        return _callee(self.decoder, instructions, index)

    def decode(self, name: str, code: bytes, address: int) -> FunctionFacts:
        """The facts of a function's machine code that starts at ``address``."""
        return self._decode(name, list(self.decoder.disasm(code, address)))

    def _decode(self, name: str, instructions: list[Any]) -> FunctionFacts:
        facts = FunctionFacts()
        successors = _flow(self.decoder, instructions, self.never_returns, self._halts)
        states = self._block_states(instructions, successors)
        before = dict(states)
        first = dict(self._block_states(instructions, successors, _retreating(successors)))
        returned = {f"a{position}" for position in range(self.results.get(name, _DEFAULT_RESULTS))}
        frame = _frame(self.decoder, instructions, successors)
        self.frame_rooted = frame.rooted
        for index, state in states:
            instruction = instructions[index]
            escapes, partner = None, None
            if self._forms_address(instruction, state):
                compares: list[tuple[int, int]] = []
                escapes = self._escapes(instructions, successors, before, index, returned, compares=compares)
                if escapes == "compared":
                    partner = self._partner(compares, before, first)
            self._first_pass(name, instruction, state, first.get(index), facts)
            self._step(name, instruction, state, facts, escapes, partner)
        facts.frame_accesses, facts.frame_addresses = frame.accesses, frame.addresses
        facts.frame_arguments, facts.frame_indexed = frame.arguments, frame.indexed
        return facts

    def _partner(
        self, compares: list[tuple[int, int]], before: dict[int, _State], first: dict[int, _State]
    ) -> int | None:
        """The address a loop's bound is compared with, on every path or on the loop's first pass:
        in C both point into one object."""
        for position, register in sorted(compares):
            for states in (before, first):
                state = states.get(position)
                address = None if state is None else state.address(register)
                if address is not None:
                    return address
        return None

    def _forms_address(self, instruction: Any, state: _State) -> bool:
        """Whether the instruction forms an address the escape walk judges: an ``addi``, or an index
        added to a known address."""
        if instruction.mnemonic in _ADDI:
            return True
        return instruction.mnemonic in ("add", "c.add") and (
            _anchored_value(instruction, list(instruction.operands), state, self.section_of) is not None
        )

    def _first_pass(
        self, function: str, instruction: Any, state: _State, first: _State | None, facts: FunctionFacts
    ) -> None:
        """A load or store in a loop whose base the loop changes (``p += sizeof *p``): the state that
        holds over every path loses it at the loop's head, but on the loop's first pass the base is
        the address it enters with. The access there is one this instruction makes on some path."""
        if first is None:
            return
        for base, displacement in _memory_references(instruction, list(instruction.operands)):
            if self.decoder.reg_name(base) == "tp" or state.address(base) is not None or base in state.anchored:
                continue
            if base in state.choices or base in state.constants:
                continue
            rooted = self.frame_rooted.get(instruction.address, _SP)
            if (
                base not in state.relative
                and base in first.relative
                and first.address(base) is None
                and self.decoder.reg_name(base) not in rooted
            ):
                # An unknown base the loop register was biased from (s1 = a0 + 0x40 before the loop):
                # on the first pass the field is that far from it, as _step counts it outside loops.
                facts.memory_offsets.add(first.relative[base] + displacement)
            entered = first.address(base)
            if entered is not None:
                self._record_address(
                    function, (entered + displacement) & _MASK, instruction.address, "load_or_store_some_path", facts
                )

    def _arguments(self, instructions: list[Any], index: int) -> set[str]:
        """The argument registers a call or a tail jump passes: as many as the callee's C takes."""
        count = _DEFAULT_ARGUMENTS
        target = self._target(instructions, index)
        callee = None if target is None else self.text_at.get(target)
        if callee is not None:
            count = self.arity.get(callee, _DEFAULT_ARGUMENTS)
        return {f"a{position}" for position in range(count)}

    def _escapes(
        self,
        instructions: list[Any],
        successors: list[list[int]],
        before: dict[int, _State],
        index: int,
        returned: set[str],
        limit: int = 256,
        compares: list[tuple[int, int]] | None = None,
    ) -> str | None:
        """Why the address an instruction sets is used, on some path, other than as the base of loads
        and stores whose state still holds it; None when it is not. ``passed`` to a call (in an
        argument the callee takes), ``returned`` (in a register the function's C result uses),
        ``used`` (stored, computed with, tested), ``join`` (a load whose state lost it), ``limit``
        (the walk did not finish), or ``compared`` when its only such use is as one side of a
        comparison or a difference of two addresses: a loop's bound. ``compares`` gets each such
        comparison's instruction and the register on its other side. Registers that hold the same
        address (``mv``) or the address plus an index (``add``) are followed; an ``addi`` of it forms
        another address, judged at that ``addi``. Callee-saved registers survive calls. Only an
        escaping address is itself an access; a base gives the accesses of its loads and stores."""
        operands = list(instructions[index].operands)
        if not operands or operands[0].type != CS_OP_REG:
            return "used"
        pending = [(following, frozenset({operands[0].reg})) for following in successors[index]]
        seen: set[tuple[int, frozenset[int]]] = set()
        compared = False
        while pending and limit:
            limit -= 1
            position, tracked = pending.pop()
            if (position, tracked) in seen:
                continue
            seen.add((position, tracked))
            instruction = instructions[position]
            mnemonic = instruction.mnemonic
            current = list(instruction.operands)
            bases, reads = _reads(instruction, current)
            state = before.get(position)
            for base in bases & tracked:
                if not _holds(state, base):
                    # The state at this load lost the address (a join), so only the addi shows it.
                    return "join"
            written = frozenset(_written_registers(instruction, current))
            used = reads & tracked
            if used and mnemonic in _DERIVES and written and not all(_holds(state, register) for register in used):
                # A register derived from it after a join that lost it (``addi s2, rq, 2047``): its
                # loads carry no object, so only this instruction shows it.
                return "join"
            if used and not (mnemonic in _DERIVES and written):
                if mnemonic not in _BOUNDS or not _two_registers(current):
                    return "used"
                # One side of a comparison: the other paths may still show more.
                compared = True
                if compares is not None:
                    compares += [(position, register) for register in sorted(reads - tracked)]
            names = {self.decoder.reg_name(register) for register in tracked}
            calls = mnemonic in {"jal", "c.jal"} or (mnemonic in {"jalr", "c.jalr"} and _links(self.decoder, current))
            tail = mnemonic in {"j", "c.j"} and not successors[position]
            indirect = mnemonic in {"jr", "c.jr"} and not (current and self.decoder.reg_name(current[0].reg) == "ra")
            if calls or tail or indirect:
                # An argument register the callee takes reaches it; across a call only callee-saved
                # registers keep their values.
                if names & self._arguments(instructions, position):
                    return "passed"
                tracked = frozenset(
                    register for register in tracked if self.decoder.reg_name(register).startswith(("s", "sp", "gp"))
                )
            if mnemonic in _ENDS or tail:
                if names & returned and not indirect:
                    return "returned"
                continue
            if (used and mnemonic in _ADDI) or mnemonic in _BOUNDS:
                # address + k is another address, judged at its own addi; a comparison's result is
                # a truth value.
                tracked = tracked - written
            else:
                # mv and add keep the address (plus an index); any other write ends it there.
                tracked = (tracked | written) if used else (tracked - written)
            if tracked:
                pending += [(following, tracked) for following in successors[position]]
        if pending:
            return "limit"
        return "compared" if compared else None

    def _block_states(
        self,
        instructions: list[Any],
        successors: list[list[int]],
        ignored: frozenset[tuple[int, int]] = frozenset(),
    ) -> list[tuple[int, _State]]:
        """Each instruction's index with the register state before it, from a forward must-analysis
        over the function's basic blocks: a register holds a value at a block's start only when every
        path into the block gives it that value, so an address formed before a branch and used after
        a join is followed, and one formed on only one path is not. Blocks no instruction leads to are
        reached through an indirect jump (a jump table): they start from the state those jumps leave.
        The ``ignored`` edges (a loop's back edges) carry no state: then a loop's head holds what the
        loop enters with, the state of its first pass."""
        if not instructions:
            return []
        leaders = {0}
        for index, following in enumerate(successors):
            if following != [index + 1]:
                leaders.update(following)
                if index + 1 < len(instructions):
                    leaders.add(index + 1)
        reached = {successor for following in successors for successor in following}
        starts = sorted(leaders)
        ends = {start: (starts[position + 1] if position + 1 < len(starts) else len(instructions))
                for position, start in enumerate(starts)}  # fmt: skip
        entry: dict[int, _State | None] = {start: None for start in starts}
        entry[0] = _State()
        indirect: list[_State] = []

        def settle(pending: list[int]) -> None:
            budget = 64 * len(starts)
            while pending and budget:
                budget -= 1
                start = pending.pop()
                state = entry[start]
                assert state is not None
                state = state.copy()
                for index in range(start, ends[start]):
                    self._step("", instructions[index], state, None)
                last = instructions[ends[start] - 1]
                operands = list(last.operands)
                if last.mnemonic in {"jr", "c.jr"} and not (
                    operands and self.decoder.reg_name(operands[0].reg) == "ra"
                ):
                    indirect.append(state.copy())
                for successor in successors[ends[start] - 1]:
                    if (ends[start] - 1, successor) in ignored:
                        continue
                    known = entry.get(successor)
                    merged = state.copy() if known is None else known.meet(state)
                    if known is None or merged != known:
                        entry[successor] = merged
                        pending.append(successor)

        settle([0])
        orphans = [start for start in starts if entry[start] is None and start not in reached]
        if orphans and indirect:
            seed = indirect[0]
            for state in indirect[1:]:
                seed = seed.meet(state)
            for start in orphans:
                entry[start] = seed.copy()
            settle(list(reversed(orphans)))
        result = []
        for start in starts:
            state = (entry[start] or _State()).copy()
            for index in range(start, ends[start]):
                result.append((index, state.copy()))
                self._step("", instructions[index], state, None)
        return result

    def _record_address(
        self,
        function: str,
        address: int,
        instruction: int,
        operation: str,
        facts: FunctionFacts,
        bound: bool = False,
        partner: int | None = None,
    ) -> None:
        """The object an address names. ``bound`` says the code uses the address only as a loop's
        bound: then it may be the end of the object before it, which C compares and never reads.
        ``partner`` is the address the bound is compared with: in C the two point into one object."""
        provenance = [{"source": "image", "address": _hex(instruction)}]
        section = self.section_of(address)
        zero_size = list(self.zero_size_at.get(address, []))
        ended = self.object_ends.get(address, []) if bound else []
        if operation in _ADDRESS_OPERATIONS and address in self.merged_blocks:
            # The start of a merged-globals block is its first member's address, but forming it
            # accesses no member: each is reached by a displacement from it, recorded where it is.
            # The names the start has go with it, for a C access that takes the first member's
            # address (an empty lock_class_key passed by address).
            starting = bisect.bisect_left(self.objects, (address,))
            names = set(zero_size)
            while starting < len(self.objects) and self.objects[starting][0] == address:
                names.add(source_name(self.objects[starting][2]))
                starting += 1
            facts.exclusions.append(
                {
                    "kind": "image_access",
                    "reason": "merged_globals_block_base",
                    "function": function,
                    "target": _hex(address),
                    "detail": {"block_bytes": self.merged_blocks[address], "names": sorted(names)},
                    "provenance": provenance,
                }
            )
            return
        resolved = resolve_sized(self.objects, address) if section is not None else None
        target = resolve_sized(self.function_ranges, address) if resolved is None and section else None
        walked = resolve_sized(self.objects, partner) if bound and partner is not None else None
        if (
            walked is not None
            and walked[1] <= address
            and walked != resolved
            and not zero_size
            and (resolved is None or resolved[1] != address)
        ):
            # A bound past the end of the object the loop walks (&flags[N].desc), inside the next
            # object or outside every one: the object the loop walks, which its compare names.
            resolved, target = walked, None
        if resolved is None and target is None and zero_size:
            # Only zero-size names at the address: an empty object, a marker the code compares, or
            # a section's end (_etext, _edata, _end), which belongs to no section at all.
            resolved = (zero_size[0], address, section[2] if section else "")
        if resolved is None and target is None and ended and address not in self.hidden_starts:
            # One past the end of an object, and nothing the image names starts there: the C's end
            # pointer, which is the only name the address can have.
            name, section_name, start = ended[0]
            resolved = (name, start, section_name)
        if resolved is None:
            if target is not None:
                name, start, section_name = target
                facts.function_addresses.append(
                    {
                        "source": function,
                        "target": name,
                        "address": _hex(address),
                        "symbol_address": _hex(start),
                        "offset": address - start,
                        "section": section_name,
                        "operation": operation,
                        "provenance": provenance,
                    }
                )
                if zero_size and start == address:
                    # Zero-size markers at the function's first instruction (_sinittext,
                    # __sched_text_start, __entry_text_end): the C may have meant the function or
                    # any of them, and the image cannot tell. The function's entry is above; the
                    # markers' names go with the address, for a C access to one of them.
                    facts.exclusions.append(
                        {
                            "kind": "image_access",
                            "reason": "code_marker_at_function_start",
                            "function": function,
                            "target": _hex(address),
                            "detail": {"function": name, "names": sorted(zero_size)},
                            "provenance": provenance,
                        }
                    )
                return
            if section is not None:
                facts.outside.append(
                    {
                        "function": function,
                        "address": _hex(address),
                        "section": section[2],
                        "operation": operation,
                        "provenance": provenance,
                    }
                )
            return
        symbol, start, section_name = resolved
        name = source_name(symbol)
        entry = {
            "function": function,
            "global": name,
            "symbol": symbol,
            "symbol_address": _hex(start),
            "offset": address - start,
            "section": section_name,
            "target_class": self.symbol_class(name, section_name),
            "operation": operation,
            "provenance": provenance,
        }
        aliases = sorted({alias for alias in zero_size if alias != name} | {end for end, _, _ in ended if end != name})
        if aliases:
            # The address cannot tell these names apart: jiffies is jiffies_64; a zero-size key
            # shares the address of the object after it; a loop's bound at the start of one object
            # may be the end of the object before it.
            entry["aliases"] = aliases
        start_names = (
            [marker for marker in self.zero_size_at.get(start, []) if marker != name] if start != address else []
        )
        if start_names:
            # Zero-size symbols at the object's start (a linker table's __start_x): the C may name the
            # table by one and reach a field from there. Only field offsets read these; as aliases
            # they would match any access to the name at any offset inside the object.
            entry["start_names"] = start_names
        facts.globals.append(entry)

    def _step(
        self,
        function: str,
        instruction: Any,
        state: _State,
        facts: FunctionFacts | None,
        escapes: str | None = "used",
        partner: int | None = None,
    ) -> None:
        """One instruction: its facts (when ``facts`` is given) and its effect on the register state.

        ``escapes`` says why an address the instruction forms is used other than as a load or store
        base (``_escapes``), or None when it is not; ``partner`` is what a bound is compared with."""
        values, relative, anchored = state.values, state.relative, state.anchored
        operands = list(instruction.operands)
        mnemonic = instruction.mnemonic
        computed = _computed_value(instruction, operands, values, state.constants)
        if facts is not None:
            rooted = self.frame_rooted.get(instruction.address, _SP)
            frame = {self.decoder.reg_name(operand.reg) for operand in operands if operand.type == CS_OP_REG} & rooted
            if mnemonic in _ADDI and operands and operands[-1].type == CS_OP_IMM and not frame:
                # Frame arithmetic (addi s0, sp, 16) is the frame's (frame_addresses), no field offset.
                facts.addi_constants.add(operands[-1].imm)
                source = operands[-2].reg if len(operands) == 3 and operands[-2].type == CS_OP_REG else operands[0].reg
                if source in relative:
                    facts.addi_constants.add(relative[source] + operands[-1].imm)
            _, reads = _reads(instruction, operands)
            if self.rules.thread_pointer and any(self.decoder.reg_name(register) == "tp" for register in reads):
                # The thread pointer used as a value (passed, stored, offset): the current task itself.
                facts.globals.append(self._thread_pointer(function, instruction.address, 0, "thread_pointer_value"))
            for base, displacement in _memory_references(instruction, operands):
                if self.decoder.reg_name(base) not in rooted:
                    # A frame slot's displacement is the frame's (frame_accesses), no field offset.
                    facts.memory_offsets.add(displacement)
                if self.decoder.reg_name(base) == "tp":
                    if self.rules.thread_pointer:
                        facts.globals.append(
                            self._thread_pointer(function, instruction.address, displacement, "thread_pointer_access")
                        )
                elif state.address(base) is not None:
                    self._record_address(
                        function, (values[base] + displacement) & _MASK, instruction.address, "load_or_store", facts
                    )
                elif base in anchored:
                    # A known address plus a per-CPU offset or an index the code computed: the
                    # object at the known address and the displacement, and the field offset from
                    # the address the anchor was formed from.
                    origin = state.origin.get(base, anchored[base])
                    self._record_address(
                        function,
                        self._in_origin((anchored[base] + displacement) & _MASK, origin),
                        instruction.address,
                        "indexed_access",
                        facts,
                    )
                    if self.decoder.reg_name(base) not in rooted:
                        facts.memory_offsets.add(anchored[base] - origin + displacement)
                elif base in state.choices:
                    # Every path into this load gives the base a known address, but not the same
                    # one: the function reads each of those objects on some path.
                    for address in sorted(state.choices[base]):
                        self._record_address(
                            function,
                            (address + displacement) & _MASK,
                            instruction.address,
                            "load_or_store_some_path",
                            facts,
                        )
                elif base in relative and self.decoder.reg_name(base) not in rooted:
                    # A base register the code has already added a constant to: the field's offset
                    # from the object the register pointed at is the sum, which a load or store
                    # beyond the reach of a 12-bit displacement is always split into.
                    facts.memory_offsets.add(relative[base] + displacement)
            if mnemonic in _DIRECT_CALLS and operands and operands[-1].type == CS_OP_IMM:
                self._call(
                    function,
                    instruction.address,
                    (instruction.address + operands[-1].imm) & _MASK,
                    mnemonic in {"j", "c.j"},
                    facts,
                )
            elif mnemonic in _REGISTER_CALLS:
                target = _register_target(operands, {register: value for register, value in values.items()
                                                     if register not in state.constants})  # fmt: skip
                if target is not None:
                    tail = mnemonic in {"jr", "c.jr"} or not _links(self.decoder, operands)
                    self._call(function, instruction.address, target, tail, facts)
        if mnemonic in {"jal", "c.jal"} or (mnemonic in {"jalr", "c.jalr"} and _links(self.decoder, operands)):
            # A call may change every caller-saved register.
            for register in (*values, *relative, *anchored, *state.choices, *state.numbers):
                name = self.decoder.reg_name(register)
                if name == "ra" or name.startswith(("a", "t")):
                    values.pop(register, None)
                    relative.pop(register, None)
                    anchored.pop(register, None)
                    state.choices.pop(register, None)
                    state.numbers.pop(register, None)
                    state.constants.discard(register)
        derived = _relative_value(instruction, operands, values, relative)
        indexed = _anchored_value(instruction, operands, state, self.section_of)
        chosen = _chosen_value(instruction, operands, state)
        number = computed is not None and _is_number(instruction, operands, state)
        for register in _written_registers(instruction, operands):
            values.pop(register, None)
            relative.pop(register, None)
            anchored.pop(register, None)
            state.choices.pop(register, None)
            state.numbers.pop(register, None)
            state.constants.discard(register)
        if chosen is not None:
            state.choices[chosen[0]] = chosen[1]
        if computed is not None:
            destination, value = computed
            values[destination] = value
            if number:
                # lui and li form numbers (an ioctl command, a mask), not addresses.
                state.constants.add(destination)
            elif facts is not None and mnemonic in _ADDI and escapes:
                self._record_address(
                    function, value, instruction.address, "addi_address", facts, escapes == "compared", partner
                )
        elif indexed is not None:
            anchored[indexed[0]] = indexed[1]
            state.origin[indexed[0]] = indexed[2]
            if facts is not None and escapes and (mnemonic in _ADDI or mnemonic in ("add", "c.add")):
                # An index added to a known address, or a per-CPU or indexed address plus a field
                # offset: the object the sum names, whatever the index is.
                self._record_address(
                    function,
                    self._in_origin(indexed[1], indexed[2]),
                    instruction.address,
                    "indexed_address",
                    facts,
                    escapes == "compared",
                    partner,
                )
        elif derived is not None:
            relative[derived[0]] = derived[1]
            names = {self.decoder.reg_name(operand.reg) for operand in operands if operand.type == CS_OP_REG}
            if facts is not None and not names & self.frame_rooted.get(instruction.address, _SP):
                # The code formed "a base plus a constant": the offset of the field it addresses,
                # which an index added after it would lose (not of the frame, which _frame keeps).
                facts.memory_offsets.add(derived[1])
        self._frame_slot(instruction, operands, state)

    def _frame_slot(self, instruction: Any, operands: list[Any], state: _State) -> None:
        """A register holding an address, a base plus a constant (``relative``) or an anchored
        address, spilled to a frame slot and reloaded from it (a value kept across calls in a long
        function: shrink_node's pgdat + 3752, sched_balance_softirq's this_rq() + 2047). Without
        this the value is lost at the reload, although every path gives the slot the same one. A
        store over any byte of a slot ends what it held, and so does any store through the other
        frame register (whose distance to this one the code does not show) or a move of the frame
        register itself. Spill slots are not memory the code passes on, so a call does not end them."""
        mnemonic = instruction.mnemonic
        for register in _written_registers(instruction, operands):
            if register in {key[0] for key in state.slots}:
                state.slots = {key: value for key, value in state.slots.items() if key[0] != register}
        width = _STORE_WIDTHS.get(mnemonic, 8 if mnemonic.startswith(_ATOMIC_PREFIXES) else None)
        if width is None and mnemonic not in _RELOADS:
            return
        references = _memory_references(instruction, operands)
        if not references or not operands or operands[0].type != CS_OP_REG:
            return
        base, displacement = references[0]
        if self.decoder.reg_name(base) not in _FRAME_REGISTERS:
            return
        register = operands[0].reg
        if width is not None:
            state.slots = {
                (slot_base, slot): value
                for (slot_base, slot), value in state.slots.items()
                if slot_base == base and (slot + 8 <= displacement or displacement + width <= slot)
            }
            held = self._spilled(register, state) if mnemonic in _SPILLS else None
            if held is not None:
                state.slots[(base, displacement)] = held
        elif (base, displacement) in state.slots:
            kind, value, origin = state.slots[(base, displacement)]
            if kind == "address":
                state.values[register] = value
                state.constants.discard(register)
            elif kind == "relative":
                state.relative[register] = value
            else:
                state.anchored[register] = value
                state.origin[register] = origin

    def _spilled(self, register: int, state: _State) -> tuple[str, int, int] | None:
        """What a frame slot keeps of a register spilled to it: its address, its constant from an
        unknown base, or its anchored address and origin; nothing for a number or an unknown value."""
        address = state.address(register)
        if address is not None:
            return ("address", address, address)
        if register in state.anchored:
            return ("anchored", state.anchored[register], state.origin.get(register, state.anchored[register]))
        if register in state.relative and register not in state.values:
            return ("relative", state.relative[register], 0)
        return None

    def _in_origin(self, address: int, origin: int) -> int:
        """An address formed from ``origin`` plus an index and a negative displacement
        (``buf[len - 1]``): in C the index is never negative, so the address stays inside the
        object at ``origin``; where the displacement takes it below that object, the object is named
        at its origin. Above the origin it is the object there: a merged-globals block's base plus
        an index and the displacement of a later member."""
        if address >= origin:
            return address
        home = resolve_sized(self.objects, origin)
        if home is None or resolve_sized(self.objects, address) == home:
            return address
        return origin

    def _thread_pointer(self, function: str, instruction: int, offset: int, operation: str) -> dict[str, Any]:
        return {
            "function": function,
            "global": self.rules.thread_pointer,
            "symbol_address": "tp",
            "offset": offset,
            "section": "",
            "target_class": "per_cpu",
            "operation": operation,
            "provenance": [{"source": "image", "address": _hex(instruction)}],
        }

    def _call(self, function: str, instruction: int, target: int, tail: bool, facts: FunctionFacts) -> None:
        callee = self.text_at.get(target)
        if callee is not None:
            facts.calls.append(
                {
                    "source": function,
                    # The compiler may call another library function for the one C names (bcmp for
                    # a memcmp whose result is only compared with zero).
                    "target": self.rules.call_aliases.get(callee, callee),
                    "instruction": _hex(instruction),
                    "tail": tail,
                    "provenance": [{"source": "image", "address": _hex(instruction)}],
                }
            )
            return
        facts.exclusions.append(
            {
                "kind": "image_call",
                "reason": "target_not_text",
                "function": function,
                "target": _hex(target),
                "detail": self._nearest_symbol(target),
                "provenance": [{"source": "image", "address": _hex(instruction)}],
            }
        )

    def _nearest_symbol(self, address: int) -> str:
        index = bisect.bisect_right(self.map, (address, "\uffff", "\uffff")) - 1
        if index < 0:
            return "no System.map symbol at or before the target"
        symbol_address = self.map[index][0]
        names: list[str] = []
        while index >= 0 and self.map[index][0] == symbol_address:
            names.append(self.map[index][2])
            index -= 1
        return f"{', '.join(sorted(names))} at {_hex(symbol_address)}"


# Instructions whose first register operand is read, not written: stores, branches, and jumps
# through a register. Everything else (sub, sll, sext.w, seqz, sc.d, ...) writes its first operand.
_NO_DESTINATION = {
    "sb", "sh", "sw", "sd", "fsw", "fsd", "c.sw", "c.sd", "c.swsp", "c.sdsp", "c.fsd", "c.fsdsp",
    "beq", "bne", "blt", "bge", "bltu", "bgeu", "beqz", "bnez", "blez", "bgez", "bltz", "bgtz", "bgt", "ble",
    "bgtu", "bleu", "c.beqz", "c.bnez", "ret", "ecall", "ebreak", "fence", "fence.i", "jr", "c.jr",
}  # fmt: skip


def _written_registers(instruction: Any, operands: list[Any]) -> tuple[int, ...]:
    """Capstone's RISC-V binding lacks regs_access; the destination is the first register operand."""
    if not operands or operands[0].type != CS_OP_REG or instruction.mnemonic in _NO_DESTINATION:
        return ()
    return (operands[0].reg,)


def _memory_references(instruction: Any, operands: list[Any]) -> list[tuple[int, int]]:
    references = [(operand.mem.base, operand.mem.disp) for operand in operands if operand.type == CS_OP_MEM]
    if (
        instruction.mnemonic in _COMPRESSED_MEMORY
        and len(operands) == 3
        and operands[1].type == CS_OP_IMM
        and operands[2].type == CS_OP_REG
    ):
        references.append((operands[2].reg, operands[1].imm))
    if not references and instruction.mnemonic.startswith(_ATOMIC_PREFIXES) and operands:
        # Capstone gives the address of an atomic, (rs1), as a plain register operand.
        last = operands[-1]
        if last.type == CS_OP_REG:
            references.append((last.reg, 0))
    return references


@dataclass
class _Frame:
    """A function's frame facts (``_frame``), and the registers holding a frame address before each
    instruction (by its address): sp, s0 once the prologue sets it from sp, and the registers an
    address of the frame was moved into."""

    accesses: set[tuple[int, int]]
    addresses: set[int]
    arguments: set[tuple[int, int, int]]
    indexed: set[tuple[int, int]]
    rooted: dict[int, frozenset[str]]


# A register the frame analysis follows: whether it holds a frame address or one plus a variable
# index ("held" / "indexed"), and the offsets it may hold. More offsets than this is no address.
_Rooted = dict[str, tuple[str, frozenset[int]]]
_FRAME_OFFSETS = 4


def _frame_deltas(decoder: Any, instructions: list[Any]) -> tuple[dict[str, tuple[int, int]], set[int]]:
    """The prologue: sp's and s0's offsets from the value sp had on entry, each with the index from
    which it holds (``addi sp, sp, -n``; ``addi s0, sp, m``), and the offsets where it saves
    callee-saved registers through sp before s0 is set (the frame's own, as are their restores)."""
    deltas: dict[str, tuple[int, int]] = {}
    saved: set[int] = set()
    for position, instruction in enumerate(instructions[:_PROLOGUE_LIMIT]):
        operands = list(instruction.operands)
        mnemonic = instruction.mnemonic
        registers = [decoder.reg_name(operand.reg) for operand in operands if operand.type == CS_OP_REG]
        immediate = next((operand.imm for operand in operands if operand.type == CS_OP_IMM), None)
        if mnemonic.startswith(("addi", "c.addi")) and registers and immediate is not None:
            target, source = registers[0], registers[-1]
            if target == source == "sp" and "sp" not in deltas and immediate < 0:
                deltas["sp"] = (immediate, position + 1)
            elif target == "s0" and source == "sp" and "s0" not in deltas and "sp" in deltas:
                deltas["s0"] = (deltas["sp"][0] + immediate, position + 1)
            continue
        if "sp" in deltas and "s0" not in deltas and mnemonic in _STORE_WIDTHS and registers[:1]:
            value = registers[0]
            references = _memory_references(instruction, operands)
            if mnemonic.endswith("sp") and len(operands) == 3 and operands[1].type == CS_OP_IMM:
                references.append((operands[2].reg, operands[1].imm))
            for base, displacement in references:
                if decoder.reg_name(base) == "sp" and value in _CALLEE_SAVED:
                    saved.add(deltas["sp"][0] + displacement)
    return deltas, saved


def _frame_call(decoder: Any, instruction: Any, following: list[int]) -> bool:
    """A call, or a tail call (a ``j`` out of the function); a ``j`` within it is a jump."""
    mnemonic = instruction.mnemonic
    if mnemonic in ("j", "c.j"):
        return not following
    return mnemonic in _DIRECT_CALLS or (mnemonic in _REGISTER_CALLS and _links(decoder, list(instruction.operands)))


def _frame_step(decoder: Any, instruction: Any, state: _Rooted, deltas: dict[str, int], call: bool) -> _Rooted:
    """The registers holding a frame address after an instruction, from those before it."""
    operands = list(instruction.operands)
    mnemonic = instruction.mnemonic
    registers = [decoder.reg_name(operand.reg) for operand in operands if operand.type == CS_OP_REG]
    immediate = next((operand.imm for operand in operands if operand.type == CS_OP_IMM), None)
    if call or mnemonic in _ENDS:
        # A callee-saved register keeps &object across a call; the others do not.
        return {name: value for name, value in state.items() if name in _CALLEE_SAVED}
    jumps = mnemonic in _BRANCHES or mnemonic in ("j", "c.j")
    written = registers[0] if registers and mnemonic not in _STORE_WIDTHS and not jumps else None
    if written is None or written in _FRAME_REGISTERS or written == "zero":
        return state

    def source(name: str) -> tuple[str, frozenset[int]] | None:
        if name in deltas:
            return "held", frozenset({deltas[name]})
        return state.get(name)

    after = {name: value for name, value in state.items() if name != written}
    found: tuple[str, frozenset[int]] | None = None
    if mnemonic.startswith(("addi", "c.addi")) and immediate is not None:
        origin = source(registers[-1])
        if origin is not None and registers[-1] == written and origin[0] == "held" and written not in deltas:
            # A pointer that walks itself (addi s1, s1, 4 in a loop over a local array): an index.
            found = ("indexed", origin[1])
        elif origin is not None:
            found = (origin[0], frozenset(offset + immediate for offset in origin[1]))
    elif mnemonic in ("mv", "c.mv") and len(registers) == 2:
        found = source(registers[1])
    elif mnemonic in ("add", "c.add"):
        # rd = a frame address + a register: the address plus a variable index.
        operands_read = registers[1:] if mnemonic == "add" else registers
        known = [source(name) for name in operands_read]
        if len(operands_read) == 2 and sum(value is not None for value in known) == 1:
            found = ("indexed", next(value for value in known if value is not None)[1])
    if found is not None and len(found[1]) <= _FRAME_OFFSETS:
        after[written] = found
    return after


def _frame_join(states: list[_Rooted]) -> _Rooted:
    """The registers every incoming path roots the same way, with every offset they may hold."""
    first, *rest = states
    joined: _Rooted = {}
    for name, (kind, offsets) in first.items():
        others = [state.get(name) for state in rest]
        if all(other is not None and other[0] == kind for other in others):
            merged = offsets.union(*(other[1] for other in others if other is not None))
            if len(merged) <= _FRAME_OFFSETS:
                joined[name] = (kind, merged)
    return joined


def _frame(decoder: Any, instructions: list[Any], successors: list[list[int]]) -> _Frame:
    """The frame accesses, the frame addresses, the frame addresses passed in argument registers to
    calls (the call's address, the argument's position, the offset), and the frame accesses past a
    variable index of a function, as offsets from the value sp had on entry (``_frame_deltas``).

    Which registers hold a frame address before each instruction is a forward must-analysis over the
    function's flow: ``addi``/``mv`` from sp, s0 or such a register forms one (``addi s3, s0, -312``
    for &fbatch.folios[0]), ``add`` of one and another register one plus a variable index
    (fbatch.folios[i]), a call keeps only the callee-saved ones, and a join keeps a register only
    where every incoming path holds a frame address in it (do_sys_poll's s1 is the frame's list head
    on entry to its loop, a heap list after it), with every offset those paths give
    (sched_balance_find_dst_cpu's &local_sgs or &idlest_sgs)."""
    prologue, saved = _frame_deltas(decoder, instructions)
    count = len(instructions)
    entry: dict[int, _Rooted] = {0: {}} if count else {}
    pending = [0] if count else []
    while pending:
        index = pending.pop()
        deltas = {name: offset for name, (offset, start) in prologue.items() if start <= index}
        call = _frame_call(decoder, instructions[index], successors[index])
        after = _frame_step(decoder, instructions[index], entry[index], deltas, call)
        for following in successors[index]:
            known = entry.get(following)
            merged = after if known is None else _frame_join([known, after])
            if known is None or merged != known:
                entry[following] = merged
                pending.append(following)
    accesses: set[tuple[int, int]] = set()
    addresses: set[int] = set()
    arguments: set[tuple[int, int, int]] = set()
    indexed_accesses: set[tuple[int, int]] = set()
    rooted: dict[int, frozenset[str]] = {}
    for index, instruction in enumerate(instructions):
        deltas = {name: offset for name, (offset, start) in prologue.items() if start <= index}
        state = entry.get(index, {})
        operands = list(instruction.operands)
        mnemonic = instruction.mnemonic
        registers = [decoder.reg_name(operand.reg) for operand in operands if operand.type == CS_OP_REG]
        bases, values = _reads(instruction, operands)
        read = {decoder.reg_name(register) for register in bases | values}
        # The registers holding a frame address that the instruction reads (not one it only writes).
        rooted[instruction.address] = frozenset({"sp", *deltas, *state} & (read | {"sp"}))
        if _frame_call(decoder, instruction, successors[index]):
            arguments.update(
                (instruction.address, int(name[1:]), offset)
                for name, (kind, offsets) in state.items()
                if name in _ARGUMENTS and kind == "held"
                for offset in offsets
            )
            continue
        after = _frame_step(decoder, instruction, state, deltas, False)
        written = registers[0] if registers and mnemonic not in _STORE_WIDTHS else None
        if (
            written is not None
            and written in after
            and after[written][0] == "held"
            and after[written] != state.get(written)
        ):
            addresses.update(after[written][1])
        width = _STORE_WIDTHS.get(mnemonic) or _LOAD_WIDTHS.get(mnemonic)
        if width is None:
            continue
        references = _memory_references(instruction, operands)
        if mnemonic.endswith("sp") and len(operands) == 3 and operands[1].type == CS_OP_IMM:
            # c.sdsp ra, 0x18(sp): capstone gives the stack pointer as a third register operand.
            references.append((operands[2].reg, operands[1].imm))
        for base, displacement in references:
            name = decoder.reg_name(base)
            if name in deltas:
                accesses.add((deltas[name] + displacement, width))
            elif name in state:
                kind, offsets = state[name]
                target = accesses if kind == "held" else indexed_accesses
                target.update((offset + displacement, width) for offset in offsets)
    return _Frame({item for item in accesses if item[0] not in saved}, addresses, arguments, indexed_accesses, rooted)


def _register_target(operands: list[Any], values: dict[int, int]) -> int | None:
    registers = [operand.reg for operand in operands if operand.type == CS_OP_REG]
    offset = next((operand.imm for operand in operands if operand.type == CS_OP_IMM), 0)
    memory = next((operand for operand in operands if operand.type == CS_OP_MEM), None)
    if memory is not None:
        return (values[memory.mem.base] + memory.mem.disp) & _MASK if memory.mem.base in values else None
    if not registers:
        return None
    base = registers[-1]
    return (values[base] + offset) & _MASK if base in values else None


def _computed_value(
    instruction: Any, operands: list[Any], values: dict[int, int], constants: set[int]
) -> tuple[int, int] | None:
    """Evaluate the address-preserving forms RISC-V code generation uses, modulo 2**64."""
    computed = _computed(instruction, operands, values, constants)
    return None if computed is None else (computed[0], computed[1] & _MASK)


def _computed(
    instruction: Any, operands: list[Any], values: dict[int, int], constants: set[int]
) -> tuple[int, int] | None:
    mnemonic = instruction.mnemonic
    kinds = [operand.type for operand in operands]
    if mnemonic == "auipc" and kinds == [CS_OP_REG, CS_OP_IMM]:
        # The 20-bit immediate is signed: a negative one reaches code or data before the instruction.
        upper = operands[1].imm & 0xFFFFF
        if upper & 0x80000:
            upper -= 0x100000
        return operands[0].reg, instruction.address + (upper << 12)
    if mnemonic in ("lui", "c.lui") and kinds == [CS_OP_REG, CS_OP_IMM]:
        # The upper 20 bits, sign-extended from bit 31 on RV64.
        upper = operands[1].imm & 0xFFFFF
        if upper & 0x80000:
            upper -= 0x100000
        return operands[0].reg, upper << 12
    if mnemonic in {"addi", "addiw"} and kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM] and operands[1].reg in values:
        return operands[0].reg, values[operands[1].reg] + operands[2].imm
    if mnemonic in ("c.addi", "c.addiw") and kinds == [CS_OP_REG, CS_OP_IMM] and operands[0].reg in values:
        return operands[0].reg, values[operands[0].reg] + operands[1].imm
    if mnemonic in ("c.mv", "mv") and kinds == [CS_OP_REG, CS_OP_REG] and operands[1].reg in values:
        return operands[0].reg, values[operands[1].reg]
    if mnemonic in ("c.li", "li") and kinds == [CS_OP_REG, CS_OP_IMM]:
        return operands[0].reg, operands[1].imm
    if (
        mnemonic == "add"
        and kinds == [CS_OP_REG, CS_OP_REG, CS_OP_REG]
        and operands[1].reg in values
        and operands[2].reg in values
    ):
        return operands[0].reg, values[operands[1].reg] + values[operands[2].reg]
    if (
        mnemonic == "c.add"
        and kinds == [CS_OP_REG, CS_OP_REG]
        and operands[0].reg in values
        and operands[1].reg in values
    ):
        return operands[0].reg, values[operands[0].reg] + values[operands[1].reg]
    if mnemonic in ("slli", "slliw", "c.slli"):
        # The size of an array element, formed as a small number shifted left. Only a number is
        # shifted: an address shifted left is no address.
        source, amount = (
            (operands[1].reg, operands[2].imm)
            if kinds == [CS_OP_REG, CS_OP_REG, CS_OP_IMM]
            else (operands[0].reg, operands[1].imm)
            if kinds == [CS_OP_REG, CS_OP_IMM]
            else (None, 0)
        )
        if source in constants and source in values and 0 <= amount < 64:
            return operands[0].reg, values[source] << amount
    return None
