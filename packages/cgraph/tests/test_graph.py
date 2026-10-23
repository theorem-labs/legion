"""The linked graph: folding, ids, structure links, subsystems and provenance."""

from __future__ import annotations

from typing import Any

from conftest import edges, node

from cgraph import link


def targets(graph: dict[str, Any], kind: str, source: str, **fields: Any) -> set[str]:
    return {
        edge["target"]
        for edge in edges(graph, kind, source)
        if all(edge.get(key) == value for key, value in fields.items())
    }


def test_folded_helpers_lend_their_accesses_to_the_image_caller(graph: dict[str, Any]) -> None:
    # hash_box, write_seqlk and hash_insert are inlined into box_new in the image.
    assert "global:table_lock" in targets(graph, "access", "function:box_new", access="lock")
    assert "global:table_lock" in targets(graph, "access", "function:box_new", access="unlock")
    assert targets(graph, "call", "function:box_new") == {
        "function:obj_alloc",
        "function:spin_lock",
        "function:spin_unlock",
        "function:box_size",
    }
    access = next(
        edge
        for edge in edges(graph, "access", "function:box_new")
        if edge["target"] == "global:table_lock" and edge["access"] == "lock"
    )
    assert access["provenance"][0]["via"][0] == "function:hash_box"


def test_a_header_helper_the_image_calls_stays_a_call_and_is_a_part_function(graph: dict[str, Any]) -> None:
    helper = node(graph, "function:box_size")
    assert helper["part"] and helper["c_file"] == "include/mini.h"
    assert "function:box_size" in targets(graph, "call", "function:box_new", image="direct")


def test_guard_takes_and_releases_the_lock_its_constructor_names(graph: dict[str, Any]) -> None:
    assert "global:tree_sem" in targets(graph, "access", "function:box_tree_add", access="lock")
    assert "function:up_write" in targets(graph, "call", "function:box_tree_add", image="tail")


def test_unselected_generic_association_is_not_an_access(graph: dict[str, Any]) -> None:
    found = targets(graph, "access", "function:bag_count")
    assert "global:box_table" not in found and "global:all_bags" not in found
    assert {item["unit"] for item in graph["analysis"]["generic_unconfirmed"]} == {"box_table", "all_bags"}


def test_two_definitions_of_one_name_get_file_ids_and_their_own_image_copy(graph: dict[str, Any]) -> None:
    store = node(graph, "function:helper_dup@src/store.c")
    bags = node(graph, "function:helper_dup@src/bags.c")
    assert store["image_addresses"] != bags["image_addresses"]
    assert targets(graph, "call", "function:store_dup") == {"function:helper_dup@src/store.c"}
    assert "global:box_count" in targets(graph, "access", "function:helper_dup@src/store.c")


def test_per_cpu_state_has_its_own_prefix(graph: dict[str, Any]) -> None:
    assert node(graph, "percpu:current_box")["kind"] == "percpu"


def test_shared_base_and_value_types_do_not_join(graph: dict[str, Any]) -> None:
    bases = {(edge["source"], edge["target"]): edge["base_rule"] for edge in edges(graph, "embeds_base")}
    assert bases == {
        ("struct:bag", "struct:base"): "shared_base",
        ("struct:box", "struct:base"): "shared_base",
        ("struct:box", "struct:uid_v"): "value_type",
    }
    subsystems = {item["id"]: set(item["members"]) for item in graph["nodes"] if item["kind"] == "subsystem"}
    assert not any({"struct:box", "struct:bag"} <= members for members in subsystems.values())


def test_subsystem_is_named_from_its_largest_struct(graph: dict[str, Any]) -> None:
    subsystems = {item["id"]: item["members"] for item in graph["nodes"] if item["kind"] == "subsystem"}
    assert subsystems == {
        "subsystem:crate": ["global:box_table", "struct:box", "struct:crate"],
        "subsystem:bag": ["global:all_bags", "struct:bag", "struct:item"],
    }


def test_hash_helper_return_joins_the_table(graph: dict[str, Any]) -> None:
    member = next(edge for edge in edges(graph, "member_of") if edge["source"] == "struct:box")
    assert member["target"] == "global:box_table"
    assert member["resolution"] == ["insertion_call", "helper_return"] and member["membership"] == "hash"


def test_guard_locks_of_a_subsystem(graph: dict[str, Any]) -> None:
    crate = node(graph, "subsystem:crate")
    assert {lock["lock"] for lock in crate["guard_locks"]} == {"global:table_lock", "global:tree_sem"}


def test_callback_table_edge_needs_the_c_initializer(graph: dict[str, Any]) -> None:
    (edge,) = [edge for edge in edges(graph, "call_indirect", "function:run_get")]
    assert edge["target"] == "function:box_get" and edge["slot"] == "box_ops.get"
    assert edge["resolution"] == "callback_table"


def test_every_node_and_edge_has_provenance(graph: dict[str, Any]) -> None:
    assert all(item["provenance"] for item in graph["nodes"])
    assert all(item["provenance"] for item in graph["edges"])
    sources = {entry["source"] for item in graph["nodes"] + graph["edges"] for entry in item["provenance"]}
    assert sources == {"c", "image", "contract_graph"}


def test_a_lock_field_of_a_global_is_that_global(graph: dict[str, Any]) -> None:
    assert not any(
        item["id"].startswith("struct:seqlk") or item["id"].startswith("struct:spin") for item in graph["nodes"]
    )


def fake_unit(name: str, calls: list[dict[str, Any]], accesses: list[dict[str, Any]]) -> dict[str, Any]:
    events = {
        "calls": calls,
        "accesses": accesses,
        "unevaluated": [],
        "list_calls": [],
        "indirect": [],
        "function_pointers": [],
        "field_accesses": [],
    }
    return {
        "name": name,
        "file": "a.c",
        "line": len(name),
        "static": True,
        "main_file": True,
        "returns": None,
        "events": events,
    }


def test_fold_only_where_the_image_has_no_direct_call() -> None:
    """The fold rule alone: inlined where the caller's image code does not call the helper."""
    where = [{"source": "c", "file": "a.c", "line": 1}]
    helper_access = {
        "unit": {"kind": "global", "name": "state", "def": None, "field": None, "lock": False},
        "access": "read",
        "field": None,
        "provenance": where,
    }
    unit = {
        "functions": {
            "caller": fake_unit(
                "caller",
                [
                    {"callee": "inlined", "arguments": [], "provenance": where, "visible": True},
                    {"callee": "called", "arguments": [], "provenance": where, "visible": True},
                    {"callee": "external", "arguments": [], "provenance": where, "visible": False},
                ],
                [],
            ),
            "inlined": fake_unit("inlined", [], [helper_access]),
            "called": fake_unit("called", [], [helper_access]),
        },
        "records": {},
    }
    linker = link.Linker.__new__(link.Linker)
    linker.tus = {"a.c": unit}
    linker.definitions = {}
    linker.by_name = {}
    for name, function in unit["functions"].items():
        definition = link.Definition(name, "a.c", function["line"], True, "a.c", f"function:{name}")
        linker.definitions[(name, "a.c", function["line"])] = definition
        linker.by_name[name] = [definition]
    linker.effective_cache = {}
    linker.callee_cache = {("caller", "a.c", 6): {"called"}}
    result = linker.effective("a.c", "caller", linker.definitions[("caller", "a.c", 6)])
    assert [call["callee"] for call in result.calls] == ["called", "external"]
    assert [fold["helper"] for fold in result.folds] == ["function:inlined"]
    assert [access["unit"]["name"] for access in result.accesses] == ["state"]


def test_parameter_accesses_bind_to_the_callers_argument() -> None:
    where = [{"source": "c", "file": "a.c", "line": 1}]
    lock = {"unit": None, "param": 0, "access": "lock", "field": None, "provenance": where}
    child = link.Effective(accesses=[lock])
    argument = {"unit": {"kind": "global", "name": "big_lock", "def": None, "field": None, "lock": True}, "param": None}
    assert [item["unit"]["name"] for item in link._bind(child, [argument]).accesses] == ["big_lock"]
    assert link._bind(child, [{"unit": None, "param": 2}]).accesses[0]["param"] == 2
    assert link._bind(child, [{"unit": None, "param": None}]).unbound == 1
