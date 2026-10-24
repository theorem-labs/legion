"""The link's C and image core, kept between runs: reused only while every input it reads is unchanged,
and the result byte-equal to a link from nothing."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from conftest import build, read

from cgraph import link
from cgraph.errors import CacheError

STABLE = ("graph.json", "checks.json", "exclusions.json", "owners.json", "topics.json", "measures.json", "report.md")


def _bytes(out: Path) -> dict[str, bytes]:
    return {name: (out / name).read_bytes() for name in STABLE}


@pytest.fixture
def cores(monkeypatch: pytest.MonkeyPatch) -> list[str]:
    """The parts each computed link core was for: empty when every core came from the cache."""
    computed: list[str] = []
    original = link._link_core

    def counted(linker: link.Linker, *arguments: Any) -> dict[str, Any]:
        computed.append(",".join(sorted(linker.part_files)))
        return original(linker, *arguments)

    monkeypatch.setattr(link, "_link_core", counted)
    return computed


def _edit_contracts(copy: Path, change: Callable[[dict[str, Any]], None]) -> None:
    path = copy / "contracts.json"
    contracts = json.loads(path.read_text())
    change(contracts)
    path.write_text(json.dumps(contracts))


def _rename_box_new_and_drop_the_table(contracts: dict[str, Any]) -> None:
    for node in contracts["nodes"]:
        if node["id"] == "Prog/src/store/BoxNew":
            node["id"] = "Prog/src/store/BoxNewRenamed"
    for edge in contracts["edges"]:
        if edge["src"] == "Prog/src/store/BoxNew":
            edge["src"] = "Prog/src/store/BoxNewRenamed"
    contracts["edges"] = [edge for edge in contracts["edges"] if edge["kind"] != "table"]


def test_a_contract_graph_change_reuses_the_core_and_equals_a_link_from_nothing(
    copy: Path, tmp_path: Path, cores: list[str]
) -> None:
    cache = tmp_path / "cache"
    before = build(copy, "both", tmp_path / "before", cache)
    assert cores == ["src/bags.c,src/store.c"]
    _edit_contracts(copy, _rename_box_new_and_drop_the_table)
    cores.clear()
    warm = build(copy, "both", tmp_path / "warm", cache)
    assert warm.parsed == [] and cores == []
    full = build(copy, "both", tmp_path / "full", None)
    assert _bytes(tmp_path / "warm") == _bytes(tmp_path / "full")
    assert _bytes(tmp_path / "warm") != _bytes(tmp_path / "before")
    graph = read(tmp_path / "warm", "graph.json")
    box_new = next(node for node in graph["nodes"] if node["id"] == "function:box_new")
    assert box_new["contract"] == "Prog/src/store/BoxNewRenamed"
    assert not any(edge.get("resolution") == "callback_table" for edge in graph["edges"])
    assert before.passes and full.passes


def test_an_unchanged_rebuild_reuses_the_core_byte_for_byte(copy: Path, tmp_path: Path, cores: list[str]) -> None:
    cache = tmp_path / "cache"
    build(copy, "both", tmp_path / "cold", cache)
    cores.clear()
    build(copy, "both", tmp_path / "warm", cache)
    assert cores == []
    build(copy, "both", tmp_path / "uncached", None)
    assert _bytes(tmp_path / "warm") == _bytes(tmp_path / "cold") == _bytes(tmp_path / "uncached")


def _change_c(copy: Path) -> None:
    source = copy / "src" / "store.c"
    source.write_text(source.read_text().replace("\tb->first = NULL;\n", "\tb->first = NULL;\n\tbox_count = 0;\n"))


def _change_config(copy: Path) -> None:
    path = copy / "cgraph.toml"
    text = path.read_text()
    assert 'percpu_structs = ["percpu_box"]' in text
    path.write_text(text.replace('percpu_structs = ["percpu_box"]', 'percpu_structs = ["percpu_box", "box"]'))


def _change_system_map(copy: Path) -> None:
    path = copy / "image" / "System.map"
    path.write_text(path.read_text() + "0000000000000000 a cgraph_unused_absolute\n")


def _unpin_box_new(copy: Path) -> None:
    """The fixture's contract graph pins box_new's image copy by its address; the change removes it."""

    def change(contracts: dict[str, Any]) -> None:
        for node in contracts["nodes"]:
            if node["id"] == "Prog/src/store/BoxNew":
                assert node["addr"]
                node["addr"] = ""

    _edit_contracts(copy, change)


@pytest.mark.parametrize(
    "change", [_change_c, _change_config, _change_system_map, _unpin_box_new], ids=["c", "config", "map", "pin"]
)
def test_a_change_to_an_input_the_core_reads_computes_it_again(
    copy: Path, tmp_path: Path, cores: list[str], change: Callable[[Path], None]
) -> None:
    cache = tmp_path / "cache"
    build(copy, "both", tmp_path / "before", cache)
    change(copy)
    cores.clear()
    build(copy, "both", tmp_path / "after", cache)
    assert cores == ["src/bags.c,src/store.c"]
    # One core per part: the new one replaces the part's earlier one.
    assert len(list((cache / "link").glob("*/*.json"))) == 1
    build(copy, "both", tmp_path / "full", None)
    assert _bytes(tmp_path / "after") == _bytes(tmp_path / "full")


def test_a_malformed_link_core_entry_is_a_cache_error(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    build(copy, "both", tmp_path / "before", cache)
    entries = list((cache / "link").glob("*/*.json"))
    assert len(entries) == 1
    entries[0].write_text("{")
    with pytest.raises(CacheError, match="remove the cache directory"):
        build(copy, "both", tmp_path / "after", cache)


def test_a_core_whose_image_facts_no_longer_match_is_computed_again(
    copy: Path, tmp_path: Path, cores: list[str]
) -> None:
    """A reused core reads the image facts it was built from, through the image cache, and is
    reused only while they are the same; a malformed image entry still stops the run (exit 3)."""
    cache = tmp_path / "cache"
    build(copy, "both", tmp_path / "before", cache)
    entries = sorted((cache / "image").glob("*.json"))
    assert entries
    for entry in entries:
        value = json.loads(entry.read_text())
        value["memory_offsets"] = [4093]
        entry.write_text(json.dumps(value))
    cores.clear()
    build(copy, "both", tmp_path / "after", cache)
    assert cores == ["src/bags.c,src/store.c"]
