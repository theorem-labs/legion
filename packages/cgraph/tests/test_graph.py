"""The linked graph: folding, ids, structure links, subsystems and provenance."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from conftest import FIXTURE, build, edges, node

from cgraph import clangfacts, commands, config, contracts, image, jsonio, link, structure


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
        "function:box_size@include/mini.h",
    }
    access = next(
        edge
        for edge in edges(graph, "access", "function:box_new")
        if edge["target"] == "global:table_lock" and edge["access"] == "lock"
    )
    assert access["provenance"][0]["via"][0] == "function:hash_box@src/store.c"


def test_a_header_helper_the_image_calls_stays_a_call_and_is_a_part_function(graph: dict[str, Any]) -> None:
    helper = node(graph, "function:box_size@include/mini.h")
    assert helper["part"] and helper["c_file"] == "include/mini.h"
    assert "function:box_size@include/mini.h" in targets(graph, "call", "function:box_new", image="direct")


def test_guard_takes_and_releases_the_lock_its_constructor_names(graph: dict[str, Any]) -> None:
    assert "global:tree_sem" in targets(graph, "access", "function:box_tree_add", access="lock")
    assert "function:up_write" in targets(graph, "call", "function:box_tree_add", image="tail")


def test_only_the_selected_generic_association_is_code(graph: dict[str, Any]) -> None:
    """The _Generic sits in a macro; its association types are read from the macro body."""
    found = targets(graph, "access", "function:bag_count")
    assert "global:all_bags" in found and "global:box_table" not in found
    calls = targets(graph, "call", "function:box_scoped")
    assert "function:box_weight" in calls and "function:bag_weight" not in calls
    fields = {(edge["target"], edge.get("field")) for edge in edges(graph, "access", "function:box_scoped")}
    assert ("struct:box", "size") in fields and ("struct:bag", "n") not in fields
    assert graph["analysis"]["generic_selections"] == {"selected": 3, "ambiguous": 0}


def test_cleanup_attribute_locals_call_their_destructor(graph: dict[str, Any]) -> None:
    """LOCK_TREE's __cleanup(tree_unlock) and __free(obj) run at scope exit, as the image shows."""
    calls = {edge["target"]: edge for edge in edges(graph, "call", "function:box_scoped")}
    assert calls["function:tree_unlock"]["image"] == "direct"
    assert calls["function:obj_free"]["provenance"][0]["via"] == ["function:__free_obj@include/mini.h"]
    accesses = {(edge["target"], edge["access"]) for edge in edges(graph, "access", "function:box_scoped")}
    assert ("struct:box", "free") in accesses


def test_strong_definition_is_the_node_and_the_weak_one_is_recorded(graph: dict[str, Any]) -> None:
    hook = node(graph, "function:arch_hook")
    assert hook["c_file"] == "src/bags.c" and hook["weak_definitions"] == ["src/store.c:88"]
    assert targets(graph, "call", "function:use_hook") == {"function:arch_hook", "function:stub_open"}
    assert graph["analysis"]["alternate_definitions"]["function:arch_hook"]["others"] == [
        {"definition": "src/store.c:88", "weak": True}
    ]


def test_ids_follow_linkage_not_collisions(graph: dict[str, Any]) -> None:
    """A static is @file even when nothing else has its name; an external function never is."""
    ids = {item["id"] for item in graph["nodes"]}
    expected = {"function:box_get@src/store.c", "function:visit@src/bags.c", "global:box_count@src/store.c",
                "global:next_id.counter@src/store.c", "function:box_new", "global:box_table"}  # fmt: skip
    assert expected <= ids


def test_two_definitions_of_one_name_get_file_ids_and_their_own_image_copy(graph: dict[str, Any]) -> None:
    store = node(graph, "function:helper_dup@src/store.c")
    bags = node(graph, "function:helper_dup@src/bags.c")
    assert store["image_addresses"] != bags["image_addresses"]
    assert targets(graph, "call", "function:store_dup") == {"function:helper_dup@src/store.c"}
    assert "global:box_count@src/store.c" in targets(graph, "access", "function:helper_dup@src/store.c")


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
    assert edge["target"] == "function:box_get@src/store.c" and edge["slot"] == "box_ops.get"
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
    linker.symbol_cache = {("caller", "a.c", 6): []}
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
    assert link._bind(child, [{"unit": None, "param": None}]).unbound == [lock]


def test_a_weak_stub_a_macro_generates_gives_way_to_the_strong_definition(graph: dict[str, Any]) -> None:
    stub = node(graph, "function:stub_open")
    assert stub["c_file"] == "src/bags.c" and stub["weak_definitions"]
    # The strong definition's body is the node's: it calls bag_weight, which the compiler folded.
    folds = {fold["helper"] for fold in graph["analysis"]["folds"] if fold["image_caller"] == "function:stub_open"}
    assert folds == {"function:bag_weight"}


def _reverse_keys(value: Any) -> Any:
    if isinstance(value, dict):
        return {key: _reverse_keys(item) for key, item in reversed(list(value.items()))}
    if isinstance(value, list):
        return [_reverse_keys(item) for item in value]
    return value


def _link_fixture(facts: dict[str, dict[str, Any]]) -> tuple[dict[str, Any], dict[str, Any]]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    files = config.part_files(loaded, "both", None)
    linked_image = image.Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image)
    try:
        contract_graph = contracts.ContractGraph.load(loaded.inputs.contract_graph, loaded.contract_function_prefix)
        return link.build(
            loaded,
            "both",
            files,
            facts,
            linked_image,
            contract_graph,
            {"c_sources": "fixture", "image": "fixture"},
        )
    finally:
        linked_image.close()


def test_canonical_does_not_depend_on_dictionary_insertion_order() -> None:
    left = {"function": "function:box_new", "provenance": [{"source": "c", "line": 8, "file": "src/store.c"}]}
    right = {"provenance": [{"file": "src/store.c", "line": 8, "source": "c"}], "function": "function:box_new"}
    assert jsonio.canonical(left) == jsonio.canonical(right)


def test_canonical_exclusions_are_unique_sorted_and_counted() -> None:
    first = {
        "kind": "access",
        "reason": "sizeof",
        "function": "function:box_new",
        "target": "global:box_count",
        "detail": {"event": "unevaluated"},
        "provenance": [{"source": "c", "line": 8, "file": "src/store.c"}],
    }
    second = {
        "target": "global:box_count",
        "function": "function:box_new",
        "reason": "sizeof",
        "kind": "access",
        "provenance": [{"file": "src/store.c", "line": 8, "source": "c"}],
        "detail": {"event": "unevaluated"},
    }
    document = jsonio.exclusion_document("both", [second, first])
    assert document["counts"] == {"access": {"sizeof": 1}}
    assert document["items"] == [first]


def test_link_build_is_byte_stable_when_every_fact_dictionary_is_reversed() -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    files = config.part_files(loaded, "both", None)
    facts = {
        source: clangfacts.extract(
            source,
            loaded.inputs.c_tree,
            commands.rewrite(
                commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source), loaded.argument_rewrites
            ),
            loaded.vocabulary,
        )
        for source in files
    }
    graph, exclusions = _link_fixture(facts)
    reversed_graph, reversed_exclusions = _link_fixture(_reverse_keys(facts))
    assert jsonio.dumps(graph) == jsonio.dumps(reversed_graph)
    assert jsonio.dumps(exclusions) == jsonio.dumps(reversed_exclusions)


def test_cached_fixture_facts_produce_every_output_byte_for_byte_as_a_fresh_build(tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    fresh = build(FIXTURE, "both", tmp_path / "fresh", cache)
    cached = build(FIXTURE, "both", tmp_path / "cached", cache)
    assert cached.parsed == []
    assert {name: Path(path).read_bytes() for name, path in fresh.outputs.items()} == {
        name: Path(path).read_bytes() for name, path in cached.outputs.items()
    }


def test_every_contract_callback_drop_has_a_c3_classification() -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    files = config.part_files(loaded, "both", None)
    facts = {
        source: clangfacts.extract(
            source,
            loaded.inputs.c_tree,
            commands.rewrite(
                commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source), loaded.argument_rewrites
            ),
            loaded.vocabulary,
        )
        for source in files
    }
    source_node = {"id": "Prog/src/store/RunGet", "kind": "fn", "syms": ["run_get"]}
    target_node = {"id": "Prog/src/store/BoxGet", "kind": "fn", "syms": ["box_get"]}
    contract_graph = contracts.ContractGraph(
        nodes={source_node["id"]: source_node, target_node["id"]: target_node},
        functions={
            ("src/store.c", "run_get"): source_node,
            ("src/store.c", "box_get"): target_node,
        },
        edges=[
            {"kind": "table", "src": source_node["id"], "dst": "Prog/src/store/NoC", "evidence": ["table:ops"]},
            {"kind": "table", "src": source_node["id"], "dst": target_node["id"], "evidence": ["call:ops"]},
            {"kind": "table", "src": source_node["id"], "dst": target_node["id"], "evidence": ["table:missing"]},
            *[
                {"kind": kind, "src": source_node["id"], "dst": target_node["id"], "evidence": []}
                for kind in ("store", "install", "kit")
            ],
        ],
    )
    linked_image = image.Image(loaded.inputs.image, loaded.inputs.system_map, loaded.image)
    try:
        linker = link.Linker(loaded, files, facts, linked_image, contract_graph)
        source = linker.visible("src/store.c", "run_get")
        assert source is not None
        exclusions: list[dict[str, Any]] = []
        link._callback_edges(
            link.GraphBuilder(),
            linker,
            contract_graph,
            [table for unit in facts.values() for table in unit["tables"]],
            {source.id: source},
            exclusions,
        )
    finally:
        linked_image.close()
    assert {item["reason"] for item in exclusions} == {
        "target_without_c_definition",
        "evidence_not_a_table",
        "no_c_initializer",
        "kind_not_read",
    }
    assert all(set(item) == {"kind", "reason", "function", "target", "detail", "provenance"} for item in exclusions)


def test_optional_unit_fact_exclusions_are_normalized_into_c3() -> None:
    loaded = config.load(FIXTURE / "cgraph.toml")
    files = config.part_files(loaded, "both", None)
    facts = {
        source: clangfacts.extract(
            source,
            loaded.inputs.c_tree,
            commands.rewrite(
                commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source), loaded.argument_rewrites
            ),
            loaded.vocabulary,
        )
        for source in files
    }
    facts["src/store.c"]["exclusions"] = [
        {
            "kind": "function",
            "reason": "result_registers_unknown",
            "function": "box_new",
            "target": None,
            "detail": {"result_type": "opaque"},
            "provenance": [{"source": "c", "file": "src/store.c", "line": 8}],
        }
    ]
    _, exclusions = _link_fixture(facts)
    assert {
        "kind": "function",
        "reason": "result_registers_unknown",
        "function": "function:box_new",
        "target": None,
        "detail": {"result_type": "opaque"},
        "provenance": [{"source": "c", "file": "src/store.c", "line": 8}],
    } in exclusions["items"]


def test_structure_drops_are_written_as_c3_items() -> None:
    builder = link.GraphBuilder()
    builder.node("struct:parent", "struct", "parent", c_file="src/a.c", line=1)
    _, embedding_drops = structure.layout_edges(
        builder,
        {
            "parent@src/a.c:1": {
                "def": "src/a.c:1",
                "fields": [
                    {"name": "child", "record": "child", "record_def": "src/a.c:4", "line": 2},
                ],
            }
        },
        config.load(FIXTURE / "cgraph.toml").vocabulary,
    )

    class MembershipLinker:
        def __init__(self) -> None:
            self.tus = {"src/a.c": {"functions": {}}}

        @staticmethod
        def unit_id(unit: dict[str, Any]) -> str:
            return f"struct:{unit['name']}"

    membership_drops = structure.membership_edges(
        builder,
        MembershipLinker(),
        [
            (
                "function:part",
                {
                    "arguments": [None],
                    "argument_helpers": [None],
                    "operation": "insert",
                    "membership": "list",
                    "provenance": [{"source": "c", "file": "src/a.c", "line": 2}],
                    "tu": "src/a.c",
                },
            ),
            (
                "function:part",
                {
                    "arguments": [{"name": "child"}, {"name": "parent"}],
                    "argument_helpers": [None, None],
                    "operation": "insert",
                    "membership": "list",
                    "provenance": [{"source": "c", "file": "src/a.c", "line": 3}],
                    "tu": "src/a.c",
                },
            ),
        ],
    )
    assert embedding_drops[0]["reason"] == "record_not_a_node"
    assert {item["reason"] for item in membership_drops} == {"arguments_unresolved", "end_not_a_node"}
