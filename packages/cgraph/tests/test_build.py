"""The command, the checks of a run, and the rebuild after a change."""

from __future__ import annotations

import hashlib
import json
import os
from multiprocessing import get_context
from pathlib import Path
from typing import Any

import pytest
from conftest import FIXTURE, build, read

from cgraph import cli, pipeline

OUTPUTS = ("graph.json", "checks.json", "owners.json", "topics.json", "measures.json", "report.md")


def output_bytes(out: Path) -> dict[str, bytes]:
    return {str(path.relative_to(out)): path.read_bytes() for path in sorted(out.rglob("*")) if path.is_file()}


def cli_arguments(copy: Path, out: Path, cache: Path | None = None, jobs: int = 1, part: str = "store") -> list[str]:
    arguments = ["build", "--config", str(copy / "cgraph.toml"), "--part", part, "--out", str(out)]
    if cache is not None:
        arguments += ["--cache", str(cache)]
    return [*arguments, "--jobs", str(jobs)]


def cli_build(copy: Path, out: Path, cache: Path | None = None, jobs: int = 1) -> int:
    return cli.main(cli_arguments(copy, out, cache, jobs))


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
    # c->box.size is matched at 8 + 56, the displacement along the member chain; every field the
    # fixture's machine code touches matches.
    assert offsets["matched"] == offsets["candidates"] > 0
    assert ("function:crate_size", "box", "size", 64) in {
        (item["function"], item["struct"], item["field"], item["offset"]) for item in offsets["sample"]["items"]
    }
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
        and edge["target"] == "global:box_count@src/store.c"
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


def test_cli_exit_status_preserves_existing_outputs_on_unable_runs(copy: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    assert (
        cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out), "--json"]) == 0
    )
    before = output_bytes(out)
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "nothing", "--out", str(out)]) == 2
    assert output_bytes(out) == before
    assert cli.main(["build", "--config", str(copy / "missing.toml"), "--part", "store", "--out", str(out)]) == 2
    assert output_bytes(out) == before
    (copy / "image" / "System.map").unlink()
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out)]) == 3
    assert output_bytes(out) == before


def test_json_summary_omits_hard_check_items(
    copy: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    result = pipeline.Result(
        passes=False,
        hard_checks=[
            {
                "name": "check",
                "ok": False,
                "count": 2,
                "detail": "two items",
                "items": [{"node": "function:one"}, {"node": "function:two"}],
                "pairs": [{"left": "one", "right": "two"}],
                "misses": [{"field": "value"}],
                "exclusions": [{"reason": "constant"}],
            }
        ],
        parsed=["src/one.c", "src/two.c"],
        reused=["src/three.c"],
        outputs={"graph.json": str(tmp_path / "out" / "graph.json")},
        counts={"nodes": 3, "edges": 2, "owners": 1, "files": 3},
    )
    monkeypatch.setattr(cli.pipeline, "run", lambda *args: result)

    assert cli.main([*cli_arguments(copy, tmp_path / "out"), "--json"]) == 1

    summary = json.loads(capsys.readouterr().out)
    assert summary == {
        "counts": {"edges": 2, "files": 3, "nodes": 3, "owners": 1},
        "hard_checks": [{"count": 2, "detail": "two items", "name": "check", "ok": False}],
        "outputs": {"graph.json": str(tmp_path / "out" / "graph.json")},
        "parsed": 2,
        "passes": False,
        "reused": 1,
    }


def test_corrupt_cache_manifest_refuses_without_changing_existing_outputs(
    copy: Path, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    cache, out = tmp_path / "cache", tmp_path / "out"
    assert cli_build(copy, out, cache) == 0
    before = output_bytes(out)
    manifest = cache / "manifest.json"
    manifest.write_text("{")

    assert cli_build(copy, out, cache) == 3

    error = capsys.readouterr().err
    assert str(manifest) in error
    assert "remove the cache directory" in error
    assert output_bytes(out) == before


def test_truncated_cache_facts_refuse_without_changing_existing_outputs(copy: Path, tmp_path: Path) -> None:
    cache, out = tmp_path / "cache", tmp_path / "out"
    assert cli_build(copy, out, cache) == 0
    before = output_bytes(out)
    manifest = json.loads((cache / "manifest.json").read_text())
    facts = cache / "facts" / f"{manifest['src/store.c']['key']}.json"
    facts.write_text("{")

    assert cli_build(copy, out, cache) == 3
    assert output_bytes(out) == before


def test_manifest_entry_without_includes_refuses_without_changing_existing_outputs(copy: Path, tmp_path: Path) -> None:
    cache, out = tmp_path / "cache", tmp_path / "out"
    assert cli_build(copy, out, cache) == 0
    before = output_bytes(out)
    manifest_path = cache / "manifest.json"
    manifest = json.loads(manifest_path.read_text())
    del manifest["src/store.c"]["includes"]
    manifest_path.write_text(json.dumps(manifest))

    assert cli_build(copy, out, cache) == 3
    assert output_bytes(out) == before


def _add_compile_commands(copy: Path, contents: str) -> None:
    configuration = copy / "cgraph.toml"
    configuration.write_text(
        configuration.read_text().replace(
            'contract_graph = "contracts.json"',
            'compile_commands = "compile_commands.json"\ncontract_graph = "contracts.json"',
        )
    )
    (copy / "compile_commands.json").write_text(contents)


def test_non_json_compile_commands_refuse_without_changing_existing_outputs(copy: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    _add_compile_commands(copy, "{")

    assert cli_build(copy, out) == 3
    assert output_bytes(out) == before


def test_compile_commands_entry_without_file_refuses_without_changing_existing_outputs(
    copy: Path, tmp_path: Path
) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    _add_compile_commands(copy, json.dumps([{"directory": str(copy), "arguments": ["clang", "src/store.c"]}]))

    assert cli_build(copy, out) == 3
    assert output_bytes(out) == before


def test_compile_commands_path_outside_c_tree_refuses_without_changing_existing_outputs(
    copy: Path, tmp_path: Path
) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    outside = tmp_path / "outside.c"
    outside.write_text("int outside;\n")
    _add_compile_commands(
        copy,
        json.dumps([{"directory": str(tmp_path), "file": str(outside), "arguments": ["clang", str(outside)]}]),
    )

    assert cli_build(copy, out) == 3
    assert output_bytes(out) == before


def test_non_json_contract_graph_refuses_without_changing_existing_outputs(copy: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    (copy / "contracts.json").write_text("{")

    assert cli_build(copy, out) == 3
    assert output_bytes(out) == before


def _raise_value_error(*args: Any, **kwargs: Any) -> Any:
    raise ValueError("link exploded")


def test_internal_error_prints_traceback_without_changing_existing_outputs(
    copy: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    monkeypatch.setattr(pipeline.link, "build", _raise_value_error)

    assert cli_build(copy, out) == 3

    error = capsys.readouterr().err
    assert "internal error" in error
    assert "Traceback" in error
    assert "link exploded" in error
    assert output_bytes(out) == before


def _exit_worker(job: tuple[str, str, list[str], Any]) -> tuple[str, dict[str, Any]]:
    os._exit(9)


def test_worker_death_returns_four_without_changing_existing_outputs(
    copy: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    out = tmp_path / "out"
    assert cli_build(copy, out) == 0
    before = output_bytes(out)
    monkeypatch.setattr(pipeline, "_extract", _exit_worker)

    assert (
        cli.main(
            [
                "build",
                "--config",
                str(copy / "cgraph.toml"),
                "--part",
                "both",
                "--out",
                str(out),
                "--jobs",
                "2",
            ]
        )
        == 4
    )
    assert output_bytes(out) == before


def test_failing_hard_check_writes_all_outputs(copy: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    configuration = copy / "cgraph.toml"
    configuration.write_text(
        configuration.read_text().replace("min_access_agreement = 0.8", "min_access_agreement = 0.99")
    )

    assert cli.main(cli_arguments(copy, out, part="both")) == 1

    assert all((out / name).is_file() for name in OUTPUTS)
    assert read(out, "checks.json")["passes"] is False


def test_concurrent_cli_builds_share_a_valid_cache(copy: Path, tmp_path: Path) -> None:
    cache = tmp_path / "cache"
    first, second = tmp_path / "first", tmp_path / "second"
    arguments = [cli_arguments(copy, first, cache), cli_arguments(copy, second, cache)]

    with get_context("fork").Pool(2) as workers:
        assert workers.map(cli.main, arguments) == [0, 0]

    assert output_bytes(first) == output_bytes(second)
    manifest = json.loads((cache / "manifest.json").read_text())
    assert set(manifest) == {"src/store.c"}


def test_an_assembly_function_of_a_part_file_is_reported_not_a_node(copy: Path, tmp_path: Path) -> None:
    contracts = json.loads((copy / "contracts.json").read_text())
    contracts["nodes"].append(
        {"id": "Prog/src/store/AnchorPair", "kind": "fn", "syms": ["anchor_pair"], "addr": "", "in_image": True}
    )
    (copy / "contracts.json").write_text(json.dumps(contracts))
    out = tmp_path / "out"
    assert cli.main(["build", "--config", str(copy / "cgraph.toml"), "--part", "store", "--out", str(out)]) == 0
    assert read(out, "graph.json")["analysis"]["contract_functions_without_c"] == ["function:anchor_pair"]
