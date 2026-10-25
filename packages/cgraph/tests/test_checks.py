"""Every hard check fails when what it guards breaks, and only that check."""

from __future__ import annotations

import copy
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from conftest import read

from cgraph import checks, cli, config, jsonio, pipeline

Documents = dict[str, Any]

# A compiled program whose kept load reads another parameter at the displacement of a field the
# compiler removed (tests/fixtures/decoy).
DECOY = Path(__file__).parent / "fixtures" / "decoy"


@pytest.fixture(scope="module")
def documents(built: Path) -> Documents:
    check_document = read(built, "checks.json")
    layers = {name: check_document[name] for name in ("call_layer", "access_layer", "structure_layer")}
    return {
        "graph": read(built, "graph.json"),
        "owners": read(built, "owners.json"),
        "topics": read(built, "topics.json"),
        "layers": layers,
        "thresholds": config.Thresholds(call_agreement=0.95, access_agreement=0.8, field_offsets=0.9),
    }


def failed(documents: Documents) -> list[str]:
    result = checks.hard(
        documents["graph"], documents["owners"], documents["topics"], documents["layers"], documents["thresholds"]
    )
    return [item["name"] for item in result if not item["ok"]]


def edge_of(graph: dict[str, Any], kind: str) -> dict[str, Any]:
    return next(edge for edge in graph["edges"] if edge["kind"] == kind)


def test_the_fixture_passes_every_hard_check(documents: Documents) -> None:
    assert failed(documents) == []


def _parse(d: Documents) -> None:
    d["graph"]["analysis"]["parse_errors"] = {"src/store.c": ["src/store.c:1: expected ';'"]}


def _function(d: Documents) -> None:
    d["graph"]["analysis"]["expected_functions"].append("function:missing_one")


def _global(d: Documents) -> None:
    d["graph"]["analysis"]["image"]["globals"].append(
        {"function": "function:box_new", "global": "ghost_state", "target_class": "state"}
    )


def _provenance(d: Documents) -> None:
    edge_of(d["graph"], "call")["provenance"] = []


def _layout(d: Documents) -> None:
    edge_of(d["graph"], "embeds")["layout"]["offset"] = -1


def _site(d: Documents) -> None:
    edge_of(d["graph"], "member_of")["provenance"] = [{"source": "image", "address": "0x0"}]


def _ids(d: Documents) -> None:
    d["graph"]["nodes"].append(copy.deepcopy(d["graph"]["nodes"][0]))


def _owner(d: Documents) -> None:
    d["owners"]["owners"] = [owner for owner in d["owners"]["owners"] if owner["id"] != "struct:box"]


def _topic(d: Documents) -> None:
    d["topics"]["owners"]["struct:box"] = ["notifications.example.node..needs"]


def _absolute(d: Documents) -> None:
    d["graph"]["nodes"][0]["c_file"] = "/home/someone/tree/src/store.c"


def _calls(d: Documents) -> None:
    d["layers"]["call_layer"]["agree"] = d["layers"]["call_layer"]["agree"] // 2


def _accesses(d: Documents) -> None:
    d["layers"]["access_layer"]["state_agreement"] = 0.5


def _offsets(d: Documents) -> None:
    sample = d["layers"]["structure_layer"]["field_offsets"]["sample"]
    sample["matched"] = 1  # one in fifty is not a pass


def _contract_mapping(d: Documents) -> None:
    d["graph"]["analysis"]["contract_mapping_duplicates"] = [
        {"address": "0x111b4", "ids": ["Prog/src/store/BoxNew", "Prog/src/store/BoxNewDuplicate"]}
    ]


MUTATIONS: list[tuple[str, Callable[[Documents], None]]] = [
    ("no C parse errors", _parse),
    ("every function of the part's files that the image holds is a node", _function),
    ("every global the part's image code addresses is a node", _global),
    ("every node and edge has provenance", _provenance),
    ("every embedding has a clang layout", _layout),
    ("every membership has a C site", _site),
    ("node ids are valid and unique", _ids),
    ("one owner for each function, data structure and subsystem", _owner),
    ("every owner has topics, each a valid subject", _topic),
    ("no absolute path in any output", _absolute),
    ("contract_mapping_unique", _contract_mapping),
    ("call agreement at least 0.95", _calls),
    ("access agreement at least 0.80", _accesses),
    ("field offsets at least 0.90, in all and in the first 50", _offsets),
]


@pytest.mark.parametrize(("name", "mutate"), MUTATIONS, ids=[name for name, _ in MUTATIONS])
def test_each_hard_check_fails_alone(documents: Documents, name: str, mutate: Callable[[Documents], None]) -> None:
    mutated = copy.deepcopy(documents)
    mutate(mutated)
    assert failed(mutated) == [name]


def test_every_hard_check_has_a_failing_case(documents: Documents) -> None:
    names = [
        item["name"]
        for item in checks.hard(
            documents["graph"], documents["owners"], documents["topics"], documents["layers"], documents["thresholds"]
        )
    ]
    assert names == [name for name, _ in MUTATIONS]


def test_a_c_file_that_does_not_parse_fails_the_run(copy: Path, tmp_path: Path) -> None:
    source = copy / "src" / "store.c"
    source.write_text(source.read_text() + "\nint broken(void) { return }\n")
    out = tmp_path / "out"
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out)]) == 1
    assert "no C parse errors" in [item["name"] for item in read(out, "checks.json")["hard_checks"] if not item["ok"]]


def test_thresholds_are_read_and_checked(tmp_path: Path) -> None:
    path = tmp_path / "cgraph.toml"
    path.write_text(
        '[inputs]\nc_tree = "t"\nimage = "i"\nsystem_map = "m"\n[parts.p]\nfiles = ["a.c"]\n'
        '[c]\nflags = ["-x"]\n[checks]\nmin_call_agreement = 1.5\n'
    )
    with pytest.raises(config.ConfigError, match=r"checks\.min_call_agreement must be a number from 0 to 1"):
        config.load(path)


def test_a_part_overrides_the_default_thresholds(tmp_path: Path) -> None:
    path = tmp_path / "cgraph.toml"
    path.write_text(
        '[inputs]\nc_tree = "t"\nimage = "i"\nsystem_map = "m"\n'
        '[parts.small]\nfiles = ["a.c"]\nchecks = { min_call_agreement = 0.99 }\n[parts.big]\nfiles = ["b.c"]\n'
        '[c]\nflags = ["-x"]\n[checks]\nmin_call_agreement = 0.9\nmin_field_offsets = 0.8\n'
    )
    loaded = config.load(path)
    assert loaded.part_thresholds["small"] == config.Thresholds(call_agreement=0.99, field_offsets=0.8)
    assert loaded.part_thresholds["big"] == config.Thresholds(call_agreement=0.9, field_offsets=0.8)


def test_a_c_global_with_no_image_symbol_is_counted_apart_and_aliases_match() -> None:
    graph = {
        "nodes": [{"id": "function:f", "kind": "function", "part": True, "image_addresses": ["0x1"]}],
        "edges": [],
        "analysis": {
            "c_targets": {"function:f": [
                {"name": "kept", "target_class": "state", "image_symbol": True},
                {"name": "removed", "target_class": "state", "image_symbol": False},
                {"name": "jiffies", "target_class": "state", "image_symbol": True},
            ]},
            "image": {
                "globals": [
                    {"function": "function:f", "global": "kept", "target_class": "state"},
                    {"function": "function:f", "global": "jiffies_64", "aliases": ["jiffies"], "target_class": "state"},
                ],
                "sized_symbol_names": ["kept", "jiffies_64"],
                "outside_sized_symbols": [],
            },
            "generic_unconfirmed": [],
        },
    }  # fmt: skip
    result = checks.accesses(graph)
    assert (result["agree"], result["only_c"], result["only_image"]) == (2, 0, 0)
    assert result["counted_apart"] == {"state_without_image_symbol": 1}
    assert result["state_agreement"] == 1.0


@pytest.mark.parametrize(
    "identity",
    [
        "function:box_new",
        "function:helper_dup@src/store.c",
        "global:next_id.counter@src/store.c",
        "struct:Box_T@src/x.c",
        "percpu:current_box",
        "subsystem:crate",
    ],
)
def test_c1_owner_ids_accept_the_contract_alphabet(identity: str) -> None:
    assert checks.ID_PATTERN.fullmatch(identity)


@pytest.mark.parametrize(
    "identity",
    [
        "",
        "box_new",
        "cluster:crate",
        "function:",
        "Function:box_new",
        "function:box new",
        "function:box\tnew",
        "function:box_new\n",
    ],
)
def test_c1_owner_ids_reject_each_invalid_whole_string(documents: Documents, identity: str) -> None:
    mutated = copy.deepcopy(documents)
    mutated["graph"]["nodes"].append({**copy.deepcopy(mutated["graph"]["nodes"][0]), "id": identity, "part": False})
    assert failed(mutated) == ["node ids are valid and unique"]


def test_a_4033_byte_topic_fails_only_the_subject_check(documents: Documents) -> None:
    mutated = copy.deepcopy(documents)
    mutated["topics"]["owners"]["struct:box"] = ["a" * 4033]
    assert failed(mutated) == ["every owner has topics, each a valid subject"]


def test_hard_checks_have_all_sorted_offending_items_and_counts(documents: Documents) -> None:
    hard_checks = checks.hard(
        documents["graph"], documents["owners"], documents["topics"], documents["layers"], documents["thresholds"]
    )
    for item in hard_checks:
        assert item["count"] == len(item["items"])
        assert item["items"] == sorted(item["items"], key=jsonio.canonical)


def test_a_hard_check_lists_every_missing_function(documents: Documents) -> None:
    mutated = copy.deepcopy(documents)
    mutated["graph"]["analysis"]["expected_functions"] = [f"function:missing_{index:02d}" for index in range(12)]
    item = next(
        item
        for item in checks.hard(
            mutated["graph"], mutated["owners"], mutated["topics"], mutated["layers"], mutated["thresholds"]
        )
        if item["name"] == "every function of the part's files that the image holds is a node"
    )
    assert item["count"] == len(item["items"]) == 12


def test_call_and_address_taken_pairs_are_the_complete_symmetric_differences(documents: Documents) -> None:
    graph = documents["graph"]
    layer = checks.calls(graph)
    c_pairs = {(item["source"], item["target"]) for item in graph["analysis"]["c_calls"]}
    image_pairs = {
        (item["source"], item["target"])
        for item in graph["analysis"]["image"]["calls"]
        if item["source"] in {node["id"] for node in graph["nodes"] if node.get("part")}
    }
    assert {(item["source"], item["target"]) for item in layer["pairs"]["only_c"]} == c_pairs - image_pairs
    assert {(item["source"], item["target"]) for item in layer["pairs"]["only_image"]} == image_pairs - c_pairs
    address_taken = layer["address_taken"]
    assert len(address_taken["pairs"]["only_c"]) == address_taken["only_c"]
    assert len(address_taken["pairs"]["only_image"]) == address_taken["only_image"]


def test_every_field_offset_miss_has_per_function_image_evidence(documents: Documents) -> None:
    offsets = checks.structure(documents["graph"])["field_offsets"]
    assert len(offsets["misses"]) == offsets["candidates"] - offsets["matched"]
    assert set(offsets["image_evidence"]) == {item["function"] for item in offsets["misses"]}


def test_duplicate_contract_mapping_hard_check_names_both_contract_ids(documents: Documents) -> None:
    mutated = copy.deepcopy(documents)
    _contract_mapping(mutated)
    item = next(
        item
        for item in checks.hard(
            mutated["graph"], mutated["owners"], mutated["topics"], mutated["layers"], mutated["thresholds"]
        )
        if item["name"] == "contract_mapping_unique"
    )
    assert item["ok"] is False
    assert item["items"][0]["ids"] == ["Prog/src/store/BoxNew", "Prog/src/store/BoxNewDuplicate"]
    assert "Prog/src/store/BoxNewDuplicate" in item["detail"]


def test_field_offset_misses_keep_all_fields_and_image_evidence_once_per_function() -> None:
    graph = {
        "nodes": [{"id": "function:box_new", "kind": "function", "part": True, "image_addresses": ["0x1"]}],
        "edges": [],
        "analysis": {
            "field_accesses": {
                "function:box_new": [
                    {"struct": "box", "field": "tag", "offset": 4, "base": "pointer", "size": 4, "bitfield": False},
                    {"struct": "box", "field": "spare", "offset": 12, "base": "pointer", "size": 4, "bitfield": False},
                ]
            },
            "image": {
                "memory_offsets": {"function:box_new": [4]},
                "addi_constants": {"function:box_new": [8]},
                "globals": [],
            },
            "excluded_embedding_bases": [],
        },
    }
    offsets = checks.structure(graph)["field_offsets"]
    assert offsets["misses"] == [
        {
            "function": "function:box_new",
            "struct": "box",
            "field": "spare",
            "offset": 12,
            "base": "pointer",
            "method": "no_matching_offset",
        }
    ]
    assert offsets["image_evidence"] == {"function:box_new": {"memory_offsets": [4], "addi_constants": [8]}}


def _field_graph(item: dict[str, Any], image: dict[str, Any], function: str = "function:read_mark") -> dict[str, Any]:
    """One function with one field access, and the image facts the check reads for it."""
    return {
        "nodes": [{"id": function, "kind": "function", "part": True, "image_addresses": ["0x11158"]}],
        "edges": [],
        "analysis": {
            "field_accesses": {function: [item]},
            "image": {"addi_constants": {}, "globals": [], **{key: {function: value} for key, value in image.items()}},
            "excluded_embedding_bases": [],
        },
    }


_MARK = {
    "struct": "slot",
    "field": "mark",
    "offset": 192,
    "displacement": 192,
    "param": 0,
    "base": "pointer",
    "size": 8,
    "bitfield": False,
}


def test_a_displacement_the_image_reaches_through_another_parameter_confirms_no_field() -> None:
    """The C names `s->mark` through the first parameter; the image's only load of 192 is through
    the second. The displacement alone is no confirmation."""
    graph = _field_graph(_MARK, {"memory_offsets": [192], "memory_parameters": [[1, 192]]})

    offsets = checks.structure(graph)["field_offsets"]

    assert offsets["candidates"] == 1 and offsets["matched"] == 0
    assert offsets["misses"][0]["method"] == "no_offset_through_the_named_parameter"
    assert offsets["image_evidence"]["function:read_mark"]["memory_parameters"] == [[1, 192]]


def test_a_displacement_the_image_reaches_through_the_same_parameter_confirms_the_field() -> None:
    graph = _field_graph(_MARK, {"memory_offsets": [192], "memory_parameters": [[0, 192]]})

    offsets = checks.structure(graph)["field_offsets"]

    assert offsets["matched"] == 1 and offsets["by_method"] == {"memory_displacement": 1}


def test_a_displacement_the_image_reaches_through_no_parameter_confirms_the_field() -> None:
    """A base the decoder cannot trace to a parameter (a pointer loaded from memory) keeps its
    displacement as evidence: only a load through another parameter is refused."""
    graph = _field_graph(_MARK, {"memory_offsets": [192], "memory_parameters": [[1, 40]]})

    offsets = checks.structure(graph)["field_offsets"]

    assert offsets["matched"] == 1


def test_a_static_function_s_argument_registers_name_no_parameter_of_its_c() -> None:
    """Only an external function's arguments are bound to a0..a7 in the order of its C parameters
    (the calling convention). The compiler may rewrite a static function's signature: clang drops
    ``fct_queue``'s unused ``fn``, so its C parameter 2 arrives in a1. A load through a1 then
    confirms the C's parameter 2, and nothing about the base can be refused."""
    graph = _field_graph(
        {**_MARK, "param": 2},
        {"memory_offsets": [192], "memory_parameters": [[1, 192]]},
        function="function:read_mark@src/g.c",
    )

    offsets = checks.structure(graph)["field_offsets"]

    assert offsets["matched"] == 1


def test_the_compiled_decoy_program_confirms_no_field_through_the_wrong_parameter(tmp_path: Path) -> None:
    """The whole way: build tests/fixtures/decoy and read the build's own outputs. The C names
    `slot.mark` at 192 through the first parameter of two functions, and each image function's only
    load reads the second parameter at 192: the displacement alone would confirm both accesses."""
    out = tmp_path / "out"

    pipeline.run(config.load(DECOY / "cgraph.toml"), "one", out)

    image = read(out, "graph.json")["analysis"]["image"]
    assert image["memory_offsets"] == {"function:read_mark": [192], "function:root": [192]}
    assert image["memory_parameters"] == {"function:read_mark": [[1, 192]], "function:root": [[1, 192]]}
    offsets = read(out, "checks.json")["structure_layer"]["field_offsets"]
    assert offsets["candidates"] == 2 and offsets["matched"] == 0
    assert offsets["by_method"] == {"no_offset_through_the_named_parameter": 2}
    assert offsets["image_evidence"]["function:read_mark"]["memory_parameters"] == [[1, 192]]
