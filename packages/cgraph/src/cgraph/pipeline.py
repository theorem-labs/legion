"""One build: facts of every unit of the part (from the cache or parsed), the link, the checks,
the owners and their topics, the measures, and the report."""

from __future__ import annotations

import hashlib
import multiprocessing
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import checks, clangfacts, commands, jsonio, link, measures, owners, report
from .cache import Cache, Contents, sources_digest
from .config import Config, part_files
from .contracts import ContractGraph
from .image import Image

VERSION = "cgraph/1"


class InputError(RuntimeError):
    """An input that cannot be read."""


@dataclass
class Result:
    passes: bool
    hard_checks: list[dict[str, Any]]
    parsed: list[str] = field(default_factory=list)
    reused: list[str] = field(default_factory=list)
    outputs: dict[str, str] = field(default_factory=dict)
    counts: dict[str, Any] = field(default_factory=dict)


def _extract(job: tuple[str, str, list[str], Any]) -> tuple[str, dict[str, Any]]:
    source, tree, arguments, vocabulary = job
    return source, clangfacts.extract(source, Path(tree), arguments, vocabulary)


def _sha256(path: Path | None) -> str | None:
    if path is None:
        return None
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as error:
        raise InputError(f"cannot read {path}: {error}") from error


def run(config: Config, part: str, out: Path, cache_directory: Path | None = None, jobs: int = 1) -> Result:
    inputs = config.inputs
    for name, path in (
        ("c_tree", inputs.c_tree),
        ("image", inputs.image),
        ("system_map", inputs.system_map),
        ("compile_commands", inputs.compile_commands),
        ("contract_graph", inputs.contract_graph),
    ):
        if path is not None and not path.exists():
            raise InputError(f"inputs.{name} {path} does not exist")
    database = (
        commands.load_database(inputs.compile_commands, inputs.c_tree, config.argument_rewrites)
        if inputs.compile_commands
        else None
    )
    files = part_files(config, part, database)
    for source in files:
        if not (inputs.c_tree / source).exists():
            raise InputError(f"{source} is not in the C tree {inputs.c_tree}")
    arguments = {
        source: database[source]
        if database is not None
        else commands.rewrite(
            commands.template_arguments(config.flags, inputs.c_tree, source), config.argument_rewrites
        )
        for source in files
    }
    cache = Cache(cache_directory)
    contents = Contents(inputs.c_tree)
    digest = config.vocabulary.digest()
    facts: dict[str, dict[str, Any]] = {}
    pending = []
    for source in files:
        cached = cache.lookup(source, arguments[source], digest, contents)
        if cached is not None:
            facts[source] = cached
        else:
            pending.append((source, str(inputs.c_tree), arguments[source], config.vocabulary))
    if pending:
        if jobs > 1 and len(pending) > 1:
            with multiprocessing.get_context("fork").Pool(min(jobs, len(pending))) as pool:
                parsed = pool.map(_extract, pending, chunksize=1)
        else:
            parsed = [_extract(job) for job in pending]
        for source, unit in parsed:
            facts[source] = unit
            cache.store(source, arguments[source], digest, unit, contents)
    cache.save()
    image = Image(inputs.image, inputs.system_map, config.image)
    try:
        contracts = ContractGraph.load(inputs.contract_graph, config.contract_function_prefix)
        recorded = {
            "tool": VERSION,
            "c_sources": sources_digest(facts, contents),
            "image": _sha256(inputs.image),
            "system_map": _sha256(inputs.system_map),
            "compile_commands_files": len(database) if database is not None else None,
            "contract_graph": _sha256(inputs.contract_graph),
            # The contract graph's own record, without the paths of the machine that made it.
            "contract_graph_meta": {
                key: value
                for key, value in sorted(contracts.meta.items())
                if not (isinstance(value, str) and value.startswith("/"))
            }
            or None,
            "vocabulary": digest,
        }
        graph = link.build(config, part, files, facts, image, contracts, recorded)
    finally:
        image.close()
    owner_list, topics = owners.build(graph, config.topic_prefix)
    numbers = measures.build(graph, contracts, inputs.contract_source)
    layers = {
        "call_layer": checks.calls(graph),
        "access_layer": checks.accesses(graph),
        "structure_layer": checks.structure(graph),
    }
    hard = checks.hard(graph, owner_list, topics, layers, config.part_thresholds.get(part, config.thresholds))
    passes = all(item["ok"] for item in hard)
    check_document = {"format": "cgraph/checks/v1", "part": part, **layers, "hard_checks": hard, "passes": passes}
    outputs = {
        "graph.json": graph,
        "checks.json": check_document,
        "owners.json": owner_list,
        "topics.json": topics,
        "measures.json": numbers,
    }
    out.mkdir(parents=True, exist_ok=True)
    for name, value in outputs.items():
        jsonio.write(out / name, value)
    (out / "report.md").write_text(report.render(graph, check_document, owner_list, topics, numbers))
    parsed_sources = sorted(job[0] for job in pending)
    counts = {
        "nodes": len(graph["nodes"]),
        "edges": len(graph["edges"]),
        "owners": len(owner_list["owners"]),
        "files": len(files),
    }
    return Result(
        passes=passes,
        hard_checks=hard,
        parsed=parsed_sources,
        reused=sorted(set(files) - set(parsed_sources)),
        outputs={name: str(out / name) for name in [*outputs, "report.md"]},
        counts=counts,
    )


def default_jobs() -> int:
    return max(1, min(48, (os.cpu_count() or 2) // 2))
