"""One owner for each function, data structure and subsystem, and the topics each owner subscribes to.

An owner's key is its node's id. The topics follow the message table of the design (``node.<id>``
followed by ``needs``, ``callers``, ``callees``, ``users``, ``linked`` or ``neighbors``, and ``all``):

- every owner: its own ``needs``, ``all``, and the ``neighbors`` topic of each neighbour in the three
  layers (calls, accesses, structure links, and subsystem membership);
- a function owner: ``callers`` of each function it calls, ``callees`` of each function that calls
  it, and ``users`` of each data structure and subsystem it accesses;
- a data-structure or per-CPU owner: ``linked`` of each data structure a structure link joins to it,
  and of its subsystem;
- a subsystem owner: ``linked`` of each member.

``fact.<name>.users`` and ``concepts`` come from the concept registry, not from the graph.
"""

from __future__ import annotations

import collections
import re
from typing import Any

CALLS = ("call", "call_indirect")
LINKS = ("embeds", "embeds_base", "points_to", "member_of")
STATE = ("struct", "global", "percpu")
_UNSAFE = re.compile(r"[^A-Za-z0-9_:-]")


def token(node_id: str) -> str:
    """One subject token for a node id: characters other than letters, digits, ``_``, ``:`` and ``-``
    are percent-encoded, so ``global:f.counter`` stays one token."""
    return _UNSAFE.sub(lambda match: "".join(f"%{byte:02X}" for byte in match.group(0).encode()), node_id)


def relations(graph: dict[str, Any]) -> dict[str, dict[str, set[str]]]:
    kinds = {node["id"]: node["kind"] for node in graph["nodes"]}
    subsystem_of = {
        member: node["id"] for node in graph["nodes"] if node["kind"] == "subsystem" for member in node["members"]
    }
    rel: dict[str, dict[str, set[str]]] = collections.defaultdict(lambda: collections.defaultdict(set))
    for edge in graph["edges"]:
        source, target, kind = edge["source"], edge["target"], edge["kind"]
        if kind in CALLS:
            rel[source]["callees"].add(target)
            rel[target]["callers"].add(source)
        elif kind == "access":
            rel[source]["uses"].add(target)
            rel[target]["users"].add(source)
            subsystem = subsystem_of.get(target)
            if subsystem is not None:
                rel[source]["uses"].add(subsystem)
                rel[subsystem]["users"].add(source)
        elif (
            kind in LINKS and kinds.get(source) in STATE and kinds.get(target) in STATE and source != target
        ) or kind == "in_subsystem":
            rel[source]["linked"].add(target)
            rel[target]["linked"].add(source)
    for relation in rel.values():
        relation["neighbors"] = set().union(
            *(relation[name] for name in ("callees", "callers", "uses", "users", "linked"))
        )
    return rel


def eligible(graph: dict[str, Any], rel: dict[str, dict[str, set[str]]]) -> dict[str, str]:
    """The nodes that get an owner, with the owner's role."""
    result: dict[str, str] = {}
    for node in graph["nodes"]:
        identity = node["id"]
        if node["kind"] == "function" and node.get("part"):
            result[identity] = "function"
        elif node["kind"] == "subsystem":
            result[identity] = "subsystem"
        elif node["kind"] in STATE and (rel[identity]["users"] or rel[identity]["linked"]):
            result[identity] = "cross_cutting" if node["kind"] == "percpu" else "data_structure"
    return result


def build(graph: dict[str, Any], prefix: str) -> tuple[dict[str, Any], dict[str, Any]]:
    rel = relations(graph)
    roles = eligible(graph, rel)
    by_id = {node["id"]: node for node in graph["nodes"]}
    subsystem_of = {
        member: node["id"] for node in graph["nodes"] if node["kind"] == "subsystem" for member in node["members"]
    }
    owners = []
    topics: dict[str, list[str]] = {}

    def topic(node_id: str, kind: str) -> str:
        return f"{prefix}node.{token(node_id)}.{kind}"

    for identity, role in sorted(roles.items()):
        node = by_id[identity]
        relation = rel[identity]
        owners.append(
            {
                "id": identity,
                "role": role,
                "kind": node["kind"],
                "name": node["name"],
                "c_file": node.get("c_file"),
                "contract": node.get("contract"),
                "subsystem": subsystem_of.get(identity),
                "neighbors": len(relation["neighbors"]),
            }
        )
        subscribed = {topic(identity, "needs"), f"{prefix}all"}
        subscribed |= {topic(other, "neighbors") for other in relation["neighbors"]}
        if role == "function":
            subscribed |= {topic(callee, "callers") for callee in relation["callees"]}
            subscribed |= {topic(caller, "callees") for caller in relation["callers"]}
            subscribed |= {topic(used, "users") for used in relation["uses"]}
        else:
            subscribed |= {topic(other, "linked") for other in relation["linked"]}
        topics[identity] = sorted(subscribed)
    roles_count = collections.Counter(roles.values())
    return (
        {
            "format": "cgraph/owners/v1",
            "part": graph["part"],
            "counts": dict(sorted(roles_count.items())),
            "eligible": sorted(roles),
            "owners": owners,
        },
        {
            "format": "cgraph/topics/v1",
            "part": graph["part"],
            "prefix": prefix,
            "token_rule": "node id with every character outside [A-Za-z0-9_:-] percent-encoded",
            "owners": topics,
        },
    )
