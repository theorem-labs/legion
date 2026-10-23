"""The command, the checks of a run, and the rebuild after a change."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from conftest import FIXTURE, build, read

from cgraph import checks, cli

OUTPUTS = ("graph.json", "checks.json", "owners.json", "topics.json", "measures.json", "report.md")


def digests(out: Path) -> dict[str, str]:
    return {name: hashlib.sha256((out / name).read_bytes()).hexdigest() for name in OUTPUTS}


def test_layer_checks_are_written(built: Path) -> None:
    result = read(built, "checks.json")
    calls = result["call_layer"]
    assert calls["agree"] == calls["c_edges"] == calls["image_edges"] and calls["agree"] > 0
    assert calls["address_taken"]["agree"] == 2
    accesses = result["access_layer"]
    assert accesses["only_image"] == 0
    assert set(accesses["disagreements"]["only_c"]) == {"state_not_addressed_by_image"}
    structure = result["structure_layer"]
    assert structure["embeds"] == {"total": 1, "layout_confirmed": 1, "unconfirmed": 0}
    offsets = structure["field_offsets"]
    # crate.box sits at offset 0, which the code folds into the load of box.size: the one miss.
    misses = {
        (item["function"], item["struct"], item["field"]) for item in offsets["sample"]["items"] if not item["matched"]
    }
    assert misses == {("function:crate_size", "crate", "box")}
    assert offsets["matched"] == offsets["candidates"] - 1
    assert result["passes"] and all(item["ok"] for item in result["hard_checks"])


def test_every_sampled_offset_miss_carries_the_image_evidence(built: Path) -> None:
    for item in read(built, "checks.json")["structure_layer"]["field_offsets"]["sample"]["items"]:
        if not item["matched"]:
            assert "image_memory_offsets" in item and "image_addi_constants" in item


def test_same_input_gives_the_same_bytes(tmp_path: Path) -> None:
    first, second = tmp_path / "first", tmp_path / "second"
    build(FIXTURE, "both", first)
    build(FIXTURE, "both", second)
    assert digests(first) == digests(second)


def test_incremental_build_after_a_change_to_one_file_equals_a_full_build(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    first = build(copy, "both", tmp_path / "before", cache)
    assert first.parsed == ["src/bags.c", "src/store.c"]
    source = copy / "src" / "store.c"
    source.write_text(source.read_text().replace("\tb->first = NULL;\n", "\tb->first = NULL;\n\tbox_count = 0;\n"))
    incremental = build(copy, "both", tmp_path / "incremental", cache)
    assert incremental.parsed == ["src/store.c"] and incremental.reused == ["src/bags.c"]
    full = build(copy, "both", tmp_path / "full", tmp_path / "empty-cache")
    assert full.parsed == ["src/bags.c", "src/store.c"]
    assert digests(tmp_path / "incremental") == digests(tmp_path / "full")
    assert digests(tmp_path / "incremental") != digests(tmp_path / "before")
    graph = read(tmp_path / "incremental", "graph.json")
    assert any(
        edge["kind"] == "access"
        and edge["source"] == "function:box_tree_add"
        and edge["target"] == "global:box_count"
        and edge["access"] == "write"
        for edge in graph["edges"]
    )


def test_a_header_change_reparses_every_unit_that_includes_it(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    build(copy, "both", tmp_path / "before", cache)
    header = copy / "include" / "mini.h"
    header.write_text(header.read_text().replace("\tlong tag;\n", "\tlong tag;\n\tlong spare;\n"))
    again = build(copy, "both", tmp_path / "after", cache)
    assert again.parsed == ["src/bags.c", "src/store.c"]
    unchanged = build(copy, "both", tmp_path / "again", cache)
    assert unchanged.parsed == [] and digests(tmp_path / "again") == digests(tmp_path / "after")


def test_cli_exit_status(copy: Path, tmp_path: Path, capsys: object) -> None:
    out = tmp_path / "out"
    assert (
        cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out), "--json"]) == 0
    )
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "nothing", "--out", str(out)]) == 2
    assert cli.main(["build", "--config", str(copy / "missing.toml"), "--part", "store", "--out", str(out)]) == 2
    (copy / "image" / "System.map").unlink()
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out)]) == 3


def test_an_assembly_function_of_a_part_file_is_reported_not_a_node(copy: Path, tmp_path: Path) -> None:
    contracts = json.loads((copy / "contracts.json").read_text())
    contracts["nodes"].append(
        {"id": "Prog/src/store/AnchorPair", "kind": "fn", "syms": ["anchor_pair"], "addr": "", "in_image": True}
    )
    (copy / "contracts.json").write_text(json.dumps(contracts))
    out = tmp_path / "out"
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out)]) == 0
    assert read(out, "graph.json")["analysis"]["contract_functions_without_c"] == ["function:anchor_pair"]


def test_a_function_the_image_holds_but_the_graph_lacks_fails_the_hard_check(built: Path) -> None:
    graph = read(built, "graph.json")
    graph["analysis"]["expected_functions"].append("function:missing_one")
    owners, topics = read(built, "owners.json"), read(built, "topics.json")
    failed = [item for item in checks.hard(graph, owners, topics, checks.structure(graph)) if not item["ok"]]
    assert [item["name"] for item in failed] == ["every function of the part's files that the image holds is a node"]
    assert "function:missing_one" in failed[0]["detail"]
