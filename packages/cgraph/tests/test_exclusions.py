"""Contract C3: every input item that does not reach graph.json is in exclusions.json with its
reason. Each test builds a compiled program whose C holds the item, and reads the item back."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path
from typing import Any

import pytest
from conftest import read

from cgraph import cli, config, pipeline

# State accesses the graph cannot hold (tests/fixtures/reasons).
REASONS = Path(__file__).parent / "fixtures" / "reasons"


@pytest.fixture(scope="module")
def built(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("reasons")
    pipeline.run(config.load(REASONS / "cgraph.toml"), "one", out)
    return out


def _accesses(out: Path, reason: str) -> list[dict[str, Any]]:
    return [
        item for item in read(out, "exclusions.json")["items"] if item["kind"] == "access" and item["reason"] == reason
    ]


def _lock(function: str, reason: str, line: int) -> dict[str, Any]:
    """The excluded state access of a lock or an unlock at ``line`` of src/reasons.c."""
    return {
        "kind": "access",
        "reason": reason,
        "function": function,
        "target": None,
        "detail": {"field": None, "parameter": 0},
        "provenance": [{"source": "c", "file": "src/reasons.c", "line": line}],
    }


def test_a_lock_that_is_the_root_s_own_parameter_is_parameter_at_root(built: Path) -> None:
    """``take_parameter`` locks and unlocks its parameter ``g``: no state of the program is it."""
    assert _accesses(built, "parameter_at_root") == [
        _lock("function:take_parameter", "parameter_at_root", 24),
        _lock("function:take_parameter", "parameter_at_root", 25),
    ]


def test_a_folded_callee_s_lock_the_caller_cannot_name_is_unbound_parameter(built: Path) -> None:
    """``take_fetched`` passes ``take_inner`` the value of a call: the lock take_inner takes through
    its parameter binds to nothing of the caller's. The provenance is the callee's lock and unlock."""
    assert _accesses(built, "unbound_parameter") == [
        _lock("function:take_fetched", "unbound_parameter", 31),
        _lock("function:take_fetched", "unbound_parameter", 32),
    ]


def test_a_folded_callee_s_lock_through_the_caller_s_member_is_an_edge(built: Path) -> None:
    """The contrast: ``take_loaded`` passes a member of its own parameter, which the graph holds."""
    edges = [
        (edge["target"], edge["field"], edge["access"])
        for edge in read(built, "graph.json")["edges"]
        if edge["kind"] == "access" and edge["source"] == "function:take_loaded"
    ]
    assert ("struct:holder@src/reasons.c", "outer", "lock") in edges
    assert ("struct:holder@src/reasons.c", "outer", "unlock") in edges
    assert not [item for item in read(built, "exclusions.json")["items"] if item["function"] == "function:take_loaded"]


def test_a_read_in_an_unreadable_generic_association_is_generic_unconfirmed(built: Path) -> None:
    """``active_ring`` reads through to_common(), whose associations are each the same member of
    another struct, inside another macro's argument (the kernel's ns_tree_active()). Every
    association has the type of the whole, and the macro's text gives no selection either, so which
    struct the read names is unknown: the access is kept with its reason, not held."""
    assert _accesses(built, "generic_unconfirmed") == [
        {
            "kind": "access",
            "reason": "generic_unconfirmed",
            "function": "function:active_ring",
            "target": "struct:ring@src/reasons.c",
            "detail": {"event": "access"},
            "provenance": [{"source": "c", "file": "src/reasons.c", "line": 75}],
        }
    ]
    # What the compiler left of the read is held against the struct the association's type names.
    edges = [
        (edge["target"], edge["field"])
        for edge in read(built, "graph.json")["edges"]
        if edge["kind"] == "access" and edge["source"] == "function:active_ring"
    ]
    assert edges == [("struct:common@src/reasons.c", "state")]


def test_checks_count_each_reason_the_exclusions_list(built: Path) -> None:
    counts = read(built, "exclusions.json")["counts"]["access"]
    assert counts == {"generic_unconfirmed": 1, "parameter_at_root": 2, "unbound_parameter": 2}
    assert read(built, "checks.json")["exclusions"]["access"] == counts


def test_facts_whose_struct_access_has_no_record_stop_the_build_without_changing_the_outputs(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    """The extractor writes the record of every struct an access names into the same unit's facts,
    so no C input gives an access without its record. Facts that do (a cache an inconsistent
    extractor wrote) are an error, exit 3, never an exclusion: the graph would lose the access
    silently. ``take_loaded`` reads ``struct holder``; its record is removed from the cached facts."""
    program, cache, out = tmp_path / "reasons", tmp_path / "cache", tmp_path / "out"
    shutil.copytree(REASONS, program)
    arguments = ["build", "--config", str(program / "cgraph.toml"), "--part", "one", "--out", str(out)]
    assert cli.main([*arguments, "--cache", str(cache)]) == 0
    before = {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(out.iterdir())}
    entry = cache / "facts" / f"{json.loads((cache / 'manifest.json').read_text())['src/reasons.c']['key']}.json"
    facts = json.loads(entry.read_text())
    del facts["records"]["holder@src/reasons.c:14"]
    entry.write_text(json.dumps(facts))
    # The link's core would otherwise be reused unchanged: its key holds the unit's key, not its facts.
    shutil.rmtree(cache / "link")
    capsys.readouterr()

    assert cli.main([*arguments, "--cache", str(cache)]) == 3

    assert "struct unit holder@src/reasons.c:14 has no record in the facts" in capsys.readouterr().err
    assert {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(out.iterdir())} == before
