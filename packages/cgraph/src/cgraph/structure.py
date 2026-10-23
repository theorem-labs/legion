"""Structure links between data structures, and the subsystems they form.

Embedding and list or hash membership join data structures into groups; a group of two or more is
a subsystem, named from its largest struct. Two kinds of embedded type do not join: a value (a
record with no pointer at any depth, or one the configuration names), and a base that many types
embed (at least ``base_type_min_containers`` of them). Pointers are links but do not join.
"""

from __future__ import annotations

import collections
from typing import TYPE_CHECKING, Any

from . import jsonio
from .config import Vocabulary

if TYPE_CHECKING:
    from .link import GraphBuilder, Linker

JOINING = ("embeds", "member_of")
STATE_KINDS = ("struct", "global", "percpu")


def _record_key(node: dict[str, Any]) -> str:
    return f"{node['name']}@{node['c_file']}:{node['line']}"


def layout_edges(
    builder: GraphBuilder, records: dict[str, dict[str, Any]], vocabulary: Vocabulary
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Embedding and pointer edges from clang's record layouts, and the input records without nodes."""
    struct_ids = {_record_key(node): node_id for node_id, node in builder.nodes.items() if node["kind"] == "struct"}
    candidates = []
    containers: dict[str, set[str]] = collections.defaultdict(set)
    drops = []
    for key, parent_id in sorted(struct_ids.items()):
        record = records.get(key)
        if record is None:
            continue
        for field in record["fields"]:
            if "record" not in field:
                continue
            child_id = struct_ids.get(f"{field['record']}@{field['record_def']}")
            if child_id is None:
                drops.append(
                    {
                        "kind": "embedding",
                        "reason": "record_not_a_node",
                        "function": None,
                        "target": f"struct:{field['record']}",
                        "detail": {"parent": parent_id, "field": field["name"], "record_def": field["record_def"]},
                        "provenance": [{"source": "c", "file": record["def"].rsplit(":", 1)[0], "line": field["line"]}],
                    }
                )
                continue
            candidates.append((parent_id, child_id, record, field))
            if not field["pointer"]:
                containers[child_id].add(parent_id)
    excluded: dict[str, str] = {}
    for child_id, parents in containers.items():
        name = builder.nodes[child_id]["name"]
        if name in vocabulary.value_types or builder.nodes[child_id].get("plain"):
            excluded[child_id] = "value_type"
        elif name in vocabulary.kit_types or name in vocabulary.lock_types:
            excluded[child_id] = "kit_type"
        elif len(parents) >= vocabulary.base_type_min_containers:
            excluded[child_id] = "shared_base"
    for parent_id, child_id, record, field in candidates:
        file = record["def"].rsplit(":", 1)[0]
        provenance = [{"source": "c", "file": file, "line": field["line"]}]
        if field["pointer"]:
            kind = "points_to"
        elif child_id in excluded:
            kind = "embeds_base"
        else:
            kind = "embeds"
        builder.edge(
            kind,
            parent_id,
            child_id,
            provenance,
            field=field["name"],
            layout={"offset": field["offset"], "size": field["size"]},
            base_rule=excluded.get(child_id),
        )
    return (
        [
            {"type": child_id, "reason": reason, "containers": sorted(containers[child_id])}
            for child_id, reason in sorted(excluded.items())
        ],
        drops,
    )


def membership_edges(
    builder: GraphBuilder, linker: Linker, list_calls: list[tuple[str, dict[str, Any]]]
) -> list[dict[str, Any]]:
    """``member_of`` from list and hash insertions, with every unresolved input recorded."""
    drops = []
    for function, call in list_calls:
        arguments = []
        resolution = ["insertion_call"]
        for unit, helper in zip(call["arguments"], call["argument_helpers"], strict=True):
            if helper is not None:
                returned = linker.tus[call["tu"]]["functions"].get(helper, {}).get("returns")
                if returned is not None:
                    unit = returned
                    resolution.append("helper_return")
            if unit is not None:
                arguments.append(unit)
        if len(arguments) < 2:
            drops.append(
                {
                    "kind": "membership",
                    "reason": "arguments_unresolved",
                    "function": function,
                    "target": None,
                    "detail": {"operation": call["operation"], "membership": call["membership"]},
                    "provenance": call["provenance"],
                }
            )
            continue
        element, container = arguments[0], arguments[-1]
        source, target = linker.unit_id(element), linker.unit_id(container)
        if source is None or target is None or source not in builder.nodes or target not in builder.nodes:
            drops.append(
                {
                    "kind": "membership",
                    "reason": "end_not_a_node",
                    "function": function,
                    "target": target,
                    "detail": {
                        "element": source,
                        "container": target,
                        "operation": call["operation"],
                        "membership": call["membership"],
                    },
                    "provenance": call["provenance"],
                }
            )
            continue
        builder.edge(
            "member_of",
            source,
            target,
            call["provenance"],
            membership=call["membership"],
            operation=call["operation"],
            resolution=resolution,
            function=function,
            element_field=element.get("field"),
            container_field=container.get("field"),
        )
    return drops


def groups(nodes: list[dict[str, Any]], edges: list[dict[str, Any]]) -> list[list[str]]:
    """Every data structure in one group with all the data structures that embedding or membership join it to."""
    parent = {node["id"]: node["id"] for node in nodes if node["kind"] in STATE_KINDS}

    def find(value: str) -> str:
        while parent[value] != value:
            parent[value] = parent[parent[value]]
            value = parent[value]
        return value

    for edge in edges:
        if edge["kind"] in JOINING and edge["source"] in parent and edge["target"] in parent:
            left, right = find(edge["source"]), find(edge["target"])
            if left != right:
                parent[max(left, right)] = min(left, right)
    members: dict[str, list[str]] = collections.defaultdict(list)
    for identity in parent:
        members[find(identity)].append(identity)
    return sorted(sorted(values) for values in members.values())


def subsystems(nodes: list[dict[str, Any]], edges: list[dict[str, Any]]) -> list[dict[str, Any]]:
    by_id = {node["id"]: node for node in nodes}
    result = []
    for group in groups(nodes, edges):
        if len(group) < 2:
            continue
        members = set(group)

        def size(identity: str) -> tuple[int, int, str]:
            node = by_id[identity]
            if node["kind"] == "struct":
                return (1, node.get("size") or 0, node["name"])
            return (0, node.get("image_size") or 0, node["name"])

        # The largest struct by clang's size; a group with no struct takes its largest global.
        largest = min(group, key=lambda identity: (-size(identity)[0], -size(identity)[1], size(identity)[2]))
        member_provenance: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
        for edge in edges:
            if edge["kind"] in JOINING and edge["source"] in members and edge["target"] in members:
                member_provenance[edge["source"]] += edge["provenance"]
                member_provenance[edge["target"]] += edge["provenance"]
        provenance = sorted(
            {jsonio.canonical(item): item for entries in member_provenance.values() for item in entries}.values(),
            key=jsonio.canonical,
        )
        name = by_id[largest]["name"]
        result.append(
            {
                "id": f"subsystem:{name}",
                "name": name,
                "members": group,
                "largest": largest,
                "provenance": provenance,
                "member_provenance": dict(member_provenance),
                "guard_locks": [],
            }
        )
    return sorted(result, key=lambda item: item["id"])


def attach_guard_locks(found: list[dict[str, Any]], edges: list[dict[str, Any]]) -> None:
    """A lock guards a subsystem where a function that writes a member, or a direct caller of it, takes it."""
    member_of = {member: item for item in found for member in item["members"]}
    held: dict[str, set[tuple[str, str | None]]] = collections.defaultdict(set)
    callers: dict[str, set[str]] = collections.defaultdict(set)
    writes: list[dict[str, Any]] = []
    for edge in edges:
        if edge["kind"] == "access" and edge.get("access") == "lock":
            held[edge["source"]].add((edge["target"], edge.get("field")))
        elif edge["kind"] == "access" and edge.get("access") == "write":
            writes.append(edge)
        elif edge["kind"] == "call":
            callers[edge["target"]].add(edge["source"])
    locks: dict[str, dict[tuple[str, str | None], dict[str, Any]]] = collections.defaultdict(dict)
    for write in writes:
        subsystem = member_of.get(write["target"])
        if subsystem is None:
            continue
        holders = [(write["source"], "same_function")] + [
            (caller, "direct_caller") for caller in sorted(callers[write["source"]])
        ]
        for holder, method in holders:
            for lock, field in held.get(holder, ()):
                entry = locks[subsystem["id"]].setdefault(
                    (lock, field), {"lock": lock, "field": field, "methods": set(), "writers": set()}
                )
                entry["methods"].add(method)
                entry["writers"].add(write["source"])
    for item in found:
        item["guard_locks"] = [
            {
                "lock": entry["lock"],
                "field": entry["field"],
                "methods": sorted(entry["methods"]),
                "writers": len(entry["writers"]),
            }
            for _, entry in sorted(locks[item["id"]].items(), key=lambda pair: (pair[0][0], pair[0][1] or ""))
        ]
