"""Every hard check fails when what it guards breaks, and only that check."""

from __future__ import annotations

import copy
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest
from conftest import read

from cgraph import checks, cli, config

Documents = dict[str, Any]


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
