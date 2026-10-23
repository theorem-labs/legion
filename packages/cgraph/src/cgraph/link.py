"""From the facts of each translation unit, the image and the contract graph, to one graph.

The link is a pure function of its inputs: the facts of each unit (whether parsed now or read from
the cache), the image and the contract graph. So a rebuild that reparses only changed units gives
the same graph as a full build.
"""

from __future__ import annotations

import collections
import os
from dataclasses import dataclass, field
from typing import Any

from . import structure
from .config import Config
from .contracts import ContractGraph
from .image import FunctionFacts, Image, Symbol

Facts = dict[str, Any]
Unit = dict[str, Any]
_EVENT_LISTS = ("accesses", "unevaluated", "list_calls", "indirect", "function_pointers", "field_accesses")


@dataclass
class Definition:
    """One C function definition: a name at one file and line."""

    name: str
    file: str
    line: int
    static: bool
    tu: str  # the first unit, in path order, that defines it
    id: str = ""


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
    unbound: int = 0


def _suffixes(keys: list[tuple[str, int]]) -> dict[tuple[str, int], str]:
    files = collections.Counter(file for file, _ in keys)
    return {(file, line): file if files[file] == 1 else f"{file}:{line}" for file, line in keys}


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
        for tu, facts in self.tus.items():
            for name, function in facts["functions"].items():
                key = (name, function["file"], function["line"])
                if key not in self.definitions:
                    definition = Definition(name, function["file"], function["line"], function["static"], tu)
                    self.definitions[key] = definition
                    self.by_name[name].append(definition)
        for name, definitions in self.by_name.items():
            if len(definitions) == 1:
                definitions[0].id = f"function:{name}"
                continue
            suffixes = _suffixes([(item.file, item.line) for item in definitions])
            for item in definitions:
                item.id = f"function:{name}@{suffixes[(item.file, item.line)]}"
        self.records: dict[str, dict[str, Any]] = {}
        for facts in self.tus.values():
            for key, record in facts["records"].items():
                self.records.setdefault(key, record)
        record_definitions: dict[str, list[tuple[str, int]]] = collections.defaultdict(list)
        for record in self.records.values():
            file, line = record["def"].rsplit(":", 1)
            record_definitions[record["name"]].append((file, int(line)))
        self.struct_ids: dict[str, str] = {}
        for name, keys in record_definitions.items():
            suffixes = _suffixes(sorted(set(keys)))
            for file, line in keys:
                self.struct_ids[f"{name}@{file}:{line}"] = (
                    f"struct:{name}" if len(suffixes) == 1 else f"struct:{name}@{suffixes[(file, line)]}"
                )
        self.global_keys: dict[str, set[str | None]] = collections.defaultdict(set)
        for facts in self.tus.values():
            for function in facts["functions"].values():
                for item in function["events"]["accesses"] + function["events"]["unevaluated"]:
                    unit = item.get("unit")
                    if unit is not None and unit["kind"] == "global":
                        self.global_keys[unit["name"]].add(unit.get("def"))
        self.symbol_cache: dict[tuple[str, str, int], list[Symbol]] = {}
        self.callee_cache: dict[tuple[str, str, int], set[str]] = {}
        self.effective_cache: dict[tuple[str, str, tuple[str, str, int]], Effective] = {}

    # Identities ----------------------------------------------------------------------------

    def visible(self, tu: str, name: str) -> Definition | None:
        function = self.tus[tu]["functions"].get(name)
        if function is None:
            return None
        return self.definitions[(name, function["file"], function["line"])]

    def resolve(self, tu: str, name: str) -> str:
        """The id a call to ``name`` from unit ``tu`` reaches."""
        definition = self.visible(tu, name)
        if definition is not None:
            return definition.id
        external = [item for item in self.by_name.get(name, []) if not item.static]
        if len(external) == 1:
            return external[0].id
        return f"function:{name}"

    def is_percpu(self, name: str) -> bool:
        if name in self.vocabulary.percpu_globals:
            return True
        return any(
            symbol.section in self.config.image.percpu_sections for symbol in self.image.object_symbols.get(name, [])
        )

    def unit_id(self, unit: Unit) -> str | None:
        if unit["kind"] == "struct":
            return self.struct_ids.get(f"{unit['name']}@{unit['def']}")
        name = unit["name"]
        prefix = "percpu" if self.is_percpu(name) else "global"
        keys = self.global_keys.get(name, {unit.get("def")})
        definition = unit.get("def")
        if len(keys) <= 1 or definition is None:
            return f"{prefix}:{name}"
        return f"{prefix}:{name}@{definition.rsplit(':', 1)[0]}"

    # The image -----------------------------------------------------------------------------

    def symbols(self, definition: Definition) -> list[Symbol]:
        key = (definition.name, definition.file, definition.line)
        if key in self.symbol_cache:
            return self.symbol_cache[key]
        candidates = self.image.functions.get(definition.name, [])
        contract = self.contracts.function(definition.file, definition.name)
        address = contract.get("addr") if contract else None
        if address:
            pinned = [symbol for symbol in candidates if symbol.address == int(address, 16)]
            if pinned:
                candidates = pinned
        if len(self.by_name[definition.name]) > 1 and not address:
            base = os.path.basename(definition.file)
            if definition.file.endswith(".c") and definition.static:
                candidates = [symbol for symbol in candidates if symbol.local and symbol.file == base]
            elif definition.file.endswith(".c"):
                candidates = [symbol for symbol in candidates if not symbol.local]
            else:
                others = {
                    os.path.basename(item.file) for item in self.by_name[definition.name] if item is not definition
                }
                candidates = [symbol for symbol in candidates if symbol.local and symbol.file not in others]
        self.symbol_cache[key] = candidates
        return candidates

    def image_facts(self, definition: Definition) -> list[FunctionFacts]:
        return [self.image.facts(symbol) for symbol in self.symbols(definition)]

    def image_callees(self, definition: Definition) -> set[str]:
        key = (definition.name, definition.file, definition.line)
        if key not in self.callee_cache:
            self.callee_cache[key] = {call["target"] for facts in self.image_facts(definition) for call in facts.calls}
        return self.callee_cache[key]

    # Folding -------------------------------------------------------------------------------

    def effective(self, tu: str, name: str, caller: Definition, stack: tuple[str, ...] = ()) -> Effective:
        """The events of ``name`` (as unit ``tu`` defines it) inside the image code of ``caller``.

        A visible helper is folded where the caller's image code has no direct call to it: the
        compiler inlined it there. A helper the image calls stays a call.
        """
        caller_key = (caller.name, caller.file, caller.line)
        key = (tu, name, caller_key)
        if key in self.effective_cache:
            return self.effective_cache[key]
        events = self.tus[tu]["functions"][name]["events"]
        result = Effective()
        for kind in _EVENT_LISTS:
            setattr(result, kind, list(events[kind]))
        callees = self.image_callees(caller)
        for call in events["calls"]:
            callee = call["callee"]
            if call.get("visible") and callee not in callees and callee not in stack and callee != name:
                helper = self.visible(tu, callee)
                assert helper is not None
                child = self.effective(tu, callee, caller, (*stack, name))
                bound = _bind(child, call["arguments"])
                result.calls += [_with_via(item, helper.id) for item in bound.calls]
                for kind in _EVENT_LISTS:
                    getattr(result, kind).extend(_with_via(item, helper.id) for item in getattr(bound, kind))
                result.folds.append({"helper": helper.id, "provenance": call["provenance"]})
                result.folds += bound.folds
                result.unbound += bound.unbound
            else:
                result.calls.append({**call, "tu": tu})
        self.effective_cache[key] = result
        return result


def _bind(child: Effective, arguments: list[dict[str, Any]]) -> Effective:
    """Give the accesses a helper makes through its parameters the state its caller passed."""
    bound = Effective(
        calls=[],
        unevaluated=list(child.unevaluated),
        list_calls=list(child.list_calls),
        indirect=list(child.indirect),
        function_pointers=list(child.function_pointers),
        field_accesses=list(child.field_accesses),
        folds=list(child.folds),
        unbound=child.unbound,
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
            bound.unbound += 1
    for call in child.calls:
        rebound = []
        for item in call["arguments"]:
            index = item.get("param")
            if item.get("unit") is None and index is not None and index < len(arguments):
                rebound.append(arguments[index])
            else:
                rebound.append(item)
        bound.calls.append({**call, "arguments": rebound})
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
            unique = {repr(sorted(entry.items())): entry for entry in item["provenance"]}
            item["provenance"] = [unique[key] for key in sorted(unique)]
        nodes = [self.nodes[key] for key in sorted(self.nodes)]
        edges = sorted(
            self.edges.values(),
            key=lambda edge: (
                edge["kind"],
                edge["source"],
                edge["target"],
                repr(sorted((key, str(value)) for key, value in edge.items() if key != "provenance")),
            ),
        )
        return nodes, edges


def build(
    config: Config,
    part: str,
    part_files: list[str],
    tus: dict[str, Facts],
    image: Image,
    contracts: ContractGraph,
    inputs: dict[str, Any],
) -> dict[str, Any]:
    """The graph of one part."""
    linker = Linker(config, part_files, tus, image, contracts)
    roots: dict[str, Definition] = {}
    for tu in sorted(linker.part_files):
        for name, function in linker.tus[tu]["functions"].items():
            if _owned(function):
                definition = linker.visible(tu, name)
                if definition is not None and linker.symbols(definition):
                    roots[definition.id] = definition
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
                functions[helper.id] = helper
                pending.append(helper.id)
    builder = GraphBuilder()
    analysis: dict[str, Any] = {
        "folds": [],
        "unevaluated": [],
        "indirect_slots": [],
        "unbound_parameter_accesses": 0,
        "generic_unconfirmed": [],
        "ambiguous_image_names": [],
    }
    image_globals: dict[str, set[str]] = {}
    for identity, definition in sorted(functions.items()):
        facts = linker.image_facts(definition)
        image_globals[identity] = {item["global"] for item in (entry for fact in facts for entry in fact.globals)}
        symbols = linker.symbols(definition)
        contract = contracts.function(definition.file, definition.name)
        provenance = [{"source": "c", "file": definition.file, "line": definition.line}]
        provenance += [{"source": "image", "address": _hex(symbol.address)} for symbol in symbols]
        if contract is not None:
            provenance.append({"source": "contract_graph", "id": contract["id"]})
        builder.node(
            identity,
            "function",
            definition.name,
            part=True,
            c_file=definition.file,
            line=definition.line,
            image_addresses=sorted(_hex(symbol.address) for symbol in symbols),
            contract=contract["id"] if contract else None,
        )
        builder.provenance(identity, provenance)
    for name, definitions in sorted(linker.by_name.items()):
        if len(definitions) > 1:
            analysis["ambiguous_image_names"].append(
                {
                    "name": name,
                    "definitions": sorted(item.id for item in definitions),
                    "mapped": {
                        item.id: sorted(_hex(symbol.address) for symbol in linker.symbols(item)) for item in definitions
                    },
                }
            )

    def function_node(identity: str, name: str, where: list[dict[str, Any]]) -> None:
        if identity in builder.nodes:
            return
        definition = next((item for item in linker.by_name.get(name, []) if item.id == identity), None)
        if definition is not None:
            builder.node(identity, "function", name, part=False, c_file=definition.file, line=definition.line)
            builder.provenance(identity, [{"source": "c", "file": definition.file, "line": definition.line}])
        else:
            builder.node(identity, "function", name, part=False)
            builder.provenance(identity, where[:1])

    unit_nodes: dict[str, Unit] = {}

    def state_node(unit: Unit, where: list[dict[str, Any]]) -> str | None:
        identity = linker.unit_id(unit)
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
    c_targets: dict[str, list[dict[str, str]]] = {}
    field_accesses: dict[str, list[dict[str, Any]]] = {}
    list_calls: list[tuple[str, dict[str, Any]]] = []
    image_pairs: set[tuple[str, str]] = set()
    for identity, definition in sorted(functions.items()):
        result = effective[identity]
        facts = linker.image_facts(definition)
        image_calls = {call["target"]: call for fact in facts for call in fact.calls}
        for call in result.calls:
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
        c_names = {call["callee"] for call in result.calls}
        for fact in facts:
            for call in fact.calls:
                image_pairs.add((identity, linker.resolve(definition.tu, call["target"])))
                if call["target"] not in c_names:
                    target = linker.resolve(definition.tu, call["target"])
                    function_node(target, call["target"], call["provenance"])
                    builder.edge("call", identity, target, call["provenance"], image_only=True, tail=call["tail"])
        targets: dict[str, str] = {}
        for access in result.accesses:
            unit = access.get("unit")
            if unit is None:
                continue
            if access.get("generic") and unit["kind"] == "global" and unit["name"] not in image_globals[identity]:
                analysis["generic_unconfirmed"].append(
                    {"function": identity, "unit": unit["name"], "provenance": access["provenance"]}
                )
                continue
            state = state_node(unit, access["provenance"])
            if state is None:
                continue
            target = state
            builder.edge(
                "access", identity, target, access["provenance"], access=access["access"], field=access.get("field")
            )
            if unit["kind"] == "global":
                targets[unit["name"]] = "per_cpu" if target.startswith("percpu:") else _c_class(config, unit["name"])
        # Globals the function's machine code addresses that its C (after folding) does not name:
        # the access a compiler moved here by inlining, or one written in assembly.
        for fact in facts:
            for item in fact.globals:
                if item["target_class"] not in ("state", "per_cpu") or item["global"] in targets:
                    continue
                unit = {"kind": "global", "name": item["global"], "def": None, "field": None, "lock": False}
                image_state = state_node(unit, item["provenance"])
                if image_state is not None:
                    builder.edge("access", identity, image_state, item["provenance"], access="image", image_only=True)
        for item in result.unevaluated:
            if item.get("unit") is not None:
                analysis["unevaluated"].append(
                    {
                        "function": identity,
                        "unit": linker.unit_id(item["unit"]),
                        "reason": item["reason"],
                        "provenance": item["provenance"],
                    }
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
        for pointer in result.function_pointers:
            target = linker.resolve(definition.tu, pointer["target"])
            function_node(target, pointer["target"], pointer["provenance"])
            builder.edge("call_indirect", identity, target, pointer["provenance"], resolution="address_taken")
        for call in result.list_calls:
            list_calls.append((identity, {**call, "tu": definition.tu}))
        for fold in result.folds:
            analysis["folds"].append({"image_caller": identity, **fold})
        analysis["unbound_parameter_accesses"] += result.unbound
        c_targets[identity] = [{"name": name, "target_class": kind} for name, kind in sorted(targets.items())]
        field_accesses[identity] = sorted(
            {
                repr(item): {key: item[key] for key in ("struct", "field", "offset")} for item in result.field_accesses
            }.values(),
            key=repr,
        )

    tables = sorted(
        (table for facts in linker.tus.values() for table in facts["tables"]),
        key=lambda table: (table["name"], table["provenance"][0]["file"]),
    )
    callbacks = _callback_edges(builder, linker, contracts, tables, functions)
    excluded = structure.layout_edges(builder, linker.records, config.vocabulary)
    structure.membership_edges(builder, linker, list_calls)
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
    expected_functions, contract_without_c = _expected_functions(linker)
    analysis.update(
        {
            "part_functions": sorted(functions),
            "roots": sorted(roots),
            "expected_functions": expected_functions,
            "contract_functions_without_c": contract_without_c,
            "c_calls": [
                {"source": source, "target": target, "sites": sites}
                for (source, target), sites in sorted(c_calls.items())
            ],
            "c_targets": c_targets,
            "field_accesses": field_accesses,
            "image": _image_analysis(linker, functions, image_names, image_pairs),
            "callback_tables": tables,
            "resolved_callbacks": callbacks,
            "excluded_embedding_bases": excluded,
            "membership_witnesses": witnesses,
            "generic_selections": {
                key: sum(linker.tus[tu]["generic_selections"][key] for tu in linker.part_files)
                for key in ("selected", "ambiguous")
            },
            "parse_errors": {
                tu: linker.tus[tu]["errors"] for tu in sorted(linker.part_files) if linker.tus[tu]["errors"]
            },
        }
    )
    for key in ("folds", "unevaluated", "indirect_slots", "generic_unconfirmed"):
        analysis[key] = sorted(analysis[key], key=repr)
    return {
        "format": "cgraph/graph/v1",
        "part": part,
        "files": sorted(part_files),
        "inputs": inputs,
        "nodes": nodes,
        "edges": edges,
        "analysis": analysis,
    }


def _c_class(config: Config, name: str) -> str:
    kind = config.image.symbol_class(name)
    return kind if kind != "constant_data" else "state"


def _callback_edges(
    builder: GraphBuilder,
    linker: Linker,
    contracts: ContractGraph,
    tables: list[dict[str, Any]],
    functions: dict[str, Definition],
) -> list[dict[str, Any]]:
    """Callback tables the contract graph resolved, kept only where a C initializer assigns the target."""
    fields: dict[tuple[str, str], set[str]] = collections.defaultdict(set)
    table_files: dict[str, str] = {}
    for table in tables:
        table_files.setdefault(table["name"], table["provenance"][0]["file"])
        for entry in table["entries"]:
            fields[(table["name"], entry["target"])].add(entry["field"])
    by_contract: dict[str, Definition] = {}
    for definition in linker.definitions.values():
        node = contracts.function(definition.file, definition.name)
        if node is not None:
            by_contract.setdefault(node["id"], definition)
    part_ids = set(functions)
    callbacks = []
    for edge in contracts.table_edges():
        source = by_contract.get(str(edge.get("src")))
        target = by_contract.get(str(edge.get("dst")))
        if source is None or target is None or source.id not in part_ids:
            continue
        for evidence in edge.get("evidence", []):
            if not evidence.startswith("table:"):
                continue
            table = evidence.split(":", 1)[1]
            for field_name in sorted(fields.get((table, target.name), set())):
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
    return sorted({repr(item): item for item in callbacks}.values(), key=repr)


def _image_analysis(
    linker: Linker,
    functions: dict[str, Definition],
    image_globals: dict[str, list[str]],
    image_pairs: set[tuple[str, str]],
) -> dict[str, Any]:
    calls, globals_, addresses, outside = [], [], [], []
    offsets: dict[str, list[int]] = {}
    constants: dict[str, list[int]] = {}
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

    def unique(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return sorted(
            {repr(sorted(item.items())): item for item in items}.values(), key=lambda item: repr(sorted(item.items()))
        )

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
    }


def _owned(function: dict[str, Any]) -> bool:
    """A function belongs to the unit that defines it in its own file or in a C file it includes
    (a unit such as a scheduler's build file includes several .c files); a header's does not."""
    return bool(function["main_file"] or function["file"].endswith(".c"))


def _expected_functions(linker: Linker) -> tuple[list[str], list[str]]:
    """Functions of the part's files that the image holds, from sources other than the root search:
    the image's own symbols (a global symbol, or a local one that follows the unit's ELF FILE
    symbol) whose name a part unit defines, and the contract graph's functions of those files
    that the image holds and C defines. The second list: contract functions of the part's files
    that the image holds but no C file defines (assembly)."""
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
    without_c: set[str] = set()
    for (source, name), node in sorted(linker.contracts.functions.items()):
        if (
            source in linker.part_files
            and (node.get("in_image") or node.get("addr"))
            and name in linker.image.functions
        ):
            definitions = [
                item
                for item in linker.by_name.get(name, [])
                if item.file == source or (item.tu == source and item.file.endswith(".c"))
            ]
            if definitions:
                expected.add(definitions[0].id)
            else:
                without_c.add(f"function:{name}")
    return sorted(expected), sorted(without_c)
