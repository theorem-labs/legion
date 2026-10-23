"""The contract graph: the proof's own graph of functions, the C file each comes from, and the
callback tables and interfaces its tools resolved."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


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
        data = json.loads(path.read_text())
        graph = cls(meta=data.get("meta", {}))
        for node in data.get("nodes", []):
            graph.nodes[node["id"]] = node
            if node.get("kind") != "fn":
                continue
            source = c_file(node["id"], prefix)
            if source is None:
                continue
            for symbol in node.get("syms", []):
                graph.functions.setdefault((source, _symbol(symbol)), node)
        graph.edges = list(data.get("edges", []))
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
