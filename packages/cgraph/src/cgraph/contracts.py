"""The contract graph: the proof's own graph of functions, the C file each comes from, and the
callback tables and interfaces its tools resolved."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import InputError


def _symbol(name: str) -> str:
    """A symbol without the suffix a compiler adds to a copy (``name.llvm.123``, ``name.cold``)."""
    return name.split(".")[0]


@dataclass
class ContractGraph:
    meta: dict[str, Any] = field(default_factory=dict)
    # (C file, symbol) -> the function node
    functions: dict[tuple[str, str], dict[str, Any]] = field(default_factory=dict)
    nodes: dict[str, dict[str, Any]] = field(default_factory=dict)
    edges: list[dict[str, Any]] = field(default_factory=list)

    @classmethod
    def load(cls, path: Path | None, prefix: str) -> ContractGraph:
        if path is None:
            return cls()
        try:
            data = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError) as error:
            raise InputError(f"cannot read contract graph {path}: {error}") from error
        if not isinstance(data, dict):
            raise InputError(f"contract graph {path} must be a JSON object")
        meta, nodes, edges = data.get("meta", {}), data.get("nodes", []), data.get("edges", [])
        if not isinstance(meta, dict) or not isinstance(nodes, list) or not isinstance(edges, list):
            raise InputError(f"contract graph {path} has malformed meta, nodes, or edges")
        graph = cls(meta=meta)
        for index, node in enumerate(nodes):
            if not isinstance(node, dict) or not isinstance(node.get("id"), str):
                raise InputError(f"contract graph {path} node {index} is malformed")
            graph.nodes[node["id"]] = node
            if node.get("kind") != "fn":
                continue
            source = c_file(node["id"], prefix)
            if source is None:
                continue
            symbols = node.get("syms", [])
            if not isinstance(symbols, list) or not all(isinstance(symbol, str) for symbol in symbols):
                raise InputError(f"contract graph {path} node {index} has malformed syms")
            for symbol in symbols:
                graph.functions.setdefault((source, _symbol(symbol)), node)
        if not all(isinstance(edge, dict) for edge in edges):
            raise InputError(f"contract graph {path} has malformed edges")
        graph.edges = edges
        return graph

    def function(self, source: str, name: str) -> dict[str, Any] | None:
        return self.functions.get((source, name))

    def table_edges(self) -> list[dict[str, Any]]:
        return [edge for edge in self.edges if edge.get("kind") == "table"]

    def binds(self) -> list[dict[str, Any]]:
        return [
            edge for edge in self.edges if edge.get("kind") == "binds" and str(edge.get("dst", "")).startswith("if:")
        ]


def c_file(node_id: str, prefix: str) -> str | None:
    """The C file of a function node: ``<prefix><dir>/<file stem>/<Function>`` gives ``<dir>/<file stem>.c``."""
    if not prefix or not node_id.startswith(prefix):
        return None
    parts = node_id[len(prefix) :].split("/")
    if len(parts) < 2:
        return None
    return "/".join(parts[:-1]) + ".c"
