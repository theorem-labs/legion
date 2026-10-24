"""The interface measure: each interface's own declaration against the groups' C names."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import pytest

from cgraph import measures
from cgraph.contracts import ContractGraph
from cgraph.errors import CacheError, InputError

GROUPS = [
    ["global:box_table", "struct:box"],
    ["global:next_id.counter@src/store.c"],
    ["struct:bag"],
    ["global:unused"],
]


def _graph() -> dict[str, Any]:
    names = {
        "global:box_table": "box_table",
        "struct:box": "box",
        "global:next_id.counter@src/store.c": "next_id.counter",
        "struct:bag": "bag",
        "global:unused": "unused",
    }
    return {
        "nodes": [
            {"id": "function:f", "kind": "function", "name": "f", "part": True, "contract": "Prog/f"},
            *({"id": identity, "kind": identity.split(":", 1)[0], "name": name} for identity, name in names.items()),
        ]
    }


def _contracts(declarations: dict[str, str]) -> ContractGraph:
    return ContractGraph(
        nodes={
            f"if:{name}": {"id": f"if:{name}", "kind": "iface", "file": file} for name, file in declarations.items()
        },
        edges=[{"kind": "binds", "src": "Prog/f", "dst": f"if:{name}"} for name in declarations],
    )


def _tree(tmp_path: Path) -> Path:
    lean = tmp_path / "lean" / "Lean"
    lean.mkdir(parents=True)
    (lean / "A.lean").write_text(
        "namespace X\n"
        "structure A where\n"
        "  t : box_table\n"
        "  u : boxes\n"
        "def after := 1\n"
        "structure B where\n"
        "  v : next_id.counter\n"
        "end X\n"
    )
    (lean / "B.lean").write_text("theorem C : bag_x = bag := rfl\n")
    return tmp_path / "lean"


DECLARATIONS = {"A": "Lean/A.lean:2", "B": "Lean/A.lean:6", "C": "Lean/B.lean:1"}


def _groups_by_interface(result: dict[str, Any] | None) -> dict[str, list[str]]:
    assert result is not None
    return {item["interface"]: item["groups"] for item in result["mappings"]}


def test_interfaces_match_whole_c_names_from_an_index_without_per_name_regex(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    source = _tree(tmp_path)
    compiled: list[str] = []
    original_compile = re.compile

    def counted_compile(pattern: Any, flags: int = 0) -> re.Pattern[str]:
        compiled.append(str(pattern))
        return original_compile(pattern, flags)

    def refused(*_: Any, **__: Any) -> None:
        raise AssertionError("a per-name re.search ran")

    monkeypatch.setattr(re, "compile", counted_compile)
    monkeypatch.setattr(re, "search", refused)
    result = measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, None)
    assert _groups_by_interface(result) == {
        "A": ["global:box_table"],
        "B": ["global:next_id.counter@src/store.c"],
        "C": ["struct:bag"],
    }
    assert result is not None
    assert (result["exactly_one"], result["several"], result["none"]) == (3, 0, 0)
    # Only the one name that is not a single identifier gets a pattern, and it gets it once.
    assert len(compiled) == 1


def test_each_declaring_module_is_read_once_per_run(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    source = _tree(tmp_path)
    reads: list[str] = []
    original = Path.read_bytes

    def counted(path: Path) -> bytes:
        reads.append(path.name)
        return original(path)

    monkeypatch.setattr(Path, "read_bytes", counted)
    measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, None)
    assert sorted(reads) == ["A.lean", "B.lean"]


def test_only_a_changed_declaring_module_is_parsed_again(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    source = _tree(tmp_path)
    cache = tmp_path / "cache"
    parsed: list[int] = []
    original = measures._parse_module

    def counted(text: str) -> dict[str, Any]:
        parsed.append(len(text))
        return original(text)

    monkeypatch.setattr(measures, "_parse_module", counted)
    first = measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)
    assert len(parsed) == 2
    parsed.clear()
    assert measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache) == first
    assert parsed == []
    (source / "Lean" / "B.lean").write_text("theorem C : box = bag := rfl\n")
    changed = measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)
    assert len(parsed) == 1
    assert _groups_by_interface(changed)["C"] == ["global:box_table", "struct:bag"]


def test_a_change_to_the_parse_rules_parses_every_module_again(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    source = _tree(tmp_path)
    cache = tmp_path / "cache"
    measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)
    parsed: list[int] = []
    original = measures._parse_module

    def counted(text: str) -> dict[str, Any]:
        parsed.append(len(text))
        return original(text)

    monkeypatch.setattr(measures, "_parse_module", counted)
    monkeypatch.setattr(measures, "_TOP_LEVEL", re.compile(r"^(?:structure|def)\b"))
    measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)
    assert len(parsed) == 2


def test_a_malformed_interface_cache_entry_is_a_cache_error(tmp_path: Path) -> None:
    source = _tree(tmp_path)
    cache = tmp_path / "cache"
    measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)
    for entry in (cache / "interfaces").glob("*.json"):
        entry.write_text("{")
    with pytest.raises(CacheError, match="remove the cache directory"):
        measures._interfaces(_graph(), _contracts(DECLARATIONS), source, GROUPS, cache)


def test_a_missing_declaring_module_is_an_input_error(tmp_path: Path) -> None:
    source = _tree(tmp_path)
    with pytest.raises(InputError, match=re.escape("Lean/Missing.lean")):
        measures._interfaces(_graph(), _contracts({"M": "Lean/Missing.lean:1"}), source, GROUPS, None)


def test_the_name_index_matches_the_whole_name_rule_for_every_name_shape() -> None:
    names = ["box", "box_table", "next_id.counter", "a.b.c", "", "(unnamed struct at x.c:1:2)", "_x", "9lives", "b"]
    bodies = [
        ["structure S where", "  f : box_table", "  g : boxes"],
        ["  v : next_id.counter", "  w : a.b.cd"],
        ["(unnamed struct at x.c:1:2) b"],
        ["_x9lives a.b.c"],
        [""],
        ["  .box. 9lives_"],
    ]
    identifiers = {position: {name} for position, name in enumerate(names)}
    index = measures._NameIndex(identifiers)
    for body in bodies:
        text = "\n".join(body)
        expected = {
            position
            for position, name in enumerate(names)
            if re.search(r"(?<![A-Za-z0-9_])" + re.escape(name) + r"(?![A-Za-z0-9_])", text)
        }
        tokens = {token for line in body for token in measures._IDENTIFIER.findall(line)}
        assert index.groups(tokens, body) == expected, body


def test_a_function_with_several_contracts_takes_the_interfaces_each_binds(tmp_path: Path) -> None:
    source = _tree(tmp_path)
    graph = _graph()
    graph["nodes"][0]["contract"] = ["Prog/f", "Prog/g"]
    contracts = ContractGraph(
        nodes={
            "if:A": {"id": "if:A", "kind": "iface", "file": "Lean/A.lean:2"},
            "if:C": {"id": "if:C", "kind": "iface", "file": "Lean/B.lean:1"},
        },
        edges=[
            {"kind": "binds", "src": "Prog/f", "dst": "if:A"},
            {"kind": "binds", "src": "Prog/g", "dst": "if:C"},
        ],
    )
    result = measures._interfaces(graph, contracts, source, GROUPS, None)
    assert result is not None
    assert {item["interface"]: item["functions"] for item in result["mappings"]} == {
        "A": ["function:f"],
        "C": ["function:f"],
    }
