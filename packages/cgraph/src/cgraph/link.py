"""From the facts of each translation unit, the image and the contract graph, to one graph.

The link is a pure function of its inputs: the facts of each unit (whether parsed now or read from
the cache), the image and the contract graph. So a rebuild that reparses only changed units gives
the same graph as a full build.
"""

from __future__ import annotations

import bisect
import collections
import dataclasses
import functools
import hashlib
import importlib.metadata
import json
import os
import re
import sys
import tempfile
from collections.abc import Callable, Collection
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import capstone

from . import expressions, jsonio, structure
from .config import Config
from .contracts import ContractGraph
from .contracts import address as contract_address
from .errors import CacheError
from .image import FunctionFacts, Image, Symbol

Facts = dict[str, Any]
Unit = dict[str, Any]
_EVENT_LISTS = ("accesses", "unevaluated", "list_calls", "indirect", "function_pointers", "field_accesses")
_FIELD_KEYS = ("struct", "field", "offset", "displacement", "base", "size", "bitfield")
# What a call argument says of the address it passes, which a value standing for it does not keep.
_ADDRESS_KEYS = frozenset({"displacement", "base", "object", "object_size", "element", "index", "inner"})
_CORE_FORMAT = "cgraph/link-core/v1"
# The events a fold under a __builtin_constant_p choice marks ``conditional``, besides calls.
_MARKED = ("accesses", "field_accesses")
# A guard's outcome at a fold where its arguments decide against the event (``_decision``).
_AGAINST: Any = object()
# Image items naming every C name at an address the code forms, where the image cannot tell which
# the C meant: the start of a merged-globals block, a zero-size marker at a function's start.
_ADDRESS_NAMES = ("merged_globals_block_base", "code_marker_at_function_start")
# Bounds on a struct's places inside a global: elements of one array, nesting depth, places kept.
_MAX_ELEMENTS = 64
_MAX_DEPTH = 8
_MAX_PLACES = 4096


@functools.cache
def _code_digest() -> str:
    """Every source file of the package, and the interpreter and libraries that decode the image."""
    hashed = hashlib.sha256(
        jsonio.canonical([sys.version, capstone.__version__, importlib.metadata.version("pyelftools")]).encode()
    )
    for path in sorted(Path(__file__).parent.glob("*.py")):
        hashed.update(path.name.encode())
        hashed.update(path.read_bytes())
    return hashed.hexdigest()


def _plain(value: Any) -> Any:
    """A configuration value as JSON: patterns by their text, sets sorted, dataclasses by field."""
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {item.name: _plain(getattr(value, item.name)) for item in dataclasses.fields(value)}
    if isinstance(value, re.Pattern):
        return value.pattern
    if isinstance(value, frozenset | set):
        return sorted(_plain(item) for item in value)
    if isinstance(value, list | tuple):
        return [_plain(item) for item in value]
    if isinstance(value, dict):
        return {str(key): _plain(item) for key, item in value.items()}
    return value


@dataclass(frozen=True)
class LinkCache:
    """The latest link core of each part, at ``<cache>/link/<part>/<key>.json``: a part's directory is
    named by the SHA-256 of its files, and storing a core removes that part's earlier ones, so the
    cache holds one core per part. ``inputs`` names the unit facts (by their facts-cache keys) and
    the image and System.map (by SHA-256); the key adds the code, the configuration's rules, the
    part's files and the image copy the contract graph pins."""

    directory: Path
    inputs: dict[str, Any]

    def key(self, config: Config, part_files: list[str], pins: list[list[Any]]) -> str:
        payload = {
            "format": _CORE_FORMAT,
            "code": _code_digest(),
            "vocabulary": _plain(config.vocabulary),
            "image_rules": _plain(config.image),
            "part_files": sorted(part_files),
            "inputs": self.inputs,
            "pins": pins,
        }
        return hashlib.sha256(jsonio.canonical(payload).encode()).hexdigest()

    def _path(self, part_files: list[str], key: str) -> Path:
        part = hashlib.sha256(jsonio.canonical(sorted(part_files)).encode()).hexdigest()
        return self.directory / "link" / part / f"{key}.json"

    def load(self, part_files: list[str], key: str) -> dict[str, Any] | None:
        path = self._path(part_files, key)
        try:
            text = path.read_text()
        except FileNotFoundError:
            return None
        except OSError as error:
            raise self._error(path) from error
        try:
            core = json.loads(text)
        except json.JSONDecodeError as error:
            raise self._error(path) from error
        if (
            not isinstance(core, dict)
            or core.get("format") != _CORE_FORMAT
            or not all(isinstance(core.get(name), list) for name in ("nodes", "edges", "exclusions"))
            or not isinstance(core.get("analysis"), dict)
            or not isinstance(core["analysis"].get("part_functions"), list)
            or not isinstance(core.get("image_facts"), str)
        ):
            raise self._error(path)
        return core

    def store(self, part_files: list[str], key: str, core: dict[str, Any]) -> None:
        path = self._path(part_files, key)
        temporary: Path | None = None
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_name = tempfile.mkstemp(prefix=f".{key}.", suffix=".tmp", dir=path.parent)
            temporary = Path(temporary_name)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(core, stream, separators=(",", ":"), ensure_ascii=False)
            os.replace(temporary, path)
            for earlier in path.parent.glob("*.json"):
                if earlier != path:
                    earlier.unlink(missing_ok=True)
        except OSError as error:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
            raise CacheError(f"cannot write link core {path}: {error}") from error

    def _error(self, path: Path) -> CacheError:
        return CacheError(f"link core {path} is malformed; remove the cache directory {self.directory}")


def _pins(linker: Linker) -> list[list[Any]]:
    """Each definition whose image copy the contract graph's address selects, with that address."""
    pins = []
    for (name, file, line), _ in sorted(linker.definitions.items()):
        contract = linker.contracts.function(file, name)
        address = contract.get("addr") if contract else None
        if address and any(symbol.address == int(address, 16) for symbol in linker.image.functions.get(name, [])):
            pins.append([name, file, line, int(address, 16)])
    return pins


def _image_digest(linker: Linker, identities: list[str]) -> str:
    """The SHA-256 of the image facts of each function the core decoded, read through the image
    cache, so a reused core is checked against the facts it was built from, and a malformed image
    cache entry stops the run as it does without a core."""
    hashed = hashlib.sha256()
    for identity in identities:
        for symbol in linker.symbols(linker.primary[identity]):
            facts = _plain(linker.image.facts(symbol))
            hashed.update(jsonio.canonical([identity, symbol.address, facts]).encode())
    return hashed.hexdigest()


@dataclass
class Definition:
    """One C function definition: a name at one file and line."""

    name: str
    file: str
    line: int
    static: bool
    tu: str  # the first unit, in path order, that defines it
    id: str = ""
    weak: bool = False


def function_id(name: str, file: str, static: bool) -> str:
    """``function:<name>`` for external linkage, ``function:<name>@<file>`` for a static function.

    Linkage decides, never a collision with another name, so a definition that appears later does
    not rename this one."""
    return f"function:{name}@{file}" if static else f"function:{name}"


@dataclass
class Effective:
    """The events of an image function once the helpers the image inlines are folded into it."""

    calls: list[dict[str, Any]] = field(default_factory=list)
    accesses: list[dict[str, Any]] = field(default_factory=list)
    unevaluated: list[dict[str, Any]] = field(default_factory=list)
    list_calls: list[dict[str, Any]] = field(default_factory=list)
    indirect: list[dict[str, Any]] = field(default_factory=list)
    function_pointers: list[dict[str, Any]] = field(default_factory=list)
    field_accesses: list[dict[str, Any]] = field(default_factory=list)
    folds: list[dict[str, Any]] = field(default_factory=list)
    unbound: list[dict[str, Any]] = field(default_factory=list)
    # Per function the code passes to a folded helper, the arguments that helper calls it with
    # (kref_put's release(kref)), in this code's terms.
    pointer_calls: dict[str, list[list[dict[str, Any]]]] = field(default_factory=dict)


def _with_via(item: dict[str, Any], helper: str) -> dict[str, Any]:
    copied = dict(item)
    copied["provenance"] = [{**entry, "via": [helper, *entry.get("via", [])]} for entry in item["provenance"]]
    return copied


class Linker:
    def __init__(
        self, config: Config, part_files: list[str], tus: dict[str, Facts], image: Image, contracts: ContractGraph
    ) -> None:
        self.config = config
        self.vocabulary = config.vocabulary
        self.part_files = set(part_files)
        self.tus = dict(sorted(tus.items()))
        self.image = image
        self.contracts = contracts
        self.definitions: dict[tuple[str, str, int], Definition] = {}
        self.by_name: dict[str, list[Definition]] = collections.defaultdict(list)
        defining_units: dict[tuple[str, str, int], list[str]] = collections.defaultdict(list)
        for tu, facts in self.tus.items():
            for name, function in facts["functions"].items():
                key = (name, function["file"], function["line"])
                defining_units[key].append(tu)
                if key not in self.definitions:
                    definition = Definition(
                        name, function["file"], function["line"], function["static"], tu,
                        function_id(name, function["file"], function["static"]), bool(function.get("weak")),
                    )  # fmt: skip
                    self.definitions[key] = definition
                    self.by_name[name].append(definition)
        # External definitions of one name: the linker keeps the strong one, which is the node; the
        # weak defaults it replaced are recorded on it.
        self.primary: dict[str, Definition] = {}
        self.alternates: dict[str, list[Definition]] = collections.defaultdict(list)
        for definition in sorted(
            self.definitions.values(), key=lambda item: (item.id, item.weak, item.file, item.line)
        ):
            if definition.id in self.primary:
                self.alternates[definition.id].append(definition)
            else:
                self.primary[definition.id] = definition
        self.records: dict[str, dict[str, Any]] = {}
        # Each file-scope global's initializer: the addresses of other globals it stores. Facts
        # before U2's ``global_initializers`` have none.
        self.initializers: dict[tuple[str, str | None], dict[str, dict[str, Any]]] = collections.defaultdict(dict)
        for facts in self.tus.values():
            for key, record in facts["records"].items():
                self.records.setdefault(key, record)
            for item in facts.get("global_initializers", []):
                holder = (item["global"]["name"], item["global"].get("def"))
                self.initializers[holder][jsonio.canonical(item)] = item
        self.records_by_name: dict[str, list[str]] = collections.defaultdict(list)
        for record_key, record in self.records.items():
            self.records_by_name[record["name"]].append(record_key)
        self.place_cache: dict[tuple[str, str], dict[str, list[int]]] = {}
        self.relative_place_cache: dict[str, dict[str, set[int]]] = {}
        header_records: dict[str, set[str]] = collections.defaultdict(set)
        for record in self.records.values():
            file = record["def"].rsplit(":", 1)[0]
            if not file.endswith(".c"):
                header_records[record["name"]].add(file)
        # A struct defined in a C file is that file's: struct:<name>@<file>. A header's is
        # struct:<name>, unless two headers define the name.
        self.struct_ids: dict[str, str] = {}
        for record in self.records.values():
            name, file = record["name"], record["def"].rsplit(":", 1)[0]
            local = file.endswith(".c") or len(header_records[name]) > 1
            self.struct_ids[f"{name}@{record['def']}"] = f"struct:{name}@{file}" if local else f"struct:{name}"
        self.global_ids: dict[str, set[str]] = collections.defaultdict(set)
        for facts in self.tus.values():
            for function in facts["functions"].values():
                for item in function["events"]["accesses"] + function["events"]["unevaluated"]:
                    unit = item.get("unit")
                    if unit is not None and unit["kind"] == "global":
                        identity = self.unit_id(unit)
                        if identity is not None:
                            self.global_ids[unit["name"]].add(identity)
        self.symbol_cache: dict[tuple[str, str, int], list[Symbol]] = {}
        self.callee_cache: dict[tuple[str, str, int], set[str]] = {}
        self.effective_cache: dict[tuple[str, str, tuple[str, str, int], bool], Effective] = {}
        self.own_cache: dict[tuple[str, str, int], tuple[frozenset[str], frozenset[str]]] = {}
        # A header's static function is compiled into each unit that includes it, with that unit's
        # view of its callees. Its facts are those of the unit whose image copy the node is.
        for key, definition in self.definitions.items():
            if definition.static and not definition.file.endswith(".c") and len(defining_units[key]) > 1:
                copies = {symbol.file for symbol in self.symbols(definition)}
                compiled = [tu for tu in defining_units[key] if os.path.basename(tu) in copies]
                if compiled:
                    definition.tu = compiled[0]

    # Identities ----------------------------------------------------------------------------

    def visible(self, tu: str, name: str) -> Definition | None:
        function = self.tus[tu]["functions"].get(name)
        if function is None:
            return None
        return self.definitions[(name, function["file"], function["line"])]

    def initializer_values(self, unit: Unit, field: str | None) -> list[dict[str, Any]]:
        """The global addresses ``unit``'s initializer stores in ``field`` (in any field when the
        access names the whole global, or a field the initializer item does not name), in C4 order."""
        items = self.initializers.get((unit["name"], unit.get("def")), {})
        return [
            items[key]
            for key in sorted(items)
            if field is None or items[key].get("field") is None or items[key]["field"] == field
        ]

    def struct_places(self, name: str, record: str) -> dict[str, list[int]]:
        """Each struct's offsets inside global ``name`` of struct type ``record``: the record itself, every
        record it embeds at any depth, and each element of an embedded array or of the global's own
        array (its count from the image symbol's size), from clang's layouts."""
        key = (name, record)
        if key in self.place_cache:
            return self.place_cache[key]
        places: dict[str, set[int]] = collections.defaultdict(set)
        symbols = self.image.object_symbols.get(name, [])
        for record_key in sorted(self.records_by_name.get(record, [])):
            size = self.records[record_key].get("size", -1)
            count = symbols[0].size // size if symbols and size > 0 and symbols[0].size % size == 0 else 1
            for element in range(max(1, min(count, _MAX_ELEMENTS))):
                for struct, offsets in self._relative_places(record_key, 0).items():
                    places[struct] |= {element * max(size, 0) + offset for offset in offsets}
        result = {struct: sorted(offsets)[:_MAX_PLACES] for struct, offsets in places.items()}
        self.place_cache[key] = result
        return result

    def _relative_places(self, record_key: str, depth: int) -> dict[str, set[int]]:
        cached = self.relative_place_cache.get(record_key)
        if cached is not None:
            return cached
        record = self.records[record_key]
        places: dict[str, set[int]] = collections.defaultdict(set)
        places[record["name"]].add(0)
        if depth < _MAX_DEPTH:
            for field in record["fields"]:
                child_key = f"{field.get('record')}@{field.get('record_def')}"
                child = self.records.get(child_key)
                if field.get("pointer") or child is None or child.get("size", -1) <= 0 or field["offset"] < 0:
                    continue
                size = child["size"]
                count = field["size"] // size if field.get("size", -1) >= size and field["size"] % size == 0 else 1
                nested = self._relative_places(child_key, depth + 1)
                for element in range(min(count, _MAX_ELEMENTS)):
                    base = field["offset"] + element * size
                    for struct, offsets in nested.items():
                        places[struct] |= {base + offset for offset in offsets}
        self.relative_place_cache[record_key] = places
        return places

    def node_of(self, definition: Definition) -> Definition:
        """The definition that is the node of ``definition``'s id: the strong one of a weak pair."""
        return self.primary[definition.id]

    def resolve(self, tu: str, name: str) -> str:
        """The id a call to ``name`` from unit ``tu`` reaches."""
        definition = self.visible(tu, name)
        return definition.id if definition is not None else f"function:{name}"

    def is_percpu(self, name: str) -> bool:
        if name in self.vocabulary.percpu_globals:
            return True
        return any(
            symbol.section in self.config.image.percpu_sections for symbol in self.image.object_symbols.get(name, [])
        )

    def unit_id(self, unit: Unit) -> str | None:
        """``global:<name>`` for external linkage, ``global:<name>@<file>`` for a static global or a
        function-local static; ``percpu:`` the same for per-CPU state."""
        if unit["kind"] == "struct":
            return self.struct_ids.get(f"{unit['name']}@{unit['def']}")
        name = unit["name"]
        prefix = "percpu" if self.is_percpu(name) else "global"
        definition = unit.get("def")
        return f"{prefix}:{name}" if definition is None else f"{prefix}:{name}@{definition.rsplit(':', 1)[0]}"

    def image_global_id(self, name: str, symbol_file: str) -> str:
        """The node a global the image addresses is: the C global of that name, the static one
        whose file the ELF FILE symbol names when there are several, or a new external one."""
        ids = sorted(self.global_ids.get(name, set()))
        if len(ids) == 1:
            return ids[0]
        matching = [identity for identity in ids if os.path.basename(identity.rsplit("@", 1)[-1]) == symbol_file]
        if len(matching) == 1:
            return matching[0]
        prefix = "percpu" if self.is_percpu(name) else "global"
        return f"{prefix}:{name}"

    # The image -----------------------------------------------------------------------------

    def symbols(self, definition: Definition) -> list[Symbol]:
        """The image copies of a definition, by linkage: the global symbol of an external function;
        the local symbol that follows its unit's ELF FILE symbol for a C file's static; every unit's
        local copy of a header's static inline that no C file's static of the name claims."""
        key = (definition.name, definition.file, definition.line)
        if key in self.symbol_cache:
            return self.symbol_cache[key]
        candidates = self.image.functions.get(definition.name, [])
        contract = self.contracts.function(definition.file, definition.name)
        address = contract.get("addr") if contract else None
        pinned = [symbol for symbol in candidates if address and symbol.address == int(address, 16)]
        if pinned:
            candidates = pinned
        elif not definition.static:
            candidates = [symbol for symbol in candidates if not symbol.local]
        elif definition.file.endswith(".c"):
            names = {os.path.basename(definition.tu), os.path.basename(definition.file)}
            candidates = [symbol for symbol in candidates if symbol.local and symbol.file in names]
        else:
            claimed = {
                os.path.basename(item.tu)
                for item in self.by_name[definition.name]
                if item.static and item.file.endswith(".c")
            }
            candidates = [symbol for symbol in candidates if symbol.local and symbol.file not in claimed]
        self.symbol_cache[key] = candidates
        return candidates

    def image_facts(self, definition: Definition) -> list[FunctionFacts]:
        return [self.image.facts(symbol) for symbol in self.symbols(definition)]

    def image_callees(self, definition: Definition) -> set[str]:
        key = (definition.name, definition.file, definition.line)
        if key not in self.callee_cache:
            self.callee_cache[key] = {call["target"] for facts in self.image_facts(definition) for call in facts.calls}
        return self.callee_cache[key]

    def image_globals(self, definition: Definition) -> set[str]:
        """Every name of every global the definition's machine code addresses."""
        return {
            name
            for facts in self.image_facts(definition)
            for entry in facts.globals
            for name in (entry["global"], *entry.get("aliases", []))
        }

    def image_address_targets(self, definition: Definition) -> set[str]:
        """The functions whose address the definition's machine code forms."""
        return {item["target"] for facts in self.image_facts(definition) for item in facts.function_addresses}

    # Folding -------------------------------------------------------------------------------

    def effective(
        self, tu: str, name: str, caller: Definition, stack: tuple[str, ...] = (), pointers: bool = True
    ) -> Effective:
        """The events of ``name`` (as unit ``tu`` defines it) inside the image code of ``caller``.

        A visible helper is folded where the caller's image code has no direct call to it: the
        compiler inlined it there. A helper the image calls stays a call. With ``pointers``, a
        function whose address the C passes is folded too where the image shows it (below).
        """
        caller_key = (caller.name, caller.file, caller.line)
        key = (tu, name, caller_key, pointers)
        if key in self.effective_cache:
            return self.effective_cache[key]
        function = self.tus[tu]["functions"][name]
        events, table = function["events"], function.get("guards", [])
        result = Effective()
        for kind in _EVENT_LISTS:
            setattr(result, kind, _resolved(events[kind], table))
        callees = self.image_callees(caller)
        for call in _resolved(events["calls"], table):
            callee = call["callee"]
            if (
                call.get("visible")
                and not call.get("generic")
                and callee not in callees
                and callee not in stack
                and callee != name
            ):
                helper = self.visible(tu, callee)
                assert helper is not None
                child = self.effective(tu, callee, caller, (*stack, name), pointers)
                bound = _bind(child, call["arguments"])
                returned = self.tus[tu]["functions"][callee].get("returns_object")
                if returned is not None and call.get("result_object") is not None:
                    # class_task_rq_lock_constructor returns its local _t, which is the caller's guard.
                    bound = _renamed(bound, returned, call["result_object"])
                # A helper folded under a __builtin_constant_p choice is itself left to the image: each
                # of its events is marked. Each is also under the guards of the call.
                mark = {"conditional": True} if call.get("conditional") else {}
                guards = call.get("guards", [])
                result.calls += [{**_under(_with_via(item, helper.id), guards), **mark} for item in bound.calls]
                for kind in _EVENT_LISTS:
                    getattr(result, kind).extend(
                        {**_under(_with_via(item, helper.id), guards), **(mark if kind in _MARKED else {})}
                        for item in getattr(bound, kind)
                    )
                result.folds.append({"helper": helper.id, "provenance": call["provenance"]})
                result.folds += bound.folds
                result.unbound += [_under(item, guards) for item in bound.unbound]
                self.pointer_calls(tu, callee, call, result)
            else:
                result.calls.append({**call, "tu": tu})
                if call.get("visible") and callee in callees and callee not in stack and callee != name:
                    # The machine code calls the helper here but may inline it at another site:
                    # its calls and accesses to globals that the caller's machine code makes and
                    # the caller's C does not are the inlined copy's.
                    own = {item["callee"] for item in events["calls"]}
                    own_globals = {
                        item["unit"]["name"]
                        for item in events["accesses"]
                        if item.get("unit") is not None and item["unit"]["kind"] == "global"
                    }
                    addressed = self.image_globals(caller)
                    helper = self.visible(tu, callee)
                    assert helper is not None
                    child = self.effective(tu, callee, caller, (*stack, name), pointers)
                    bound = _bind(child, call["arguments"])
                    guards = call.get("guards", [])
                    for item in bound.calls:
                        if item["callee"] in callees and item["callee"] not in own and item["callee"] != callee:
                            result.calls.append({**_under(_with_via(item, helper.id), guards), "partial_inline": True})
                    for item in bound.accesses:
                        unit = item.get("unit")
                        if (
                            unit is not None
                            and unit["kind"] == "global"
                            and unit["name"] in addressed
                            and unit["name"] not in own_globals
                        ):
                            result.accesses.append(
                                {**_under(_with_via(item, helper.id), guards), "partial_inline": True}
                            )
        if not pointers:
            self.effective_cache[key] = result
            return result
        # A function whose address the C passes, which the machine code neither calls nor forms:
        # the compiler inlined it where the callee that takes the pointer was inlined. The image must
        # show it: a call or a global of the target that the caller's own C (every helper folded,
        # no function through a pointer) does not explain.
        formed = self.image_address_targets(caller)
        kept = []
        for pointer in result.function_pointers:
            target = pointer["target"]
            helper = self.visible(tu, target)
            if helper is None or target in stack or target == name or not self.symbols(caller):
                kept.append(pointer)
                continue
            caller_calls, caller_globals = self.own_events(caller)
            addressed = self.image_globals(caller)
            child = self.effective(tu, target, caller, (*stack, name))
            calls_shown = [
                item for item in child.calls if item["callee"] in callees and item["callee"] not in caller_calls
            ]
            globals_shown = [
                item
                for item in child.accesses
                if item.get("unit") is not None
                and item["unit"]["kind"] == "global"
                and item["unit"]["name"] in addressed
                and item["unit"]["name"] not in caller_globals
            ]
            if target in callees or target in formed or not (calls_shown or globals_shown):
                # The image calls the target or passes its address on, or shows nothing of it. Where it
                # also inlined a copy (an action a helper calls through its parameter on an error
                # path), the calls and globals that only that copy explains are the copy's.
                guards = pointer.get("guards", [])
                result.calls += [
                    {**_reframed(_with_via(item, helper.id), guards), "partial_inline": True} for item in calls_shown
                ]
                result.accesses += [
                    {**_reframed(_with_via(item, helper.id), guards), "partial_inline": True} for item in globals_shown
                ]
                kept.append(pointer)
                continue
            guards = pointer.get("guards", [])
            passed = {jsonio.canonical(arguments): arguments for arguments in result.pointer_calls.get(target, [])}
            if len(passed) == 1:
                # A folded helper calls it through its parameter (kref_put's release(kref)): its
                # parameters are those arguments, so its fields are rebased and its guards decided.
                bound = _bind(child, next(iter(passed.values())))
                result.calls += [_under(_with_via(item, helper.id), guards) for item in bound.calls]
                for kind in _EVENT_LISTS:
                    if kind != "function_pointers":
                        getattr(result, kind).extend(
                            _under(_with_via(item, helper.id), guards) for item in getattr(bound, kind)
                        )
                kept += [_under(_with_via(item, helper.id), guards) for item in bound.function_pointers]
                result.unbound += [_under(item, guards) for item in bound.unbound]
            else:
                # Its own guards are on its parameters, which no argument of this code binds: its
                # events are under the guards of the pointer's use only.
                result.calls += [_reframed(_with_via(item, helper.id), guards) for item in child.calls]
                for kind in _EVENT_LISTS:
                    if kind != "function_pointers":
                        getattr(result, kind).extend(
                            _reframed(_with_via(item, helper.id), guards) for item in getattr(child, kind)
                        )
                kept += [_reframed(_with_via(item, helper.id), guards) for item in child.function_pointers]
                result.unbound += [_reframed(item, guards) for item in child.unbound]
            result.folds.append({"helper": helper.id, "provenance": pointer["provenance"], "through": "pointer"})
            result.folds += child.folds
        result.function_pointers = kept
        self.effective_cache[key] = result
        return result

    def pointer_calls(self, tu: str, helper: str, call: dict[str, Any], result: Effective) -> None:
        """The calls a folded helper makes through a parameter its caller passes a function in: that
        function's arguments in the caller's terms, kept per function in ``result.pointer_calls``."""
        record = self.tus[tu]["functions"][helper]
        through = _resolved(record["events"].get("parameter_calls", []), record.get("guards", []))
        for item in through:
            passed = call["arguments"][item["param"]] if item["param"] < len(call["arguments"]) else {}
            target = passed.get("function")
            if target is None:
                continue
            restated = {"callee": target, "arguments": item["arguments"], "provenance": item["provenance"]}
            if item.get("guards"):
                restated["guards"] = item["guards"]
            for bound in _bind(Effective(calls=[restated]), call["arguments"]).calls:
                result.pointer_calls.setdefault(target, []).append(bound["arguments"])

    def own_events(self, caller: Definition) -> tuple[frozenset[str], frozenset[str]]:
        """The callees and the globals of the caller's own C: every helper the image inlines folded,
        no function whose address the C passes."""
        key = (caller.name, caller.file, caller.line)
        if key not in self.own_cache:
            result = self.effective(caller.tu, caller.name, caller, (), pointers=False)
            self.own_cache[key] = (
                frozenset(item["callee"] for item in result.calls),
                frozenset(
                    item["unit"]["name"]
                    for item in result.accesses
                    if item.get("unit") is not None and item["unit"]["kind"] == "global"
                ),
            )
        return self.own_cache[key]


def _rebased_base(outer: str | None, inner: str) -> str:
    """The base of an address a folded callee forms from its parameter, given what the caller passed:
    the caller's local object stays that object; the caller's global or container_of pointer gives the
    callee's known address that base; a plain pointer leaves the callee's own base (its container_of,
    the pointer, or unknown)."""
    if outer in ("local", "other"):
        return outer
    if outer in ("global", "container_of") and inner != "unknown":
        return outer
    return inner


def _rebase(access: dict[str, Any], argument: dict[str, Any] | None) -> dict[str, Any]:
    """A field access a helper makes through its parameter, as its caller's code makes it.

    Only an argument whose ``displacement`` the facts give (a member's address reached from a pointer,
    ``f(&p->m)``, 0 for the pointer itself, or the address of the caller's local or global object) is
    that far from the caller's base: the displacement is added, the caller's parameter kept, and the
    caller's base taken. A pointer loaded through the parameter (``f(p->q)``) is another object, so
    the access keeps its own displacement and names no parameter."""
    if argument is None:
        return {**access, "param": None}
    if "displacement" in argument:
        # A container_of in the callee subtracts from its parameter: a negative displacement is known.
        known = access["base"] != "unknown" and (access["displacement"] >= 0 or access["base"] == "container_of")
        displacement = access["displacement"] + argument["displacement"] if known else -1
        rebased = {
            **access,
            "displacement": displacement,
            "param": argument.get("param"),
            "base": _rebased_base(argument.get("base"), access["base"]),
        }
        if argument.get("inner", argument["displacement"]) != argument["displacement"] and known:
            # The caller's register that holds the argument's base (a helper's result, a walk's
            # member address) other than the base itself: the access is this far from it.
            rebased["inner_displacement"] = access["displacement"] + argument["inner"]
        elif argument["displacement"] != 0 and known:
            # The machine code may keep the member's address in a register: the access's own
            # displacement from it, the innermost one.
            rebased["inner_displacement"] = access.get("inner_displacement", access["displacement"])
        if "element" in argument and known and access.get("element_displacement") is None:
            # Past the argument's variable index (&cpu_base->clock_base[idx]): from the element.
            rebased["element_displacement"] = argument["element"] + access["displacement"]
        if argument.get("index"):
            # The argument's own parameter indices, in the caller's parameters.
            rebased["index"] = [*access.get("index", []), *argument["index"]]
        held = _object_of(argument)
        # The address of a local object is no parameter, whatever value it stands for besides.
        return {**rebased, **held, "param": None} if held else rebased
    unit = argument.get("unit")
    if unit is not None and unit["kind"] == "global" and unit.get("field") is None and argument.get("param") is None:
        # The address of a whole global: the field is that far into the global.
        return {**access, "base": "global", "param": None}
    return {**access, "param": None}


def _bind(child: Effective, arguments: list[dict[str, Any]]) -> Effective:
    """Give the accesses a helper makes through its parameters the state its caller passed."""
    bound = Effective(
        calls=[],
        unevaluated=list(child.unevaluated),
        list_calls=list(child.list_calls),
        indirect=list(child.indirect),
        function_pointers=list(child.function_pointers),
        field_accesses=[
            _rebase(item, arguments[item["param"]] if item["param"] < len(arguments) else None)
            if item.get("param") is not None
            else item
            for item in (_fix_index(access, arguments) for access in child.field_accesses)
        ],
        folds=list(child.folds),
        unbound=list(child.unbound),
    )
    for access in child.accesses:
        if access.get("unit") is not None:
            bound.accesses.append(access)
            continue
        index = access.get("param")
        argument = arguments[index] if index is not None and index < len(arguments) else None
        if argument is not None and argument.get("unit") is not None:
            unit = dict(argument["unit"])
            bound.accesses.append({**access, "unit": unit, "field": unit.get("field"), "param": None})
        elif argument is not None and argument.get("param") is not None:
            bound.accesses.append({**access, "param": argument["param"]})
        else:
            bound.unbound.append(access)
    for call in child.calls:
        rebound = []
        for item in (_fix_index(argument, arguments) for argument in call["arguments"]):
            index = item.get("param")
            if index is None or index >= len(arguments):
                rebound.append(item)
            elif "displacement" in item:
                # The parameter itself (0) or a member of it: the caller's argument, that much further.
                outer = arguments[index]
                if "displacement" in outer:
                    nested = {
                        **item,
                        "param": outer.get("param"),
                        "displacement": item["displacement"] + outer["displacement"],
                        "base": _rebased_base(outer.get("base"), item.get("base", "pointer")),
                    }
                    if "element" in outer and "element" not in item:
                        nested["element"] = outer["element"] + item["displacement"]
                    if outer.get("inner", outer["displacement"]) != outer["displacement"]:
                        nested["inner"] = item["displacement"] + outer["inner"]
                    if outer.get("index"):
                        nested["index"] = [*item.get("index", []), *outer["index"]]
                    held = _object_of(outer)
                    rebound.append({**nested, **held, "param": None} if held else nested)
                elif item["displacement"] == 0:
                    rebound.append(outer)
                else:
                    rebound.append({**item, "param": None})
            elif item.get("unit") is None:
                # An untyped value derived from the parameter: the caller's argument stands for it, as a
                # value, not as a pointer an offset can rebase.
                rebound.append({key: value for key, value in arguments[index].items() if key not in _ADDRESS_KEYS})
            else:
                # A value loaded through the parameter (p->q): another object.
                rebound.append({**item, "param": None})
        bound.calls.append({**call, "arguments": rebound})
    return _guarded(bound, child, arguments)


def _renamed(bound: Effective, returned: str, result: str) -> Effective:
    """A folded helper's events, with the local object it returns by value named as the caller's
    local it initializes: after inlining they are one object."""

    def rename(item: dict[str, Any]) -> dict[str, Any]:
        return {**item, "object": result} if item.get("object") == returned else item

    return dataclasses.replace(
        bound,
        field_accesses=[rename(item) for item in bound.field_accesses],
        calls=[{**call, "arguments": [rename(item) for item in call["arguments"]]} for call in bound.calls],
    )


def _object_of(argument: dict[str, Any]) -> dict[str, Any]:
    """The caller's local object an argument is the address of (prepare_alloc_pages(..., &ac, ...)),
    which a folded callee's fields through that parameter are in."""
    if argument.get("base") not in ("local", "other") or "object" not in argument:
        return {}
    return {"object": argument["object"], "object_size": argument["object_size"]}


def _fix_index(item: dict[str, Any], arguments: list[dict[str, Any]]) -> dict[str, Any]:
    """An access or argument whose address steps by the helper's parameters (``rw_sem + level - 1``),
    as its caller's arguments give them: a constant argument adds its steps to the displacement
    (sb_end_write's SB_FREEZE_WRITE: 96 bytes); the caller's own parameter carries the step on; any
    other value leaves a variable index in a register."""
    terms = item.get("index")
    if not terms:
        return item
    shift, kept = 0, []
    for term in terms:
        argument = arguments[term["param"]] if term["param"] < len(arguments) else None
        value = _constant(argument)
        expression = argument.get("expr") if argument is not None else None
        if value is not None:
            shift += value * term["scale"]
        elif isinstance(expression, dict) and "p" in expression and set(expression) <= {"p", "w", "s"}:
            kept.append({"param": expression["p"], "scale": term["scale"]})
    fixed = {key: value for key, value in item.items() if key != "index"}
    if kept:
        fixed["index"] = kept
    if shift and "displacement" in fixed and fixed.get("base") != "unknown" and fixed["displacement"] != -1:
        fixed["displacement"] += shift
    return fixed


# Guards: the code of a helper compiled only for some values of its parameters --------------------
#
# The facts give an event ``guards``: conditions on the function's parameters (expressions.py states
# them). An event is compiled only where every guard's expression is truthy as its "v" says. At a fold
# the caller's arguments replace the parameters; a guard every value of which is decided against the
# event makes it dead code the compiler removed for these arguments.


def _argument_values(arguments: list[dict[str, Any]]) -> list[dict[str, Any] | None]:
    """The value each argument passes, as an expression in the caller's terms, or None."""
    return [argument.get("expr") for argument in arguments]


def _constant(argument: dict[str, Any] | None) -> int | None:
    """The integer an argument always passes, or None."""
    return expressions.evaluate(argument.get("expr")) if argument is not None else None


def _resolved(items: list[dict[str, Any]], table: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Events with their guard indexes replaced by the guards of the function's table."""
    return [
        {**item, "guards": [table[index] for index in item["guards"]]} if item.get("guards") else item for item in items
    ]


def _reframed(item: dict[str, Any], guards: list[dict[str, Any]]) -> dict[str, Any]:
    """An event of a function folded through a pointer: its own guards name parameters no argument
    binds, so it is under the given guards only."""
    copied = {key: value for key, value in item.items() if key != "guards"}
    return {**copied, "guards": list(guards)} if guards else copied


def _under(item: dict[str, Any], guards: list[dict[str, Any]]) -> dict[str, Any]:
    """An event of a folded helper, under the guards of the call that folded it."""
    if not guards:
        return item
    return {**item, "guards": [*item.get("guards", []), *guards]}


def _decided(
    guards: list[dict[str, Any]], arguments: list[dict[str, Any]], decisions: dict[int, Any] | None = None
) -> list[dict[str, Any]] | None:
    """The guards that still depend on a parameter once the arguments are bound, or None when one is
    decided against the event. ``decisions`` keeps each guard's outcome for these arguments by the
    guard's identity (with the guard, so that no identity is reused while it is kept): a helper's
    events share their guards (its table's, and those of the calls that folded it), so each is decided
    once per fold, and the guards it leaves stay shared a fold up."""
    decisions = {} if decisions is None else decisions
    left = []
    for guard in guards:
        key = id(guard)
        if key not in decisions:
            decisions[key] = (guard, _decision(guard, _argument_values(arguments)))
        outcome = decisions[key][1]
        if outcome is _AGAINST:
            return None
        if outcome is not None:
            left.append(outcome)
    return left


def _decision(guard: dict[str, Any], values: list[dict[str, Any] | None]) -> Any:
    """One guard with the arguments' values put in: ``_AGAINST`` where every value it may take is
    against the event, None where every one is for it or it no longer names a parameter, else the
    guard on the caller's parameters."""
    expression = expressions.substitute(guard["e"], values)
    truth = expressions.truth(expression)
    if truth is not None:
        return None if bool(truth) == guard["v"] else _AGAINST
    return {"e": expression, "v": guard["v"]} if expressions.mentions_parameter(expression) else None


def _guarded(bound: Effective, child: Effective, arguments: list[dict[str, Any]]) -> Effective:
    """The bound events with the arguments applied to their guards: an event a guard decides against
    is dead for these arguments (a call or an access to state becomes unevaluated, with reason
    ``constant_argument``; any other event is dropped). Each call's argument values are restated in
    the caller's terms, for the folds below it."""
    values = _argument_values(arguments)
    for original, call in zip(child.calls, bound.calls, strict=True):
        restated = []
        for item, argument in zip(original["arguments"], call["arguments"], strict=True):
            value = expressions.substitute(item["expr"], values) if "expr" in item else None
            restated.append(
                {**argument, "expr": value} if value is not None else {k: v for k, v in argument.items() if k != "expr"}
            )
        call["arguments"] = restated
    dead: list[dict[str, Any]] = []
    decisions: dict[int, Any] = {}
    for kind in ("calls", *_EVENT_LISTS, "unbound"):
        kept = []
        for item in getattr(bound, kind):
            if not item.get("guards"):
                kept.append(item)
                continue
            left = _decided(item["guards"], arguments, decisions)
            if left is not None:
                kept.append({**item, "guards": left} if left else {k: v for k, v in item.items() if k != "guards"})
            elif kind == "unevaluated":
                kept.append({k: v for k, v in item.items() if k != "guards"})
            elif kind == "calls":
                dead.append({"call": item["callee"], "reason": "constant_argument", "provenance": item["provenance"]})
            elif kind == "accesses" and item.get("unit") is not None:
                dead.append({"unit": item["unit"], "reason": "constant_argument", "provenance": item["provenance"]})
        setattr(bound, kind, kept)
    bound.unevaluated += dead
    return bound


def _hex(value: int) -> str:
    return f"0x{value:x}"


class GraphBuilder:
    """Nodes and aggregated edges. An edge is one kind between two nodes (with its access and
    field); its provenance lists every site."""

    def __init__(self) -> None:
        self.nodes: dict[str, dict[str, Any]] = {}
        self.edges: dict[tuple[Any, ...], dict[str, Any]] = {}

    def node(self, node_id: str, kind: str, name: str, **fields: Any) -> dict[str, Any]:
        node = self.nodes.get(node_id)
        if node is None:
            node = {"id": node_id, "kind": kind, "name": name, "provenance": []}
            self.nodes[node_id] = node
        for key, value in fields.items():
            if value is not None and node.get(key) in (None, False, [], ""):
                node[key] = value
        return node

    def provenance(self, node_id: str, entries: list[dict[str, Any]]) -> None:
        self.nodes[node_id]["provenance"].extend(entries)

    def edge(self, kind: str, source: str, target: str, provenance: list[dict[str, Any]], **fields: Any) -> None:
        if not provenance:
            raise ValueError(f"{kind} {source}->{target} has no provenance")
        identity = (
            kind,
            source,
            target,
            *(
                f"{key}={fields[key]}"
                for key in sorted(fields)
                if key in ("access", "field", "membership", "resolution", "slot")
            ),
        )
        edge = self.edges.get(identity)
        if edge is None:
            edge = {"kind": kind, "source": source, "target": target, "provenance": [], **fields}
            self.edges[identity] = edge
        edge["provenance"].extend(provenance)

    def finalize(self) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        for item in (*self.nodes.values(), *self.edges.values()):
            unique = {jsonio.canonical(entry): entry for entry in item["provenance"]}
            item["provenance"] = [unique[key] for key in sorted(unique)]
        nodes = [self.nodes[key] for key in sorted(self.nodes)]
        edges = sorted(self.edges.values(), key=jsonio.canonical)
        return nodes, edges


def build(
    config: Config,
    part: str,
    part_files: list[str],
    tus: dict[str, Facts],
    image: Image,
    contracts: ContractGraph,
    inputs: dict[str, Any],
    cache: LinkCache | None = None,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """The graph and C3 exclusions of one part.

    The link has two layers. The core is everything the unit facts, the image and the configuration
    decide, with the image copy the contract graph pins for each definition; the contract layer is
    the contract each function node names, the callback-table edges and the contract functions of
    the part. With a cache, a core is reused while every input it reads is unchanged, so a landing
    that changes only the contract graph links only the contract layer again."""
    linker = Linker(config, part_files, tus, image, contracts)
    core: dict[str, Any] | None = None
    key = cache.key(config, part_files, _pins(linker)) if cache is not None else None
    if cache is not None and key is not None:
        core = cache.load(part_files, key)
        if core is not None and core["image_facts"] != _image_digest(linker, core["analysis"]["part_functions"]):
            core = None
    if core is None:
        core = _link_core(linker, config, part_files)
        if cache is not None and key is not None:
            cache.store(part_files, key, core)
    return _assemble(core, linker, contracts, part, part_files, inputs)


def _link_core(linker: Linker, config: Config, part_files: list[str]) -> dict[str, Any]:
    """Nodes, edges, exclusions and analysis from the unit facts, the image and the configuration."""
    image = linker.image
    roots: dict[str, Definition] = {}
    for tu in sorted(linker.part_files):
        unit_file = os.path.basename(tu)
        for name, function in linker.tus[tu]["functions"].items():
            definition = linker.visible(tu, name)
            if definition is None:
                continue
            symbols = linker.symbols(definition)
            # The unit's own functions, and every header function the unit compiled a copy of: a copy
            # no part function calls exists because the unit takes its address (a table, a store).
            if symbols and (_owned(function) or any(item.local and item.file == unit_file for item in symbols)):
                roots[definition.id] = linker.node_of(definition)
    functions = dict(roots)
    effective: dict[str, Effective] = {}
    pending = sorted(functions)
    while pending:
        identity = pending.pop()
        if identity in effective:
            continue
        definition = functions[identity]
        result = linker.effective(definition.tu, definition.name, definition)
        effective[identity] = result
        callees = linker.image_callees(definition)
        for call in result.calls:
            helper = linker.visible(call["tu"], call["callee"])
            if (
                helper is not None
                and helper.id not in functions
                and call["callee"] in callees
                and linker.symbols(helper)
            ):
                functions[helper.id] = linker.node_of(helper)
                pending.append(helper.id)
    specialized = _specialized_parameters(linker, functions)
    specializations: set[tuple[str, int, str]] = set()
    builder = GraphBuilder()
    analysis: dict[str, Any] = {
        "folds": [],
        "indirect_slots": [],
        "alternate_definitions": {},
    }
    exclusions: list[dict[str, Any]] = []

    def exclude(
        kind: str,
        reason: str,
        function: str | None,
        target: str | None,
        detail: Any,
        provenance: list[dict[str, Any]],
    ) -> None:
        exclusions.append(
            {
                "kind": kind,
                "reason": reason,
                "function": function,
                "target": target,
                "detail": detail,
                "provenance": provenance,
            }
        )

    # C3 compatibility seam: facts before U2's integration have no ``exclusions`` key and add no
    # items. U2 emits the same item shape; bare function names become the C1 node id here.
    for tu, unit_facts in linker.tus.items():
        for item in unit_facts.get("exclusions", []):
            function = item.get("function")
            if function is not None and ":" not in function:
                function = linker.resolve(tu, function)
            target = item.get("target")
            if item.get("kind") == "call" and target is not None and ":" not in target:
                target = linker.resolve(tu, target)
            exclude(
                item["kind"],
                item["reason"],
                function,
                target,
                item.get("detail"),
                item.get("provenance", []),
            )
    image_globals: dict[str, set[str]] = {}
    for identity, definition in sorted(functions.items()):
        image_facts = linker.image_facts(definition)
        image_globals[identity] = {
            name
            for fact in image_facts
            for entry in fact.globals
            for name in (entry["global"], *entry.get("aliases", []))
        }
        symbols = linker.symbols(definition)
        provenance = [{"source": "c", "file": definition.file, "line": definition.line}]
        provenance += [{"source": "image", "address": _hex(symbol.address)} for symbol in symbols]
        builder.node(
            identity,
            "function",
            definition.name,
            part=True,
            c_file=definition.file,
            line=definition.line,
            image_addresses=sorted(_hex(symbol.address) for symbol in symbols),
            weak_definitions=[f"{item.file}:{item.line}" for item in linker.alternates.get(identity, [])] or None,
        )
        builder.provenance(identity, provenance)
    for identity, alternates in sorted(linker.alternates.items()):
        node = linker.primary[identity]
        analysis["alternate_definitions"][identity] = {
            "node": f"{node.file}:{node.line}", "weak": node.weak,
            "others": [{"definition": f"{item.file}:{item.line}", "weak": item.weak} for item in alternates],
        }  # fmt: skip

    def function_node(identity: str, name: str, where: list[dict[str, Any]]) -> None:
        if identity in builder.nodes:
            return
        definition = linker.primary.get(identity)
        if definition is not None:
            builder.node(identity, "function", name, part=False, c_file=definition.file, line=definition.line)
            builder.provenance(identity, [{"source": "c", "file": definition.file, "line": definition.line}])
        else:
            builder.node(identity, "function", name, part=False)
            builder.provenance(identity, where[:1])

    unit_nodes: dict[str, Unit] = {}

    def state_node(unit: Unit, where: list[dict[str, Any]], known: str | None = None) -> str | None:
        identity = known or linker.unit_id(unit)
        if identity is None:
            return None
        if identity not in builder.nodes:
            if unit["kind"] == "struct":
                record = linker.records[f"{unit['name']}@{unit['def']}"]
                file, line = record["def"].rsplit(":", 1)
                builder.node(
                    identity,
                    "struct",
                    unit["name"],
                    c_file=file,
                    line=int(line),
                    size=record["size"],
                    plain=record["plain"],
                    per_cpu=True if unit["name"] in config.vocabulary.percpu_structs else None,
                )
                builder.provenance(identity, [{"source": "c", "file": file, "line": int(line)}])
            else:
                node_kind = identity.split(":", 1)[0]
                symbols = image.object_symbols.get(unit["name"], [])
                symbol = symbols[0] if len(symbols) == 1 else None
                builder.node(
                    identity,
                    node_kind,
                    unit["name"],
                    lock=bool(unit.get("lock")) or None,
                    image_address=_hex(symbol.address) if symbol else None,
                    image_size=symbol.size if symbol else None,
                    section=symbol.section if symbol else None,
                )
                entries = list(where[:1])
                if unit.get("def"):
                    file, line = unit["def"].rsplit(":", 1)
                    entries = [{"source": "c", "file": file, "line": int(line)}]
                if symbol is not None:
                    entries.append({"source": "image", "address": _hex(symbol.address)})
                builder.provenance(identity, entries)
            unit_nodes[identity] = unit
        elif unit.get("lock") and builder.nodes[identity]["kind"] != "struct":
            builder.nodes[identity]["lock"] = True
        return identity

    c_calls: collections.Counter[tuple[str, str]] = collections.Counter()
    c_targets: dict[str, list[dict[str, Any]]] = {}
    # The local objects whose address each function passes to a call: callee, position, object, offset.
    object_arguments: dict[str, set[tuple[str, int, str, int]]] = collections.defaultdict(set)
    field_accesses: dict[str, list[dict[str, Any]]] = {}
    global_struct_offsets: dict[str, list[dict[str, Any]]] = {}
    named_records: dict[str, dict[str, str]] = collections.defaultdict(dict)
    list_calls: list[tuple[str, dict[str, Any]]] = []
    image_pairs: set[tuple[str, str]] = set()
    # The C names at the start of each merged-globals block a function's machine code forms: the image
    # gives the block, the address is that of each name there.
    block_start_names: dict[str, set[str]] = collections.defaultdict(set)
    for identity, definition in sorted(functions.items()):
        result = effective[identity]
        image_facts = linker.image_facts(definition)
        for fact in image_facts:
            for item in getattr(fact, "exclusions", []):
                function = item.get("function") or identity
                if ":" not in function:
                    function = identity
                detail = item.get("detail")
                if item["reason"] in _ADDRESS_NAMES and isinstance(detail, dict):
                    block_start_names[identity] |= set(detail.get("names", []))
                exclude(
                    item["kind"],
                    item["reason"],
                    function,
                    item.get("target"),
                    detail,
                    item.get("provenance", []),
                )
        image_calls = {call["target"]: call for fact in image_facts for call in fact.calls}
        for call in result.calls:
            if (call.get("generic") or call.get("conditional")) and call["callee"] not in image_calls:
                # A call in a _Generic association whose selection could not be read, or under a
                # __builtin_constant_p choice, that the machine code does not make: not compiled.
                exclude(
                    "call",
                    "generic_unconfirmed" if call.get("generic") else "image_decided",
                    identity,
                    linker.resolve(call["tu"], call["callee"]),
                    {"event": "call"},
                    call["provenance"],
                )
                continue
            target = linker.resolve(call["tu"], call["callee"])
            function_node(target, call["callee"], call["provenance"])
            image_call = image_calls.get(call["callee"])
            builder.edge(
                "call",
                identity,
                target,
                call["provenance"],
                image=("tail" if image_call["tail"] else "direct") if image_call else "none",
            )
            c_calls[(identity, target)] += len(call["provenance"])
            for position, argument in enumerate(call["arguments"]):
                if argument.get("base") == "local" and "object" in argument and "displacement" in argument:
                    # &object passed to a call the image makes: where the frame holds the object.
                    object_arguments[identity].add(
                        (call["callee"], position, argument["object"], argument["displacement"])
                    )
        c_names = {call["callee"] for call in result.calls}
        for fact in image_facts:
            for call in fact.calls:
                image_pairs.add((identity, linker.resolve(definition.tu, call["target"])))
                if call["target"] not in c_names:
                    target = linker.resolve(definition.tu, call["target"])
                    function_node(target, call["target"], call["provenance"])
                    builder.edge("call", identity, target, call["provenance"], image_only=True, tail=call["tail"])
        targets: dict[str, str] = {}
        addressed = image_globals[identity]
        # Every call site gives the parameter one global, and the image addresses it, or an address
        # its initializer stores: the compiler specialized the function to that global.
        bound_parameters = _bound_parameters(linker, specialized.get(identity, {}), addressed)
        # This code's arguments that such a callee takes in its own code (do_copy's xwrite(..., &wfile_pos)):
        # where this code's image forms no address of the global, its accesses of it within the
        # argument are the callee's.
        moved: dict[tuple[str, int, tuple[str, ...], str], tuple[str, int]] = {}
        for call in result.calls:
            callee = linker.resolve(call["tu"], call["callee"])
            values = _bound_parameters(linker, specialized.get(callee, {}), image_globals.get(callee, set()))
            for position, argument in enumerate(call["arguments"]):
                unit, value, lines = argument.get("unit"), values.get(position), argument.get("lines")
                if unit is None or value is None or lines is None or unit["kind"] != "global":
                    continue
                if unit["name"] != value["name"] or unit["name"] in addressed:
                    continue
                for site in call["provenance"]:
                    for line in range(lines[0], lines[1] + 1):
                        moved[(site["file"], line, tuple(site.get("via", [])), unit["name"])] = (callee, position)
        here = [{"source": "c", "file": definition.file, "line": definition.line}]
        # An access through such a parameter is the global's. The C records none for a dereference
        # of a scalar (``*pos``) or a field through the pointer (``tk->f`` is struct state): the
        # global the image addresses is read.
        implied = [
            {"unit": value, "access": "read", "field": None, "provenance": here, "parameter": index, "implied": True}
            for index, value in sorted(bound_parameters.items())
            if value["name"] in addressed
        ]
        for access in [*result.accesses, *implied]:
            unit = access.get("unit")
            bound_global = bound_parameters.get(access.get("param", -1)) if unit is None else None
            if bound_global is not None:
                unit = dict(bound_global)
                access = {
                    **access,
                    "unit": unit,
                    "field": unit.get("field"),
                    "param": None,
                    "parameter": access["param"],
                }
            if access.get("parameter") is not None:
                if access.get("implied") and unit is not None and unit["name"] in targets:
                    continue
                specializations.add(
                    (identity, access["parameter"], str(linker.unit_id(bound_parameters[access["parameter"]])))
                )
            if unit is None:
                exclude(
                    "access",
                    "parameter_at_root",
                    identity,
                    None,
                    {"field": access.get("field"), "parameter": access.get("param")},
                    access["provenance"],
                )
                continue
            moves = {
                moved.get((site.get("file", ""), site.get("line", 0), tuple(site.get("via", [])), unit["name"]))
                for site in access["provenance"]
            }
            if unit["kind"] == "global" and access.get("parameter") is None and None not in moves:
                callee, position = min(move for move in moves if move is not None)
                exclude(
                    "access",
                    "argument_specialized_into_callee",
                    identity,
                    linker.unit_id(unit),
                    {"callee": callee, "parameter": position},
                    access["provenance"],
                )
                continue
            unit_id = linker.unit_id(unit)
            if access.get("conditional") and unit["kind"] == "global" and unit["name"] not in addressed:
                exclude("access", "image_decided", identity, unit_id, {"event": "access"}, access["provenance"])
                continue
            if access.get("generic") and (unit["kind"] != "global" or unit["name"] not in addressed):
                # In a _Generic association whose selection could not be read: a global the
                # machine code addresses stays; a struct access cannot be confirmed and goes.
                exclude("access", "generic_unconfirmed", identity, unit_id, {"event": "access"}, access["provenance"])
                continue
            state = state_node(unit, access["provenance"])
            if state is None:
                exclude(
                    "access",
                    "record_unknown",
                    identity,
                    unit_id,
                    {"unit": unit["name"], "kind": unit["kind"]},
                    access["provenance"],
                )
                continue
            target = state
            builder.edge(
                "access", identity, target, access["provenance"], access=access["access"], field=access.get("field")
            )
            if unit["kind"] == "global":
                targets[unit["name"]] = "per_cpu" if target.startswith("percpu:") else _c_class(config, unit["name"])
                if unit.get("record"):
                    named_records[identity][unit["name"]] = unit["record"]
                for item in linker.initializer_values(unit, access.get("field")):
                    _initializer_value(
                        builder, config, state_node, identity, str(unit_id), item, access, addressed, targets
                    )
        for index, value in sorted(bound_parameters.items()):
            if value["name"] not in addressed:
                # The image forms only the addresses the global's initializer stores: the compiler
                # read the field and folded its value. The global itself is not addressed.
                specializations.add((identity, index, str(linker.unit_id(value))))
                for item in linker.initializer_values(value, None):
                    _initializer_value(
                        builder, config, state_node, identity, str(linker.unit_id(value)), item,
                        {"provenance": here}, addressed, targets,
                    )  # fmt: skip
        # Globals the function's machine code addresses that its C (after folding) does not name:
        # the access a compiler moved here by inlining, or one written in assembly.
        for fact in image_facts:
            for item in fact.globals:
                if item["target_class"] not in ("state", "per_cpu") or {item["global"], *item.get("aliases", [])} & set(
                    targets
                ):
                    continue
                symbols = image.object_symbols.get(item.get("symbol", item["global"]), [])
                identity_hint = linker.image_global_id(item["global"], symbols[0].file if symbols else "")
                unit = {"kind": "global", "name": item["global"], "def": None, "field": None, "lock": False}
                image_state = state_node(unit, item["provenance"], identity_hint)
                if image_state is not None:
                    builder.edge("access", identity, image_state, item["provenance"], access="image", image_only=True)
        for item in result.unevaluated:
            if "call" in item:
                exclude(
                    "call",
                    item["reason"],
                    identity,
                    linker.resolve(definition.tu, item["call"]),
                    {"event": "unevaluated"},
                    item["provenance"],
                )
            elif item.get("unit") is not None:
                exclude(
                    "access",
                    item["reason"],
                    identity,
                    linker.unit_id(item["unit"]),
                    {"event": "unevaluated"},
                    item["provenance"],
                )
        for slot in result.indirect:
            analysis["indirect_slots"].append(
                {
                    "function": identity,
                    "ops_struct": slot["ops_struct"],
                    "field": slot["field"],
                    "provenance": slot["provenance"],
                }
            )
            # A slot of a table of the function's own unit, whose target the image calls directly:
            # the C initializer and the machine code agree on the target.
            for table in linker.tus[definition.tu]["tables"]:
                if table["ops_struct"] != slot["ops_struct"]:
                    continue
                for entry in table["entries"]:
                    if entry["field"] == slot["field"] and entry["target"] in image_calls:
                        target = linker.resolve(definition.tu, entry["target"])
                        function_node(target, entry["target"], table["provenance"])
                        builder.edge(
                            "call_indirect",
                            identity,
                            target,
                            slot["provenance"] + table["provenance"],
                            resolution="table_slot",
                            slot=f"{table['name']}.{slot['field']}",
                        )
        for pointer in result.function_pointers:
            target = linker.resolve(definition.tu, pointer["target"])
            function_node(target, pointer["target"], pointer["provenance"])
            builder.edge("call_indirect", identity, target, pointer["provenance"], resolution="address_taken")
        for call in result.list_calls:
            list_calls.append((identity, {**call, "tu": definition.tu}))
        for fold in result.folds:
            analysis["folds"].append({"image_caller": identity, **fold})
        for access in result.unbound:
            exclude(
                "access",
                "unbound_parameter",
                identity,
                None,
                {"field": access.get("field"), "parameter": access.get("param")},
                access["provenance"],
            )
        c_targets[identity] = [
            # Whether the image has any symbol of this name: a global the compiler removed has none.
            {"name": name, "target_class": kind, "image_symbol": name in image.map_source_names}
            for name, kind in sorted(targets.items())
        ]
        field_accesses[identity] = sorted(
            {jsonio.canonical(_field_access(item)): _field_access(item) for item in result.field_accesses}.values(),
            key=jsonio.canonical,
        )
        structs = {item["struct"] for item in field_accesses[identity]}
        inside = [
            {"global": name, "struct": struct, "offsets": offsets}
            for name, record in sorted(named_records[identity].items())
            for struct, offsets in sorted(linker.struct_places(name, record).items())
            if struct in structs
        ]
        if inside:
            global_struct_offsets[identity] = inside

    tables = sorted(
        (table for unit_facts in linker.tus.values() for table in unit_facts["tables"]),
        key=jsonio.canonical,
    )
    excluded, structure_drops = structure.layout_edges(builder, linker.records, config.vocabulary)
    exclusions.extend(structure_drops)
    exclusions.extend(structure.membership_edges(builder, linker, list_calls))
    nodes, edges = builder.finalize()
    subsystems = structure.subsystems(nodes, edges)
    structure.attach_guard_locks(subsystems, edges)
    for subsystem in subsystems:
        builder.node(
            subsystem["id"],
            "subsystem",
            subsystem["name"],
            members=subsystem["members"],
            guard_locks=subsystem["guard_locks"],
        )
        builder.provenance(subsystem["id"], subsystem["provenance"])
        for member, provenance in subsystem["member_provenance"].items():
            builder.edge("in_subsystem", member, subsystem["id"], provenance)
    nodes, edges = builder.finalize()
    image_names = {identity: sorted(names) for identity, names in image_globals.items()}
    witnesses = [witness for tu in sorted(linker.part_files) for witness in linker.tus[tu]["witnesses"]]
    image_analysis = _image_analysis(linker, functions, image_names, image_pairs, field_accesses)
    analysis.update(
        {
            "part_functions": sorted(functions),
            "roots": sorted(roots),
            "c_calls": [
                {"source": source, "target": target, "sites": sites}
                for (source, target), sites in sorted(c_calls.items())
            ],
            "c_targets": c_targets,
            "field_accesses": field_accesses,
            "image": image_analysis,
            "callback_tables": tables,
            "excluded_embedding_bases": excluded,
            "membership_witnesses": sorted(witnesses, key=jsonio.canonical),
            "generic_selections": {
                key: sum(linker.tus[tu]["generic_selections"][key] for tu in linker.part_files)
                for key in ("selected", "ambiguous")
            },
            "parse_errors": {
                tu: linker.tus[tu]["errors"] for tu in sorted(linker.part_files) if linker.tus[tu]["errors"]
            },
            "global_struct_offsets": global_struct_offsets,
            "object_arguments": {
                identity: [list(item) for item in sorted(items)] for identity, items in sorted(object_arguments.items())
            },
            "specialized_parameters": [
                {"function": function, "parameter": index, "global": target}
                for function, index, target in sorted(specializations)
            ],
        }
    )
    for key in ("folds", "indirect_slots"):
        analysis[key] = sorted(analysis[key], key=jsonio.canonical)
    analysis["image"]["block_start_names"] = {
        identity: sorted(names) for identity, names in sorted(block_start_names.items())
    }
    return {
        "format": _CORE_FORMAT,
        "nodes": nodes,
        "edges": edges,
        "exclusions": exclusions,
        "analysis": analysis,
        "image_facts": _image_digest(linker, analysis["part_functions"]),
    }


def _unique_sorted(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
    unique = {jsonio.canonical(item): item for item in items}
    return [unique[key] for key in sorted(unique)]


def _assemble(
    core: dict[str, Any],
    linker: Linker,
    contracts: ContractGraph,
    part: str,
    part_files: list[str],
    inputs: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    """The contract layer over a core: the same graph a link of both layers at once gives."""
    nodes: dict[str, dict[str, Any]] = {node["id"]: node for node in core["nodes"]}
    analysis = dict(core["analysis"])
    part_functions = set(analysis["part_functions"])
    join = _join_contracts(
        contracts, linker.config.contract_function_prefix, _functions_at(linker), part_functions, linker.image.text_at
    )
    claimed: dict[str, list[str]] = collections.defaultdict(list)
    for contract_id, identity in join.owners.items():
        claimed[identity].append(contract_id)
    for identity in analysis["part_functions"]:
        ids = sorted(claimed.get(identity, []))
        if ids:
            node = nodes[identity]
            # One contract is its id; a function whose image copies several contract nodes name (a
            # header helper compiled into several units) carries the list. Each contract has one owner.
            node["contract"] = ids[0] if len(ids) == 1 else ids
            node["provenance"] = _unique_sorted(
                [*node["provenance"], *({"source": "contract_graph", "id": item} for item in ids)]
            )
    core_ids = set(nodes)
    builder = GraphBuilder()
    builder.nodes = nodes
    exclusions = list(core["exclusions"])
    callbacks = _callback_edges(
        builder,
        linker,
        contracts,
        analysis["part_functions"],
        exclusions,
        {contract_id: linker.primary[identity] for contract_id, identity in join.resolved.items()},
    )
    for identity, node in nodes.items():
        if identity not in core_ids:
            node["provenance"] = _unique_sorted(node["provenance"])
    added = sorted(
        ({**edge, "provenance": _unique_sorted(edge["provenance"])} for edge in builder.edges.values()),
        key=jsonio.canonical,
    )
    edges: list[dict[str, Any]] = []
    previous = 0
    for edge in added:
        position = bisect.bisect_left(core["edges"], jsonio.canonical(edge), lo=previous, key=jsonio.canonical)
        edges += core["edges"][previous:position]
        edges.append(edge)
        previous = position
    edges += core["edges"][previous:]
    expected_functions = _expected_functions(linker)
    exclusions.extend(join.drops)
    exclusion_document = jsonio.exclusion_document(part, exclusions)
    analysis.update(
        {
            "expected_functions": expected_functions,
            "resolved_callbacks": callbacks,
            "contract_mapping_duplicates": join.duplicates,
            "excluded": exclusion_document["counts"],
        }
    )
    return (
        {
            "format": "cgraph/graph/v1",
            "part": part,
            "files": sorted(part_files),
            "inputs": inputs,
            "nodes": [nodes[identity] for identity in sorted(nodes)],
            "edges": edges,
            "analysis": analysis,
        },
        exclusion_document,
    )


def _c_class(config: Config, name: str) -> str:
    kind = config.image.symbol_class(name)
    return kind if kind != "constant_data" else "state"


def _callback_edges(
    builder: GraphBuilder,
    linker: Linker,
    contracts: ContractGraph,
    functions: Collection[str],
    exclusions: list[dict[str, Any]],
    by_contract: dict[str, Definition],
) -> list[dict[str, Any]]:
    """Callback edges with every contract edge from a part function either linked or excluded.

    The contract graph names a slot's table by name only. A static table of the source's own unit
    is the one its code reads; another unit's table of the same name is not. A table the source's
    unit does not define is read from any unit that does (an external table)."""
    fields: dict[tuple[str, str], dict[str, set[str]]] = collections.defaultdict(lambda: collections.defaultdict(set))
    defined_in: dict[str, set[str]] = collections.defaultdict(set)
    for tu, facts in linker.tus.items():
        for table in facts["tables"]:
            defined_in[table["name"]].add(tu)
            for entry in table["entries"]:
                fields[(table["name"], tu)][entry["target"]].add(entry["field"])

    def initializer(table: str, source: Definition, target: str) -> set[str]:
        units = [source.tu] if source.tu in defined_in[table] else sorted(defined_in[table])
        return set().union(*(fields[(table, tu)].get(target, set()) for tu in units))

    part_ids = set(functions)

    def exclude(
        reason: str,
        source: Definition,
        target: Definition | None,
        edge: dict[str, Any],
        detail: dict[str, Any],
    ) -> None:
        exclusions.append(
            {
                "kind": "callback",
                "reason": reason,
                "function": source.id,
                "target": target.id if target is not None else str(edge.get("dst")),
                "detail": detail,
                "provenance": [{"source": "contract_graph", "id": f"{edge.get('src')}->{edge.get('dst')}"}],
            }
        )

    for edge in contracts.edges:
        if edge.get("kind") not in ("store", "install", "kit"):
            continue
        source = by_contract.get(str(edge.get("src")))
        if source is not None and source.id in part_ids:
            target = by_contract.get(str(edge.get("dst")))
            exclude(
                "kind_not_read",
                source,
                target,
                edge,
                {"edge_kind": edge["kind"], "evidence": edge.get("evidence", [])},
            )

    callbacks = []
    for edge in contracts.table_edges():
        source = by_contract.get(str(edge.get("src")))
        if source is None or source.id not in part_ids:
            continue
        target = by_contract.get(str(edge.get("dst")))
        if target is None:
            exclude("target_without_c_definition", source, None, edge, {"evidence": edge.get("evidence", [])})
            continue
        evidence = [item for item in edge.get("evidence", []) if item.startswith("table:")]
        if not evidence:
            exclude("evidence_not_a_table", source, target, edge, {"evidence": edge.get("evidence", [])})
            continue
        for item in evidence:
            table = item.split(":", 1)[1]
            field_names = initializer(table, source, target.name)
            if not field_names:
                exclude("no_c_initializer", source, target, edge, {"table": table})
                continue
            for field_name in sorted(field_names):
                if target.id not in builder.nodes:
                    builder.node(target.id, "function", target.name, part=False, c_file=target.file, line=target.line)
                    builder.provenance(target.id, [{"source": "c", "file": target.file, "line": target.line}])
                provenance = [{"source": "contract_graph", "id": f"{edge['src']}->{edge['dst']}"}]
                builder.edge(
                    "call_indirect",
                    source.id,
                    target.id,
                    provenance,
                    resolution="callback_table",
                    slot=f"{table}.{field_name}",
                )
                callbacks.append({"source": source.id, "target": target.id, "table": table, "field": field_name})
    return sorted({jsonio.canonical(item): item for item in callbacks}.values(), key=jsonio.canonical)


def _image_analysis(
    linker: Linker,
    functions: dict[str, Definition],
    image_globals: dict[str, list[str]],
    image_pairs: set[tuple[str, str]],
    field_accesses: dict[str, list[dict[str, Any]]],
) -> dict[str, Any]:
    calls, globals_, addresses, outside = [], [], [], []
    offsets: dict[str, list[int]] = {}
    constants: dict[str, list[int]] = {}
    frame_accesses: dict[str, list[list[int]]] = {}
    frame_addresses: dict[str, list[int]] = {}
    frame_arguments: dict[str, list[list[Any]]] = {}
    frame_indexed: dict[str, list[list[int]]] = {}
    for identity, definition in sorted(functions.items()):
        facts = linker.image_facts(definition)
        for fact in facts:
            for call in fact.calls:
                calls.append({**call, "source": identity, "target": linker.resolve(definition.tu, call["target"])})
            globals_ += [{**item, "function": identity} for item in fact.globals]
            addresses += [
                {**item, "source": identity, "target": linker.resolve(definition.tu, item["target"])}
                for item in fact.function_addresses
            ]
            outside += [{**item, "function": identity} for item in fact.outside]
        offsets[identity] = sorted({value for fact in facts for value in fact.memory_offsets})
        constants[identity] = sorted({value for fact in facts for value in fact.addi_constants})
        if any(item.get("object") for item in field_accesses.get(identity, [])):
            # The frame of a function with a local object's fields (frames.py); each copy's own.
            frame_accesses[identity] = [
                list(item) for item in sorted({item for fact in facts for item in fact.frame_accesses})
            ]
            frame_addresses[identity] = sorted({value for fact in facts for value in fact.frame_addresses})
            frame_indexed[identity] = [
                list(item) for item in sorted({item for fact in facts for item in fact.frame_indexed})
            ]
            targets = {call["provenance"][0]["address"]: call["target"] for fact in facts for call in fact.calls}
            frame_arguments[identity] = [
                [targets[_hex(address)], position, offset]
                for address, position, offset in sorted({item for fact in facts for item in fact.frame_arguments})
                if _hex(address) in targets
            ]

    def unique(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return sorted({jsonio.canonical(item): item for item in items}.values(), key=jsonio.canonical)

    return {
        "calls": unique(calls),
        "globals": unique(globals_),
        "function_addresses": unique(addresses),
        "outside_sized_symbols": unique(outside),
        "memory_offsets": offsets,
        "addi_constants": constants,
        "sized_symbol_names": linker.image.sized_object_names,
        "globals_by_function": image_globals,
        "call_pairs": len(image_pairs),
        "frame_accesses": frame_accesses,
        "frame_addresses": frame_addresses,
        "frame_arguments": frame_arguments,
        "frame_indexed": frame_indexed,
    }


def _initializer_value(
    builder: GraphBuilder,
    config: Config,
    state_node: Callable[..., str | None],
    identity: str,
    holder: str,
    item: dict[str, Any],
    access: dict[str, Any],
    addressed: Collection[str],
    targets: dict[str, str],
) -> None:
    """A global whose address the holder's initializer stores, where the function's machine code
    forms it: the compiler read the holder's field and folded the stored address."""
    value = item["target"]
    if value["name"] not in addressed:
        return
    state = state_node(value, item["provenance"])
    if state is None:
        return
    builder.edge(
        "access",
        identity,
        state,
        access["provenance"] + item["provenance"],
        access="read",
        field=None,
        initializer=holder,
    )
    targets[value["name"]] = "per_cpu" if state.startswith("percpu:") else _c_class(config, value["name"])


def _field_access(item: dict[str, Any]) -> dict[str, Any]:
    """A field access as the structure check reads it. ``address`` marks a member whose value is only
    its address (``&p->m``, an array member decaying to a pointer); ``element_displacement`` is the
    displacement past a variable index; ``param`` is the parameter the base pointer is."""
    access = {key: item.get(key, -1) for key in _FIELD_KEYS}
    if item.get("address"):
        access["address"] = True
    if item.get("conditional"):
        # Under a __builtin_constant_p choice: the image decides whether it was compiled.
        access["conditional"] = True
    for key in ("element_displacement", "inner_displacement", "param", "element_size", "object", "object_size"):
        if item.get(key) is not None:
            access[key] = item[key]
    return access


def _bound_parameters(linker: Linker, specialized: dict[int, Unit], addressed: set[str]) -> dict[int, Unit]:
    """A function's parameters every call site gives one global, where its image addresses that
    global or an address its initializer stores: the compiler specialized the function to it."""
    return {
        index: value
        for index, value in specialized.items()
        if value["name"] in addressed
        or any(item["target"]["name"] in addressed for item in linker.initializer_values(value, None))
    }


def _specialized_parameters(linker: Linker, functions: dict[str, Definition]) -> dict[str, dict[int, Unit]]:
    """For each static function of the part, the parameters every call site of its unit gives one
    global: directly, or through a parameter of a static caller every call site of which does. A
    function whose address its unit takes may be called from anywhere, and has none."""
    calls: dict[str, dict[str, list[tuple[str, list[dict[str, Any]]]]]] = {}
    taken: dict[str, set[str]] = {}

    def index(tu: str) -> tuple[dict[str, list[tuple[str, list[dict[str, Any]]]]], set[str]]:
        if tu not in calls:
            sites: dict[str, list[tuple[str, list[dict[str, Any]]]]] = collections.defaultdict(list)
            names: set[str] = set()
            for caller, function in linker.tus[tu]["functions"].items():
                for call in function["events"]["calls"]:
                    sites[call["callee"]].append((caller, call["arguments"]))
                names |= {pointer["target"] for pointer in function["events"]["function_pointers"]}
            for table in linker.tus[tu]["tables"]:
                names |= {entry["target"] for entry in table["entries"]}
            calls[tu], taken[tu] = dict(sites), names
        return calls[tu], taken[tu]

    def given(tu: str, name: str, parameter: int, seen: frozenset[tuple[str, int]]) -> Unit | None:
        sites, addressed = index(tu)
        definition = linker.visible(tu, name)
        if (
            (name, parameter) in seen
            or name in addressed
            or not sites.get(name)
            or not definition
            or not definition.static
        ):
            return None
        found: Unit | None = None
        for caller, arguments in sites[name]:
            if parameter >= len(arguments):
                return None
            argument = arguments[parameter]
            if argument.get("param") is not None:
                # The caller's own parameter pointer (displacement 0, or an untyped value); a member of
                # it or a pointer loaded through it is not one global.
                if argument.get("displacement", 0) != 0 or ("displacement" not in argument and argument.get("unit")):
                    return None
                value = given(tu, caller, argument["param"], seen | {(name, parameter)})
            else:
                value = argument.get("unit")
            if value is None or value["kind"] != "global":
                return None
            if found is not None and (found["name"], found.get("def"), found.get("field")) != (
                value["name"], value.get("def"), value.get("field"),
            ):  # fmt: skip
                return None
            found = value
        return found

    result: dict[str, dict[int, Unit]] = {}
    for identity, definition in sorted(functions.items()):
        if not definition.static:
            continue
        count = linker.tus[definition.tu]["functions"][definition.name].get("parameters", 0)
        bound = {
            parameter: value
            for parameter in range(count)
            if (value := given(definition.tu, definition.name, parameter, frozenset())) is not None
        }
        if bound:
            result[identity] = bound
    return result


def _owned(function: dict[str, Any]) -> bool:
    """A function the unit compiles as its own: one its file or a C file it includes defines (a unit
    such as a scheduler's build file includes several .c files), or one with external linkage that a
    header only it includes defines (kernel/rcu/tree.c's tree_stall.h). A header's inline is not."""
    return bool(function["owned"])


def _functions_at(linker: Linker) -> dict[int, list[str]]:
    """Each image address a C definition's copy starts at, with the node ids of the definitions there."""
    at: dict[int, set[str]] = collections.defaultdict(set)
    for definition in linker.definitions.values():
        for symbol in linker.symbols(definition):
            at[symbol.address].add(linker.node_of(definition).id)
    return {address: sorted(ids) for address, ids in at.items()}


@dataclass(frozen=True)
class ContractJoin:
    """The contract graph's function nodes joined to the graph's functions by image address."""

    owners: dict[str, str]  # contract id -> the part function that owns it
    resolved: dict[str, str]  # contract id -> the C function at its address (a callback may name a non-part one)
    drops: list[dict[str, Any]]  # C3 items: every function node no part function owns
    duplicates: list[dict[str, Any]]  # addresses that several function nodes give


def _join_contracts(
    contracts: ContractGraph,
    prefix: str,
    functions_at: dict[int, list[str]],
    part: set[str],
    text_at: dict[int, str],
) -> ContractJoin:
    """Each function node of the contract graph to the part function whose image copy starts at the
    node's address. The address is the machine code the contract is about, so the join holds where the
    node's id names another file than the C function's own: a file name with a hyphen, a function of
    another C file or of a header, an alias symbol. Every other function node is a C3 item."""
    owners: dict[str, str] = {}
    resolved: dict[str, str] = {}
    drops: list[dict[str, Any]] = []
    given: dict[int, list[str]] = collections.defaultdict(list)
    for node in contracts.function_nodes():
        identity = node["id"]
        address = contract_address(node)
        symbols = node.get("syms", [])
        detail: dict[str, Any] = {"address": node.get("addr") or None, "symbol": symbols[0] if symbols else None}
        if prefix and not identity.startswith(prefix):
            reason = "outside_function_prefix"
        elif address is None:
            # A macro, an always-inline helper or an assembly label: nothing in the image to own.
            reason = "no_image_symbol"
        else:
            given[address].append(identity)
            candidates = functions_at.get(address, [])
            in_part = [candidate for candidate in candidates if candidate in part]
            chosen = in_part or candidates
            if len(chosen) == 1:
                resolved[identity] = chosen[0]
            if len(in_part) == 1:
                owners[identity] = in_part[0]
                continue
            if in_part:
                reason = "address_of_several_part_functions"
                detail["candidates"] = in_part
            else:
                # Assembly, or a C function of a unit outside the part.
                reason = "no_part_function_at_address"
                detail["image_symbol"] = text_at.get(address)
                detail["c_functions"] = candidates
        drops.append(
            {
                "kind": "contract_function",
                "reason": reason,
                "function": None,
                "target": identity,
                "detail": detail,
                "provenance": [{"source": "contract_graph", "id": identity}],
            }
        )
    duplicates = [
        {"address": _hex(address), "ids": sorted(ids)} for address, ids in sorted(given.items()) if len(ids) > 1
    ]
    return ContractJoin(owners, resolved, drops, duplicates)


def _expected_functions(linker: Linker) -> list[str]:
    """Functions of the part's files that the image holds: an image symbol (a global one, or a local
    one that follows the unit's ELF FILE symbol) whose name a part unit defines."""
    expected: set[str] = set()
    for source in sorted(linker.part_files):
        base = os.path.basename(source)
        for name, function in linker.tus[source]["functions"].items():
            if not _owned(function):
                continue
            symbols = linker.image.functions.get(name, [])
            if any(
                (symbol.local and symbol.file in (base, os.path.basename(function["file"])))
                or (not symbol.local and not function["static"])
                for symbol in symbols
            ):
                definition = linker.visible(source, name)
                assert definition is not None
                expected.add(definition.id)
    return sorted(expected)
