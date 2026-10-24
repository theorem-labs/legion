"""Numbers over the graph: how the data structures group, how many function pairs share written
state with and without a call path, how many neighbours each owner has, and how the contract
graph's interfaces map onto the groups."""

from __future__ import annotations

import bisect
import collections
import hashlib
import json
import os
import re
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from . import structure
from .contracts import ContractGraph
from .errors import CacheError, InputError

WRITES = ("write", "allocate", "free")
CALLS = ("call", "call_indirect")
_TOP_LEVEL = re.compile(r"^(?:structure|class|def|abbrev|inductive|theorem|lemma|instance|namespace|end)\b")
# A maximal run of identifier characters: a C name made of them alone matches a declaration exactly
# where one of its runs equals the name.
_IDENTIFIER = re.compile(r"[A-Za-z0-9_]+")
_MODULE_FORMAT = "cgraph/interface-module/v1"


def _percentile(values: list[int], quantile: float) -> int:
    if not values:
        return 0
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(quantile * len(ordered)))]


def _median(values: list[int]) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    middle = len(ordered) // 2
    return float(ordered[middle]) if len(ordered) % 2 else (ordered[middle - 1] + ordered[middle]) / 2


def build(
    graph: dict[str, Any],
    contracts: ContractGraph,
    contract_source: Path | None,
    cache_directory: Path | None = None,
) -> dict[str, Any]:
    nodes, edges = graph["nodes"], graph["edges"]
    by_id = {node["id"]: node for node in nodes}
    functions = {
        node["id"] for node in nodes if node["kind"] == "function" and node.get("part") and node.get("image_addresses")
    }
    groups = structure.groups(nodes, edges)
    group_of = {member: index for index, group in enumerate(groups) for member in group}
    per_cpu = {
        index
        for index, group in enumerate(groups)
        if any(by_id[member]["kind"] == "percpu" or by_id[member].get("per_cpu") for member in group)
    }
    users: dict[int, set[str]] = collections.defaultdict(set)
    writers: dict[int, set[str]] = collections.defaultdict(set)
    for edge in edges:
        if edge["kind"] != "access" or edge["source"] not in functions or edge["target"] not in group_of:
            continue
        group = group_of[edge["target"]]
        users[group].add(edge["source"])
        if edge.get("access") in WRITES:
            writers[group].add(edge["source"])
    adjacency: dict[str, set[str]] = collections.defaultdict(set)
    for edge in edges:
        if edge["kind"] in CALLS and edge["source"] in functions and edge["target"] in functions:
            adjacency[edge["source"]].add(edge["target"])
    reach: dict[str, set[str]] = {}
    for start in sorted(functions):
        seen: set[str] = set()
        stack = [start]
        while stack:
            for nxt in adjacency[stack.pop()]:
                if nxt not in seen:
                    seen.add(nxt)
                    stack.append(nxt)
        reach[start] = seen
    pairs: set[tuple[str, str]] = set()
    for group, group_writers in writers.items():
        if group in per_cpu:
            continue
        for writer in group_writers:
            for user in users[group]:
                if user != writer:
                    pairs.add((min(user, writer), max(user, writer)))
    with_path = sum(1 for left, right in pairs if right in reach[left] or left in reach[right])
    shared = collections.defaultdict(set)
    for group, group_users in users.items():
        if group not in per_cpu:
            for function in group_users:
                shared[function].add(group)
    call_sites = [
        (edge["source"], edge["target"], len([item for item in edge["provenance"] if item["source"] == "c"]))
        for edge in edges
        if edge["kind"] == "call"
        and not edge.get("image_only")
        and edge["source"] in functions
        and edge["target"] in functions
    ]
    sharing_sites = sum(sites for left, right, sites in call_sites if shared[left] & shared[right])
    subsystems = []
    for node in nodes:
        if node["kind"] != "subsystem":
            continue
        group = group_of[node["members"][0]]
        subsystems.append(
            {"id": node["id"], "members": node["members"], "users": len(users[group]), "writers": len(writers[group])}
        )
    subsystems.sort(key=lambda item: (-item["users"], item["id"]))
    return {
        "format": "cgraph/measures/v1",
        "part": graph["part"],
        "functions": len(functions),
        "groups": {
            "total": len(groups),
            "subsystems": sum(1 for group in groups if len(group) > 1),
            "singletons": sum(1 for group in groups if len(group) == 1),
        },
        "subsystems_by_users": subsystems[:20],
        "pairs_sharing_written_state": {
            "pairs": len(pairs),
            "with_call_path": with_path,
            "share_with_call_path": round(with_path / len(pairs), 4) if pairs else 0.0,
            "method": "Pairs of image functions of the part where one writes (write, "
            "allocate or free) a data structure of a group the other accesses; "
            "groups with per-CPU state left out.",
        },
        "calls_sharing_state": {"call_sites": sum(sites for _, _, sites in call_sites), "sharing": sharing_sites},
        "owner_neighbors": _owner_neighbors(nodes, edges, functions),
        "interfaces": _interfaces(graph, contracts, contract_source, groups, cache_directory)
        if contracts.nodes
        else None,
    }


def _owner_neighbors(nodes: list[dict[str, Any]], edges: list[dict[str, Any]], functions: set[str]) -> dict[str, Any]:
    """Owners of functions and data structures (structs and globals) joined by calls between part
    functions, accesses of part functions and structure links; the largest owners by neighbours."""
    data = {node["id"] for node in nodes if node["kind"] in ("struct", "global")}
    neighbours: dict[str, set[str]] = collections.defaultdict(set)
    for edge in edges:
        source, target = edge["source"], edge["target"]
        joined = (
            (edge["kind"] in CALLS and source in functions and target in functions)
            or (edge["kind"] == "access" and source in functions and target in data)
            or (
                edge["kind"] in ("embeds", "embeds_base", "member_of", "points_to")
                and source in data
                and target in data
                and source != target
            )
        )
        if joined:
            neighbours[source].add(target)
            neighbours[target].add(source)
    owners = functions | {identity for identity in data if neighbours[identity]}
    counts = [len(neighbours[owner]) for owner in owners]
    top = sorted(((len(neighbours[owner]), owner) for owner in owners), key=lambda item: (-item[0], item[1]))[:10]
    return {
        "owners": len(owners),
        "functions": len(functions),
        "data_structures": len(owners - functions),
        "median": _median(counts),
        "p90": _percentile(counts, 0.9),
        "max": max(counts, default=0),
        "largest": [{"owner": owner, "neighbors": count} for count, owner in top],
        "owner_pairs": sum(len(neighbours[owner] & owners) for owner in owners) // 2,
    }


def _parse_module(text: str) -> dict[str, Any]:
    """The top-level lines of a declaring module and the identifier runs of each of its lines."""
    lines = text.splitlines()
    return {
        "format": _MODULE_FORMAT,
        "top_level": [index for index, line in enumerate(lines) if _TOP_LEVEL.match(line)],
        "tokens": [sorted(set(_IDENTIFIER.findall(line))) for line in lines],
    }


@dataclass
class _Module:
    lines: list[str]
    top_level: list[int]
    tokens: list[list[str]]


def _valid_module(value: Any, lines: int) -> bool:
    return (
        isinstance(value, dict)
        and value.get("format") == _MODULE_FORMAT
        and isinstance(value.get("top_level"), list)
        and all(isinstance(index, int) and 0 <= index < lines for index in value["top_level"])
        and isinstance(value.get("tokens"), list)
        and len(value["tokens"]) == lines
        and all(isinstance(line, list) and all(isinstance(token, str) for token in line) for line in value["tokens"])
    )


class _Modules:
    """Declaring modules, each read once per run; their parse is kept under ``<cache>/interfaces``,
    keyed by the module's contents and the parse rules, so only a changed module is parsed again."""

    def __init__(self, source: Path, cache_directory: Path | None) -> None:
        self.source = source
        self.directory = cache_directory / "interfaces" if cache_directory is not None else None
        self.loaded: dict[str, _Module] = {}

    def get(self, relative: str, interface: str) -> _Module:
        module = self.loaded.get(relative)
        if module is not None:
            return module
        path = self.source / relative
        try:
            content = path.read_bytes()
        except OSError as error:
            raise InputError(
                f"interface {interface}: cannot read its declaration {relative} under {self.source}: {error}"
            ) from error
        text = content.decode("utf-8", errors="replace")
        lines = text.splitlines()
        # The key is the module's contents and the rules that parse it, so a change to either parses again.
        rules = json.dumps([_MODULE_FORMAT, _TOP_LEVEL.pattern, _IDENTIFIER.pattern]).encode()
        parsed = self._cached(hashlib.sha256(rules + b"\0" + content).hexdigest(), text, len(lines))
        module = _Module(lines, parsed["top_level"], parsed["tokens"])
        self.loaded[relative] = module
        return module

    def _cached(self, digest: str, text: str, lines: int) -> dict[str, Any]:
        if self.directory is None:
            return _parse_module(text)
        path = self.directory / f"{digest}.json"
        if path.exists():
            try:
                value = json.loads(path.read_text())
            except (OSError, json.JSONDecodeError) as error:
                raise self._error(path) from error
            if not _valid_module(value, lines):
                raise self._error(path)
            return dict(value)
        parsed = _parse_module(text)
        temporary: Path | None = None
        try:
            self.directory.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_name = tempfile.mkstemp(prefix=f".{digest}.", suffix=".tmp", dir=self.directory)
            temporary = Path(temporary_name)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(parsed, stream, sort_keys=True, separators=(",", ":"))
            os.replace(temporary, path)
        except OSError as error:
            if temporary is not None:
                temporary.unlink(missing_ok=True)
            raise CacheError(f"cannot write interface cache entry {path}: {error}") from error
        return parsed

    def _error(self, path: Path) -> CacheError:
        assert self.directory is not None
        return CacheError(
            f"interface cache entry {path} is malformed; remove the cache directory {self.directory.parent}"
        )


class _NameIndex:
    """The groups each C name of a group belongs to. A name made only of identifier characters is a
    dictionary key; any other name keeps one compiled whole-name pattern, tried only where every
    identifier run of the name occurs in the declaration."""

    def __init__(self, identifiers: dict[int, set[str]]) -> None:
        self.plain: dict[str, set[int]] = collections.defaultdict(set)
        others: dict[str, set[int]] = collections.defaultdict(set)
        for position, names in identifiers.items():
            for name in names:
                if _IDENTIFIER.fullmatch(name):
                    self.plain[name].add(position)
                else:
                    others[name].add(position)
        self.others = [
            (
                re.compile(r"(?<![A-Za-z0-9_])" + re.escape(name) + r"(?![A-Za-z0-9_])"),
                frozenset(_IDENTIFIER.findall(name)),
                frozenset(positions),
            )
            for name, positions in sorted(others.items())
        ]

    def groups(self, tokens: set[str], body: list[str]) -> set[int]:
        matched: set[int] = set()
        for token in tokens:
            plain = self.plain.get(token)
            if plain is not None:
                matched |= plain
        text: str | None = None
        for pattern, runs, positions in self.others:
            if positions <= matched or not runs <= tokens:
                continue
            if text is None:
                text = "\n".join(body)
            if pattern.search(text):
                matched |= positions
        return matched


def _interfaces(
    graph: dict[str, Any],
    contracts: ContractGraph,
    source: Path | None,
    groups: list[list[str]],
    cache_directory: Path | None = None,
) -> dict[str, Any] | None:
    """Each interface the part's functions take, mapped to the groups whose C names its own declaration uses."""
    if source is None:
        return None
    by_id = {node["id"]: node for node in graph["nodes"]}
    contract_to_function: dict[str, str] = {}
    for node in graph["nodes"]:
        contract = node.get("contract") if node["kind"] == "function" and node.get("part") else None
        for contract_id in [contract] if isinstance(contract, str) else contract or []:
            contract_to_function[contract_id] = node["id"]
    taken: dict[str, set[str]] = collections.defaultdict(set)
    for edge in contracts.binds():
        function = contract_to_function.get(str(edge.get("src")))
        if function is not None:
            taken[edge["dst"].removeprefix("if:")].add(function)
    index = _NameIndex({position: {by_id[member]["name"] for member in group} for position, group in enumerate(groups)})
    modules = _Modules(source, cache_directory)
    summary: collections.Counter[str] = collections.Counter()
    mappings: list[dict[str, Any]] = []
    for interface in sorted(taken):
        node = contracts.nodes.get(f"if:{interface}")
        matched: set[int] = set()
        location = node.get("file", "") if node else ""
        if location and ":" in location:
            relative, line_text = location.rsplit(":", 1)
            if not line_text.isdigit() or int(line_text) < 1:
                raise InputError(f"interface {interface}: declaration {location} has no positive line number")
            module = modules.get(relative, interface)
            start = int(line_text)
            following = bisect.bisect_left(module.top_level, start)
            end = module.top_level[following] if following < len(module.top_level) else len(module.lines)
            tokens: set[str] = set().union(*module.tokens[start - 1 : end])
            matched = index.groups(tokens, module.lines[start - 1 : end])
        kind = "one" if len(matched) == 1 else "several" if matched else "none"
        summary[kind] += 1
        mappings.append(
            {
                "interface": interface,
                "functions": sorted(taken[interface]),
                "groups": sorted(groups[position][0] for position in matched),
                "classification": kind,
                "declaration": location,
            }
        )
    return {
        "interfaces": len(mappings),
        "exactly_one": summary["one"],
        "several": summary["several"],
        "none": summary["none"],
        "mappings": mappings,
        "method": "Exact C names (globals, structs) of each group inside the interface's own declaration at the "
        "contract graph's file:line.",
    }
