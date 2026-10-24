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
from typing import Any

from capstone import CS_ARCH_RISCV, CS_MODE_RISCV64, CS_MODE_RISCVC, CS_OP_IMM, CS_OP_MEM, CS_OP_REG, Cs
from elftools.common.exceptions import ELFError
from elftools.elf.constants import SH_FLAGS
from elftools.elf.elffile import ELFFile
from elftools.elf.sections import SymbolTableSection

from .config import ImageRules
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
# Instructions after which control does not continue. ebreak is not one: a WARN resumes after it.
_ENDS = {"ret", "jr", "c.jr", "unimp", "c.unimp", "mret", "sret"}
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
# Stores and loads of a whole register, which spill an address to a frame slot and reload it.
_SPILLS = frozenset({"sd", "c.sd", "c.sdsp"})
_RELOADS = frozenset({"ld", "c.ld", "c.ldsp"})
# The symbols that mark a change from code to data, which name no object of their own.
_MAPPING_SYMBOL = "$"
# The most addresses a register may hold, one from each path into a block, before it is unknown.
_MAX_CHOICES = 4


@dataclass
class _State:
    """Registers that hold a known value (``constants`` are those formed with ``lui``/``li``, which
    are numbers, not addresses); registers that hold an unknown base plus a known constant
    (``relative``); registers that hold a known address plus an unknown offset (``anchored``: a
    per-CPU offset or an index added to the address of an object), with the address each was formed
    from (``origin``); the frame slots holding an address the code spilled (``slots``, keyed by the
    frame register and its displacement); and registers every path into a block gives a known
    address (``choices``) or a known number (``numbers``), but not the same one: the value on each
    path."""

    values: dict[int, int] = field(default_factory=dict)
    relative: dict[int, int] = field(default_factory=dict)
    anchored: dict[int, int] = field(default_factory=dict)
    constants: set[int] = field(default_factory=set)
    origin: dict[int, int] = field(default_factory=dict)
    slots: dict[tuple[int, int], int] = field(default_factory=dict)
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
        def same(mine: dict[Any, int], theirs: dict[Any, int]) -> dict[Any, int]:
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


def _flow(decoder: Any, instructions: list[Any], never_returns: Any = None) -> list[list[int]]:
    """The successors of each instruction: the next one, a branch target, none after a return, an
    indirect jump, a jump to an address outside the function, or a call to a function that never
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
        elif mnemonic in _ENDS or (mnemonic in {"jalr", "c.jalr"} and not _links(decoder, operands)):
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


_RECORD_FIELDS = ("calls", "globals", "function_addresses", "outside", "exclusions")
_NUMBER_FIELDS = ("memory_offsets", "addi_constants")


def _facts_from_cache(value: dict[str, Any]) -> FunctionFacts | None:
    """The facts a cache entry holds, or None when it does not have their shape."""
    if set(value) != {*_RECORD_FIELDS, *_NUMBER_FIELDS}:
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
    calls, globals_, function_addresses, outside, exclusions = records
    memory_offsets, addi_constants = numbers
    return FunctionFacts(
        calls=calls,
        globals=globals_,
        function_addresses=function_addresses,
        outside=outside,
        memory_offsets=set(memory_offsets),
        addi_constants=set(addi_constants),
        exclusions=exclusions,
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
        self.image_cache = ImageCache(cache_directory, image, system_map, rules, Path(__file__))

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
        """The text functions ``_arguments`` may read the arity of: every direct jump target."""
        return sorted(
            {
                name
                for instruction in instructions
                if instruction.mnemonic in _DIRECT_CALLS
                for operands in [list(instruction.operands)]
                if operands and operands[-1].type == CS_OP_IMM
                for name in [self.text_at.get((instruction.address + operands[-1].imm) & _MASK)]
                if name is not None
            }
        )

    def decode(self, name: str, code: bytes, address: int) -> FunctionFacts:
        """The facts of a function's machine code that starts at ``address``."""
        return self._decode(name, list(self.decoder.disasm(code, address)))

    def _decode(self, name: str, instructions: list[Any]) -> FunctionFacts:
        facts = FunctionFacts()
        successors = _flow(self.decoder, instructions, self.never_returns)
        states = self._block_states(instructions, successors)
        before = dict(states)
        returned = {f"a{position}" for position in range(self.results.get(name, _DEFAULT_RESULTS))}
        for index, state in states:
            instruction = instructions[index]
            escapes = instruction.mnemonic not in _ADDI or self._escapes(
                instructions, successors, before, index, returned
            )
            self._step(name, instruction, state, facts, escapes)
        return facts

    def _arguments(self, instruction: Any, operands: list[Any]) -> set[str]:
        """The argument registers a call or a tail jump passes: as many as the callee's C takes."""
        count = _DEFAULT_ARGUMENTS
        if instruction.mnemonic in _DIRECT_CALLS and operands and operands[-1].type == CS_OP_IMM:
            callee = self.text_at.get((instruction.address + operands[-1].imm) & _MASK)
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
    ) -> bool:
        """Whether the address an ``addi`` sets is used, on some path, other than as the base of loads
        and stores whose state still holds it: passed to a call (in an argument the callee takes),
        returned (in a register the function's C result uses), stored, compared or computed with.
        Registers that hold the same address (``mv``) or the address plus an index (``add``) are
        followed; an ``addi`` of it forms another address, judged at that ``addi``. Callee-saved
        registers survive calls. Only an escaping address is itself an access; a base gives the
        accesses of its loads and stores."""
        operands = list(instructions[index].operands)
        if not operands or operands[0].type != CS_OP_REG:
            return True
        pending = [(following, frozenset({operands[0].reg})) for following in successors[index]]
        seen: set[tuple[int, frozenset[int]]] = set()
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
                held = state is not None and (
                    state.address(base) is not None or base in state.anchored or base in state.relative
                )
                if not held:
                    # The state at this load lost the address (a join), so only the addi shows it.
                    return True
            written = frozenset(_written_registers(instruction, current))
            used = reads & tracked
            if used and not (mnemonic in _DERIVES and written):
                return True
            names = {self.decoder.reg_name(register) for register in tracked}
            calls = mnemonic in {"jal", "c.jal"} or (mnemonic in {"jalr", "c.jalr"} and _links(self.decoder, current))
            tail = mnemonic in {"j", "c.j"} and not successors[position]
            indirect = mnemonic in {"jr", "c.jr"} and not (current and self.decoder.reg_name(current[0].reg) == "ra")
            if calls or tail or indirect:
                # An argument register the callee takes reaches it; across a call only callee-saved
                # registers keep their values.
                if names & self._arguments(instruction, current):
                    return True
                tracked = frozenset(
                    register for register in tracked if self.decoder.reg_name(register).startswith(("s", "sp", "gp"))
                )
            if mnemonic in _ENDS or tail:
                if names & returned and not indirect:
                    return True
                continue
            if used and mnemonic in _ADDI:
                # address + k is another address: its own addi decides whether it is an access.
                tracked = tracked - written
            else:
                # mv and add keep the address (plus an index); any other write ends it there.
                tracked = (tracked | written) if used else (tracked - written)
            if tracked:
                pending += [(following, tracked) for following in successors[position]]
        return bool(pending)

    def _block_states(self, instructions: list[Any], successors: list[list[int]]) -> list[tuple[int, _State]]:
        """Each instruction's index with the register state before it, from a forward must-analysis
        over the function's basic blocks: a register holds a value at a block's start only when every
        path into the block gives it that value, so an address formed before a branch and used after
        a join is followed, and one formed on only one path is not. Blocks no instruction leads to are
        reached through an indirect jump (a jump table): they start from the state those jumps leave."""
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
        self, function: str, address: int, instruction: int, operation: str, facts: FunctionFacts
    ) -> None:
        provenance = [{"source": "image", "address": _hex(instruction)}]
        section = self.section_of(address)
        zero_size = list(self.zero_size_at.get(address, []))
        ended = self.object_ends.get(address, [])
        if operation in _ADDRESS_OPERATIONS and address in self.merged_blocks:
            # The start of a merged-globals block is its first member's address, but forming it
            # accesses no member: each is reached by a displacement from it, recorded where it is.
            facts.exclusions.append(
                {
                    "kind": "image_access",
                    "reason": "merged_globals_block_base",
                    "function": function,
                    "target": _hex(address),
                    "detail": f"the base of a merged-globals block of {self.merged_blocks[address]} bytes",
                    "provenance": provenance,
                }
            )
            return
        resolved = resolve_sized(self.objects, address) if section is not None else None
        target = resolve_sized(self.function_ranges, address) if resolved is None and section else None
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
                # A code marker at a function's start (__entry_text_end, _sinittext) names the
                # function's first instruction, which the entry above already records.
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
            # shares the address of the object after it; the end of one object is the start of the
            # next, and the C may have formed either.
            entry["aliases"] = aliases
        facts.globals.append(entry)

    def _step(
        self, function: str, instruction: Any, state: _State, facts: FunctionFacts | None, escapes: bool = True
    ) -> None:
        """One instruction: its facts (when ``facts`` is given) and its effect on the register state.

        ``escapes`` says whether an ``addi``'s address is used other than as a load or store base."""
        values, relative, anchored = state.values, state.relative, state.anchored
        operands = list(instruction.operands)
        mnemonic = instruction.mnemonic
        computed = _computed_value(instruction, operands, values, state.constants)
        if facts is not None:
            if mnemonic in _ADDI and operands and operands[-1].type == CS_OP_IMM:
                facts.addi_constants.add(operands[-1].imm)
                source = operands[-2].reg if len(operands) == 3 and operands[-2].type == CS_OP_REG else operands[0].reg
                if source in relative:
                    facts.addi_constants.add(relative[source] + operands[-1].imm)
            _, reads = _reads(instruction, operands)
            if self.rules.thread_pointer and any(self.decoder.reg_name(register) == "tp" for register in reads):
                # The thread pointer used as a value (passed, stored, offset): the current task itself.
                facts.globals.append(self._thread_pointer(function, instruction.address, 0, "thread_pointer_value"))
            for base, displacement in _memory_references(instruction, operands):
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
                    self._record_address(
                        function, (anchored[base] + displacement) & _MASK, instruction.address, "indexed_access", facts
                    )
                    facts.memory_offsets.add(anchored[base] - state.origin.get(base, anchored[base]) + displacement)
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
                elif base in relative:
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
                self._record_address(function, value, instruction.address, "addi_address", facts)
        elif indexed is not None:
            anchored[indexed[0]] = indexed[1]
            state.origin[indexed[0]] = indexed[2]
            if facts is not None and escapes and (mnemonic in _ADDI or mnemonic in ("add", "c.add")):
                # An index added to a known address, or a per-CPU or indexed address plus a field
                # offset: the object the sum names, whatever the index is.
                self._record_address(function, indexed[1], instruction.address, "indexed_address", facts)
        elif derived is not None:
            relative[derived[0]] = derived[1]
            if facts is not None:
                # The code formed "a base plus a constant": the offset of the field it addresses,
                # which an index added after it would lose.
                facts.memory_offsets.add(derived[1])
        self._frame_slot(instruction, operands, state)

    def _frame_slot(self, instruction: Any, operands: list[Any], state: _State) -> None:
        """A register holding an address, spilled to a frame slot and reloaded from it. Without
        this the address is lost at the first join after the reload, although every path gives the
        slot the same value."""
        mnemonic = instruction.mnemonic
        if mnemonic not in _SPILLS and mnemonic not in _RELOADS:
            return
        references = _memory_references(instruction, operands)
        if not references or not operands or operands[0].type != CS_OP_REG:
            return
        base, displacement = references[0]
        if self.decoder.reg_name(base) not in _FRAME_REGISTERS:
            return
        register = operands[0].reg
        if mnemonic in _SPILLS:
            address = state.address(register)
            if address is None:
                state.slots.pop((base, displacement), None)
            else:
                state.slots[(base, displacement)] = address
        elif (base, displacement) in state.slots:
            state.values[register] = state.slots[(base, displacement)]
            state.constants.discard(register)

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
