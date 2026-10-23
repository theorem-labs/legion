"""Owners and the topics each subscribes to."""

from __future__ import annotations

import collections
from pathlib import Path
from typing import Any

from conftest import read

from cgraph import checks
from cgraph.owners import token

PREFIX = "notifications.example."


def topic(node_id: str, kind: str) -> str:
    return f"{PREFIX}node.{token(node_id)}.{kind}"


def test_one_owner_for_each_function_data_structure_and_subsystem(built: Path) -> None:
    owners = read(built, "owners.json")
    roles = {owner["id"]: owner["role"] for owner in owners["owners"]}
    assert collections.Counter(owner["id"] for owner in owners["owners"]).most_common(1)[0][1] == 1
    assert roles["function:box_new"] == "function"
    assert roles["struct:box"] == "data_structure" and roles["global:box_table"] == "data_structure"
    assert roles["subsystem:crate"] == "subsystem"
    assert roles["percpu:current_box"] == "cross_cutting"
    assert "function:obj_alloc" not in roles  # not a function of the part
    graph = read(built, "graph.json")
    part = {item["id"] for item in graph["nodes"] if item["kind"] == "function" and item.get("part")}
    assert part <= set(roles)


def test_owner_key_is_the_node_id_and_carries_the_contract(built: Path) -> None:
    owners = {owner["id"]: owner for owner in read(built, "owners.json")["owners"]}
    assert owners["function:box_new"]["contract"] == "Prog/src/store/BoxNew"
    assert owners["struct:box"]["subsystem"] == "subsystem:crate"


def test_function_owner_topics(built: Path) -> None:
    topics = set(read(built, "topics.json")["owners"]["function:box_new"])
    assert {topic("function:box_new", "needs"), f"{PREFIX}all"} <= topics
    assert topic("function:box_size", "callers") in topics
    assert topic("struct:box", "users") in topics and topic("subsystem:crate", "users") in topics
    assert topic("global:table_lock", "users") in topics
    assert topic("struct:box", "neighbors") in topics and topic("function:box_size", "neighbors") in topics
    assert not any(item.endswith(".linked") for item in topics)
    callee = set(read(built, "topics.json")["owners"]["function:box_size"])
    assert topic("function:box_new", "callees") in callee


def test_data_structure_and_subsystem_topics(built: Path) -> None:
    owners = read(built, "topics.json")["owners"]
    box = set(owners["struct:box"])
    assert {
        topic("struct:crate", "linked"),
        topic("global:box_table", "linked"),
        topic("struct:item", "linked"),
        topic("struct:base", "linked"),
        topic("subsystem:crate", "linked"),
    } <= box
    assert topic("function:box_new", "neighbors") in box
    assert not any(item.endswith((".callers", ".callees", ".users")) for item in box)
    crate = set(owners["subsystem:crate"])
    assert {topic(member, "linked") for member in ("struct:box", "struct:crate", "global:box_table")} <= crate


def test_node_with_a_dot_is_one_subject_token(built: Path) -> None:
    assert token("global:next_id.counter") == "global:next_id%2Ecounter"
    assert token("function:helper_dup@src/store.c") == "function:helper_dup%40src%2Fstore%2Ecc"[:-1]
    owners = read(built, "topics.json")["owners"]
    assert topic("global:next_id.counter", "users") in owners["function:box_new"]
    assert all(checks.valid_subject(item) for items in owners.values() for item in items)


def test_hard_check_fails_on_a_missing_owner(built: Path) -> None:
    graph: dict[str, Any] = read(built, "graph.json")
    owners = read(built, "owners.json")
    topics = read(built, "topics.json")
    structure = checks.structure(graph)
    owners["owners"] = [owner for owner in owners["owners"] if owner["id"] != "struct:box"]
    failed = {item["name"] for item in checks.hard(graph, owners, topics, structure) if not item["ok"]}
    assert failed == {"one owner for each function, data structure and subsystem"}
