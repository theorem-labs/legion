"""Numbers over the graph: how the data structures group, how many function pairs share written
state with and without a call path, how many neighbours each owner has, and how the contract
graph's interfaces map onto the groups."""

from __future__ import annotations

import collections
import re
from pathlib import Path
from typing import Any

from . import structure
from .contracts import ContractGraph

WRITES = ("write", "alloc", "free")
CALLS = ("call", "call_indirect")
_TOP_LEVEL = re.compile(r"^(?:structure|class|def|abbrev|inductive|theorem|lemma|instance|namespace|end)\b")


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


def build(graph: dict[str, Any], contracts: ContractGraph, contract_source: Path | None) -> dict[str, Any]:
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
            "alloc or free) a data structure of a group the other accesses; "
            "groups with per-CPU state left out.",
        },
        "calls_sharing_state": {"call_sites": sum(sites for _, _, sites in call_sites), "sharing": sharing_sites},
        "owner_neighbors": _owner_neighbors(nodes, edges, functions),
        "interfaces": _interfaces(graph, contracts, contract_source, groups) if contracts.nodes else None,
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


def _interfaces(
    graph: dict[str, Any], contracts: ContractGraph, source: Path | None, groups: list[list[str]]
) -> dict[str, Any] | None:
    """Each interface the part's functions take, mapped to the groups whose C names its own declaration uses."""
    if source is None:
        return None
    by_id = {node["id"]: node for node in graph["nodes"]}
    contract_to_function = {
        node["contract"]: node["id"]
        for node in graph["nodes"]
        if node["kind"] == "function" and node.get("contract") and node.get("part")
    }
    taken: dict[str, set[str]] = collections.defaultdict(set)
    for edge in contracts.binds():
        function = contract_to_function.get(edge.get("src"))
        if function is not None:
            taken[edge["dst"].removeprefix("if:")].add(function)
    identifiers = {index: {by_id[member]["name"] for member in group} for index, group in enumerate(groups)}
    cache: dict[Path, list[str]] = {}
    summary: collections.Counter[str] = collections.Counter()
    mappings: list[dict[str, Any]] = []
    for interface in sorted(taken):
        node = contracts.nodes.get(f"if:{interface}")
        matched: set[int] = set()
        location = node.get("file", "") if node else ""
        if location and ":" in location:
            relative, line_text = location.rsplit(":", 1)
            path = source / relative
            if path not in cache:
                try:
                    cache[path] = path.read_text(errors="replace").splitlines()
                except OSError:
                    cache[path] = []
            lines = cache[path]
            start = int(line_text)
            end = next((index for index in range(start, len(lines)) if _TOP_LEVEL.match(lines[index])), len(lines))
            body = "\n".join(lines[start - 1 : end])
            for index, names in identifiers.items():
                if any(re.search(r"(?<![A-Za-z0-9_])" + re.escape(name) + r"(?![A-Za-z0-9_])", body) for name in names):
                    matched.add(index)
        kind = "one" if len(matched) == 1 else "several" if matched else "none"
        summary[kind] += 1
        mappings.append(
            {
                "interface": interface,
                "functions": sorted(taken[interface]),
                "groups": sorted(groups[index][0] for index in matched),
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
