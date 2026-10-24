"""The configuration: the inputs, the parts, and the vocabulary of the C program.

The code knows the C language, the ELF format and RISC-V. Everything that belongs to one program
(its list and lock helpers, its per-CPU state, its message topics) comes from this file.
"""

from __future__ import annotations

import hashlib
import json
import re
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class ConfigError(ValueError):
    """A configuration that cannot be used. The message names the key."""


MEMBERSHIP_KINDS = ("hash", "hlist", "list", "llist", "rbtree")
SYMBOL_CLASSES = ("compiler_symbol", "constant_data", "per_cpu")


@dataclass(frozen=True)
class CallRules:
    lock: re.Pattern[str] | None = None
    unlock: re.Pattern[str] | None = None
    allocate: re.Pattern[str] | None = None
    free: re.Pattern[str] | None = None
    # A free whose callee matches this takes the cache first and the object second.
    free_object_after_first: re.Pattern[str] | None = None

    def action(self, callee: str) -> str | None:
        """The access kind a call to ``callee`` makes on its argument: unlock, lock, free or allocate."""
        for name, pattern in (
            ("unlock", self.unlock),
            ("lock", self.lock),
            ("free", self.free),
            ("allocate", self.allocate),
        ):
            if pattern is not None and pattern.search(callee):
                return name
        return None


@dataclass(frozen=True)
class Vocabulary:
    """What the C extraction needs to know about the program's own types and helpers."""

    kit_types: frozenset[str] = frozenset()
    value_types: frozenset[str] = frozenset()
    lock_types: frozenset[str] = frozenset()
    lock_type_words: tuple[str, ...] = ()
    percpu_globals: frozenset[str] = frozenset()
    percpu_structs: frozenset[str] = frozenset()
    calls: CallRules = field(default_factory=CallRules)
    membership: dict[str, str] = field(default_factory=dict)
    membership_witness_macros: tuple[str, ...] = ()
    cleanup_class: re.Pattern[str] | None = None
    cleanup_destructor: str = ""
    # Macros that put a cleanup attribute on a local, and the function each names: "{arg}" is the
    # macro's argument ("__free" = "__free_{arg}").
    cleanup_macros: dict[str, str] = field(default_factory=dict)
    base_type_min_containers: int = 2

    def digest(self) -> str:
        """A stable hash of everything that changes the facts of one translation unit."""
        payload = {
            "kit_types": sorted(self.kit_types),
            "value_types": sorted(self.value_types),
            "lock_types": sorted(self.lock_types),
            "lock_type_words": list(self.lock_type_words),
            "percpu_globals": sorted(self.percpu_globals),
            "calls": {
                name: getattr(self.calls, name).pattern if getattr(self.calls, name) else None
                for name in ("lock", "unlock", "allocate", "free", "free_object_after_first")
            },
            "membership": dict(sorted(self.membership.items())),
            "membership_witness_macros": list(self.membership_witness_macros),
            "cleanup_class": self.cleanup_class.pattern if self.cleanup_class else None,
            "cleanup_destructor": self.cleanup_destructor,
            "cleanup_macros": dict(sorted(self.cleanup_macros.items())),
        }
        return hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest()


@dataclass(frozen=True)
class TrapTable:
    """Where the image lists its trap instructions, and which of them resume: one entry per trap,
    ``entry_size`` bytes each, little-endian, holding the trap's address and a word of flags. The
    address is absolute, or with ``relative`` a signed offset from the address field itself. A trap
    whose flags share a bit with ``resume_flags`` resumes after it; any other the table lists stops
    there. The kernel's ``__bug_table`` lists each BUG and WARN, and BUGFLAG_WARNING marks a WARN."""

    section: str
    entry_size: int
    address_offset: int
    address_size: int
    relative: bool
    flags_offset: int
    flags_size: int
    resume_flags: int


@dataclass(frozen=True)
class ImageRules:
    thread_pointer: str | None = None
    percpu_sections: tuple[str, ...] = ()
    symbol_classes: dict[str, re.Pattern[str]] = field(default_factory=dict)
    # Image callee -> the C function it stands for.
    call_aliases: dict[str, str] = field(default_factory=dict)
    trap_table: TrapTable | None = None

    def symbol_class(self, name: str, section: str = "") -> str:
        """``per_cpu``, ``compiler_symbol``, ``constant_data`` or ``state`` for a data symbol."""
        if section and section in self.percpu_sections:
            return "per_cpu"
        for kind in SYMBOL_CLASSES:
            pattern = self.symbol_classes.get(kind)
            if pattern is not None and pattern.search(name):
                return kind
        return "state"


@dataclass(frozen=True)
class Inputs:
    c_tree: Path
    image: Path
    system_map: Path
    compile_commands: Path | None = None
    contract_graph: Path | None = None
    contract_source: Path | None = None


@dataclass(frozen=True)
class Thresholds:
    """The least agreement each layer check must reach for a run to pass, each a fraction in [0, 1]."""

    # C calls and image calls between image functions of the part: |both| / |either|.
    call_agreement: float = 0.0
    # Accesses to state globals, C against image: |both| / |either|.
    access_agreement: float = 0.0
    # Field accesses whose displacement the function's machine code holds.
    field_offsets: float = 0.0


def _thresholds(table: dict[str, Any], where: str) -> Thresholds:
    _unknown(table, {"min_call_agreement", "min_access_agreement", "min_field_offsets"}, where)
    values = {}
    for key, name in (
        ("min_call_agreement", "call_agreement"),
        ("min_access_agreement", "access_agreement"),
        ("min_field_offsets", "field_offsets"),
    ):
        value = table.get(key, 0.0)
        if not isinstance(value, int | float) or not 0 <= value <= 1:
            raise ConfigError(f"{where}.{key} must be a number from 0 to 1")
        values[name] = float(value)
    return Thresholds(**values)


@dataclass(frozen=True)
class Config:
    path: Path
    inputs: Inputs
    parts: dict[str, list[str] | str]
    # A part's own thresholds, over the configuration's [checks].
    part_thresholds: dict[str, Thresholds]
    flags: tuple[str, ...]
    # (pattern, replacement) for compile arguments the parser does not accept; "" drops the argument.
    argument_rewrites: tuple[tuple[re.Pattern[str], str], ...]
    vocabulary: Vocabulary
    image: ImageRules
    contract_function_prefix: str
    topic_prefix: str
    thresholds: Thresholds = field(default_factory=lambda: Thresholds())


def _expect(table: dict[str, Any], key: str, kind: type | tuple[type, ...], where: str, required: bool = False) -> Any:
    if key not in table:
        if required:
            raise ConfigError(f"{where}.{key} is required")
        return None
    value = table[key]
    if not isinstance(value, kind):
        raise ConfigError(f"{where}.{key} must be {_kind_name(kind)}")
    return value


def _kind_name(kind: type | tuple[type, ...]) -> str:
    names = {str: "a string", list: "a list", dict: "a table", int: "an integer", bool: "a boolean"}
    if isinstance(kind, tuple):
        return " or ".join(names.get(item, item.__name__) for item in kind)
    return names.get(kind, kind.__name__)


def _strings(table: dict[str, Any], key: str, where: str) -> list[str]:
    value = _expect(table, key, list, where) or []
    if not all(isinstance(item, str) for item in value):
        raise ConfigError(f"{where}.{key} must be a list of strings")
    return list(value)


def _regex(table: dict[str, Any], key: str, where: str) -> re.Pattern[str] | None:
    value = _expect(table, key, str, where)
    if value is None:
        return None
    try:
        return re.compile(value, re.IGNORECASE)
    except re.error as error:
        raise ConfigError(f"{where}.{key} is not a valid regular expression: {error}") from error


def _unknown(table: dict[str, Any], allowed: set[str], where: str) -> None:
    extra = sorted(set(table) - allowed)
    if extra:
        raise ConfigError(f"{where} has unknown key {extra[0]}")


def _trap_table(table: dict[str, Any]) -> TrapTable:
    where = "image.trap_table"
    _unknown(
        table,
        {"section", "entry_size", "address_offset", "address_size", "relative", "flags_offset", "flags_size"}
        | {"resume_flags"},
        where,
    )

    def number(key: str) -> int:
        value = _expect(table, key, int, where, required=True)
        if isinstance(value, bool) or value < 0:
            raise ConfigError(f"{where}.{key} must be an integer")
        return int(value)

    section = _expect(table, "section", str, where, required=True)
    if not section:
        raise ConfigError(f"{where}.section must name a section")
    relative = _expect(table, "relative", bool, where, required=True)
    trap = TrapTable(
        section=section,
        entry_size=number("entry_size"),
        address_offset=number("address_offset"),
        address_size=number("address_size"),
        relative=relative,
        flags_offset=number("flags_offset"),
        flags_size=number("flags_size"),
        resume_flags=number("resume_flags"),
    )
    if trap.address_size not in (4, 8):
        raise ConfigError(f"{where}.address_size must be 4 or 8")
    if trap.flags_size not in (1, 2, 4, 8):
        raise ConfigError(f"{where}.flags_size must be 1, 2, 4 or 8")
    if not trap.resume_flags:
        raise ConfigError(f"{where}.resume_flags must be a nonzero bit mask")
    for name, offset, size in (
        ("address", trap.address_offset, trap.address_size),
        ("flags", trap.flags_offset, trap.flags_size),
    ):
        if offset + size > trap.entry_size:
            raise ConfigError(f"{where}'s {name} field ends at byte {offset + size}, past entry_size {trap.entry_size}")
    return trap


def load(path: Path, root: Path | None = None) -> Config:
    """Read and check a configuration. ``root`` replaces ``[inputs] root``."""
    try:
        data = tomllib.loads(path.read_text())
    except FileNotFoundError as error:
        raise ConfigError(f"configuration {path} does not exist") from error
    except tomllib.TOMLDecodeError as error:
        raise ConfigError(f"configuration {path} is not valid TOML: {error}") from error
    _unknown(data, {"inputs", "parts", "c", "image", "contract_graph", "topics", "checks"}, "configuration")

    inputs_table = _expect(data, "inputs", dict, "configuration", required=True)
    _unknown(
        inputs_table,
        {"root", "c_tree", "image", "system_map", "compile_commands", "contract_graph", "contract_source"},
        "inputs",
    )
    base = path.resolve().parent
    configured_root = _expect(inputs_table, "root", str, "inputs")
    if root is not None:
        base = root.resolve()
    elif configured_root is not None:
        base = (base / configured_root).resolve()

    def input_path(key: str, required: bool) -> Path | None:
        value = _expect(inputs_table, key, str, "inputs", required=required)
        return None if value is None else (base / value)

    c_tree = input_path("c_tree", True)
    image = input_path("image", True)
    system_map = input_path("system_map", True)
    assert c_tree is not None and image is not None and system_map is not None
    inputs = Inputs(
        c_tree=c_tree,
        image=image,
        system_map=system_map,
        compile_commands=input_path("compile_commands", False),
        contract_graph=input_path("contract_graph", False),
        contract_source=input_path("contract_source", False),
    )

    parts_table = _expect(data, "parts", dict, "configuration", required=True)
    parts: dict[str, list[str] | str] = {}
    part_checks: dict[str, dict[str, Any]] = {}
    for name, part in sorted(parts_table.items()):
        where = f"parts.{name}"
        if not isinstance(part, dict):
            raise ConfigError(f"{where} must be a table")
        _unknown(part, {"files", "checks"}, where)
        part_checks[name] = _expect(part, "checks", dict, where) or {}
        files = _expect(part, "files", (list, str), where, required=True)
        if isinstance(files, str):
            if files != "all":
                raise ConfigError(f'{where}.files must be a list of C files or "all"')
            if inputs.compile_commands is None:
                raise ConfigError(f'{where}.files = "all" needs inputs.compile_commands')
            parts[name] = "all"
        else:
            if not files or not all(isinstance(item, str) and item.endswith(".c") for item in files):
                raise ConfigError(f"{where}.files must be a non-empty list of .c paths")
            parts[name] = sorted(set(files))

    c_table = _expect(data, "c", dict, "configuration") or {}
    _unknown(
        c_table,
        {
            "flags",
            "argument_rewrites",
            "kit_types",
            "value_types",
            "lock_types",
            "lock_type_words",
            "percpu_globals",
            "percpu_structs",
            "calls",
            "membership",
            "membership_witness_macros",
            "cleanup",
            "base_type_min_containers",
        },
        "c",
    )
    flags = tuple(_strings(c_table, "flags", "c"))
    if inputs.compile_commands is None and not flags:
        raise ConfigError("c.flags is required when inputs.compile_commands is not set")
    rewrites_table = _expect(c_table, "argument_rewrites", dict, "c") or {}
    rewrites: list[tuple[re.Pattern[str], str]] = []
    for pattern, replacement in sorted(rewrites_table.items()):
        if not isinstance(replacement, str):
            raise ConfigError(f"c.argument_rewrites.{pattern} must be a string")
        try:
            rewrites.append((re.compile(pattern), replacement))
        except re.error as error:
            raise ConfigError(
                f"c.argument_rewrites key {pattern} is not a valid regular expression: {error}"
            ) from error
    calls_table = _expect(c_table, "calls", dict, "c") or {}
    _unknown(calls_table, {"lock", "unlock", "allocate", "free", "free_object_after_first"}, "c.calls")
    calls = CallRules(
        lock=_regex(calls_table, "lock", "c.calls"),
        unlock=_regex(calls_table, "unlock", "c.calls"),
        allocate=_regex(calls_table, "allocate", "c.calls"),
        free=_regex(calls_table, "free", "c.calls"),
        free_object_after_first=_regex(calls_table, "free_object_after_first", "c.calls"),
    )
    membership_table = _expect(c_table, "membership", dict, "c") or {}
    _unknown(membership_table, set(MEMBERSHIP_KINDS), "c.membership")
    membership: dict[str, str] = {}
    for kind in MEMBERSHIP_KINDS:
        for call in _strings(membership_table, kind, "c.membership"):
            if call in membership:
                raise ConfigError(f"c.membership names {call} twice")
            membership[call] = kind
    cleanup_table = _expect(c_table, "cleanup", dict, "c") or {}
    _unknown(cleanup_table, {"class_type", "destructor", "attribute_macros"}, "c.cleanup")
    cleanup_macros = _expect(cleanup_table, "attribute_macros", dict, "c.cleanup") or {}
    for name, template in cleanup_macros.items():
        if not isinstance(template, str) or "{arg}" not in template:
            raise ConfigError(f"c.cleanup.attribute_macros.{name} must be a string containing {{arg}}")
    cleanup_class = _expect(cleanup_table, "class_type", str, "c.cleanup")
    cleanup_destructor = _expect(cleanup_table, "destructor", str, "c.cleanup") or ""
    if (cleanup_class is None) != (not cleanup_destructor):
        raise ConfigError("c.cleanup needs both class_type and destructor")
    if cleanup_destructor and "{name}" not in cleanup_destructor:
        raise ConfigError("c.cleanup.destructor must contain {name}")
    try:
        cleanup_pattern = re.compile(cleanup_class) if cleanup_class else None
    except re.error as error:
        raise ConfigError(f"c.cleanup.class_type is not a valid regular expression: {error}") from error
    if cleanup_pattern is not None and cleanup_pattern.groups != 1:
        raise ConfigError("c.cleanup.class_type must have exactly one group, the class name")
    minimum = _expect(c_table, "base_type_min_containers", int, "c")
    vocabulary = Vocabulary(
        kit_types=frozenset(_strings(c_table, "kit_types", "c")),
        value_types=frozenset(_strings(c_table, "value_types", "c")),
        lock_types=frozenset(_strings(c_table, "lock_types", "c")),
        lock_type_words=tuple(_strings(c_table, "lock_type_words", "c")),
        percpu_globals=frozenset(_strings(c_table, "percpu_globals", "c")),
        percpu_structs=frozenset(_strings(c_table, "percpu_structs", "c")),
        calls=calls,
        membership=membership,
        membership_witness_macros=tuple(_strings(c_table, "membership_witness_macros", "c")),
        cleanup_class=cleanup_pattern,
        cleanup_destructor=cleanup_destructor,
        cleanup_macros=dict(cleanup_macros),
        base_type_min_containers=2 if minimum is None else minimum,
    )
    if vocabulary.base_type_min_containers < 2:
        raise ConfigError("c.base_type_min_containers must be at least 2")

    image_table = _expect(data, "image", dict, "configuration") or {}
    _unknown(
        image_table,
        {"arch", "thread_pointer", "percpu_sections", "symbol_classes", "call_aliases", "trap_table"},
        "image",
    )
    aliases = _expect(image_table, "call_aliases", dict, "image") or {}
    if not all(isinstance(value, str) for value in aliases.values()):
        raise ConfigError("image.call_aliases must map names to names")
    arch = _expect(image_table, "arch", str, "image") or "riscv64"
    if arch != "riscv64":
        raise ConfigError("image.arch must be riscv64, the only architecture the image reader decodes")
    classes_table = _expect(image_table, "symbol_classes", dict, "image") or {}
    _unknown(classes_table, set(SYMBOL_CLASSES), "image.symbol_classes")
    symbol_classes: dict[str, re.Pattern[str]] = {}
    for kind in SYMBOL_CLASSES:
        pattern = _expect(classes_table, kind, str, "image.symbol_classes")
        if pattern is not None:
            try:
                symbol_classes[kind] = re.compile(pattern)
            except re.error as error:
                raise ConfigError(f"image.symbol_classes.{kind} is not a valid regular expression: {error}") from error
    trap_table = _expect(image_table, "trap_table", dict, "image")
    image_rules = ImageRules(
        thread_pointer=_expect(image_table, "thread_pointer", str, "image"),
        percpu_sections=tuple(_strings(image_table, "percpu_sections", "image")),
        symbol_classes=symbol_classes,
        call_aliases=dict(aliases),
        trap_table=None if trap_table is None else _trap_table(trap_table),
    )

    contract_table = _expect(data, "contract_graph", dict, "configuration") or {}
    _unknown(contract_table, {"function_prefix"}, "contract_graph")
    prefix = _expect(contract_table, "function_prefix", str, "contract_graph") or ""

    topics_table = _expect(data, "topics", dict, "configuration") or {}
    _unknown(topics_table, {"prefix"}, "topics")
    topic_prefix = _expect(topics_table, "prefix", str, "topics") or ""
    if topic_prefix and not topic_prefix.endswith("."):
        raise ConfigError("topics.prefix must end with a dot")
    defaults = _expect(data, "checks", dict, "configuration") or {}

    return Config(
        path=path,
        inputs=inputs,
        parts=parts,
        part_thresholds={
            name: _thresholds({**defaults, **table}, f"parts.{name}.checks") for name, table in part_checks.items()
        },
        flags=flags,
        argument_rewrites=tuple(rewrites),
        vocabulary=vocabulary,
        image=image_rules,
        contract_function_prefix=prefix,
        topic_prefix=topic_prefix,
        thresholds=_thresholds(defaults, "checks"),
    )


def part_files(config: Config, part: str, commands: dict[str, list[str]] | None) -> list[str]:
    """The C files of a part, relative to the C tree."""
    if part not in config.parts:
        raise ConfigError(f"no part {part}; the configuration has {', '.join(sorted(config.parts))}")
    files = config.parts[part]
    if files == "all":
        assert commands is not None
        return sorted(commands)
    assert isinstance(files, list)
    if commands is not None:
        missing = [file for file in files if file not in commands]
        if missing:
            raise ConfigError(f"parts.{part}.files names {missing[0]}, which inputs.compile_commands does not compile")
    return list(files)
