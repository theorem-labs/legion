"""The run's report in Markdown: what the graph holds, each layer against its second source, the
hard checks, the owners, and the measures."""

from __future__ import annotations

import collections
from typing import Any

from . import jsonio


def _counts(items: list[dict[str, Any]], key: str) -> str:
    counter = collections.Counter(item[key] for item in items)
    return ", ".join(f"{name} {count}" for name, count in sorted(counter.items()))


def _classes(groups: dict[str, Any]) -> str:
    return ", ".join(f"{name} {value['count']}" for name, value in groups.items()) or "none"


def render(
    graph: dict[str, Any],
    checks: dict[str, Any],
    owners: dict[str, Any],
    topics: dict[str, Any],
    measures: dict[str, Any],
    exclusions: dict[str, Any] | None = None,
) -> str:
    nodes, edges = graph["nodes"], graph["edges"]
    accesses = [edge for edge in edges if edge["kind"] == "access"]
    call, access, structure = checks["call_layer"], checks["access_layer"], checks["structure_layer"]
    fields = structure["field_offsets"]
    groups = measures["groups"]
    pairs = measures["pairs_sharing_written_state"]
    neighbours = measures["owner_neighbors"]
    subscriptions = [len(items) for items in topics["owners"].values()]
    exclusion_document = exclusions or {"counts": checks.get("exclusions", {}), "items": []}
    exclusion_lines = []
    for kind, reasons in sorted(exclusion_document["counts"].items()):
        exclusion_lines.append(
            f"- `{kind}`: {', '.join(f'{reason} {count}' for reason, count in sorted(reasons.items()))}."
        )
    for kind, reasons in sorted(exclusion_document["counts"].items()):
        for reason in sorted(reasons):
            items = [item for item in exclusion_document["items"] if item["kind"] == kind and item["reason"] == reason]
            exclusion_lines.extend(f"  - `{kind}` / `{reason}`: `{jsonio.canonical(item)}`" for item in items[:10])
    if exclusion_lines:
        exclusion_lines = ["## Exclusions", "", *exclusion_lines, ""]
    lines = [
        f"# The graph of part `{graph['part']}`",
        "",
        f"{len(graph['files'])} C files; image SHA-256 `{graph['inputs']['image']}`; C sources "
        f"`{graph['inputs']['c_sources']}`.",
        "",
        "## What the graph holds",
        "",
        f"- Nodes: {len(nodes)} ({_counts(nodes, 'kind')}); {measures['functions']} functions of the part are in the image.",
        f"- Edges: {len(edges)} ({_counts(edges, 'kind')}).",
        f"- Access edges by kind: {_counts(accesses, 'access')}.",
        f"- Groups of data structures: {groups['total']}, of which {groups['subsystems']} subsystems "
        f"(two or more members) and {groups['singletons']} single data structures.",
        "",
        *exclusion_lines,
        "## Each layer against a second source",
        "",
        "| Layer | Result |",
        "| --- | --- |",
        f"| Calls (C after folding, against the image) | {call['agree']} agree; {call['only_c']} only in C "
        f"({_classes(call['disagreements']['only_c'])}); {call['only_image']} only in the image "
        f"({_classes(call['disagreements']['only_image'])}). Function addresses passed: C {call['address_taken']['c_sites']}, "
        f"image {call['address_taken']['image_addresses']}, agree {call['address_taken']['agree']}. "
        f"Callback-table edges: {call['callback_tables']}. |",
        f"| Accesses to globals (C against image addresses in sized symbols) | {access['agree']} agree of "
        f"{access['image_accesses']} in the image and {access['c_accesses']} in C; only in C: "
        f"{_classes(access['disagreements']['only_c'])}; only in the image: "
        f"{_classes(access['disagreements']['only_image'])}; addresses outside a sized symbol: "
        f"{access['disagreements']['outside_sized_symbol']['count']}. |",
        f"| Embeddings (against clang's record layout) | {structure['embeds']['layout_confirmed']} of "
        f"{structure['embeds']['total']} have a layout; memberships with a C site: "
        f"{structure['member_of']['with_c_site']} of {structure['member_of']['total']}. |",
        f"| Field offsets (against the image) | {fields['matched']} of {fields['candidates']} match; first "
        f"{fields['sample']['size']}: {fields['sample']['matched']}. |",
        "",
        "## Hard checks",
        "",
        *[f"- {'PASS' if item['ok'] else 'FAIL'}: {item['name']} ({item['detail']})" for item in checks["hard_checks"]],
        "",
        "## Owners and topics",
        "",
        f"- Owners: {len(owners['owners'])} ({', '.join(f'{role} {count}' for role, count in owners['counts'].items())}).",
        f"- Topic subscriptions: {sum(subscriptions)}; per owner median {sorted(subscriptions)[len(subscriptions) // 2] if subscriptions else 0}, "
        f"max {max(subscriptions, default=0)}.",
        f"- Neighbouring owners (functions and data structures): median {neighbours['median']}, 90th percentile "
        f"{neighbours['p90']}, max {neighbours['max']} ({neighbours['largest'][0]['owner'] if neighbours['largest'] else '-'}).",
        "",
        "## Measures",
        "",
        f"- Function pairs that share a group one of them writes (per-CPU state left out): {pairs['pairs']}; "
        f"with a call path: {pairs['with_call_path']} ({pairs['share_with_call_path']:.1%}).",
        f"- C call sites between functions of the part: {measures['calls_sharing_state']['call_sites']}; "
        f"of them sharing a group: {measures['calls_sharing_state']['sharing']}.",
        *[
            f"- Subsystem `{item['id']}`: {len(item['members'])} members, {item['users']} users, {item['writers']} writers."
            for item in measures["subsystems_by_users"][:5]
        ],
    ]
    interfaces = measures.get("interfaces")
    if interfaces:
        lines.append(
            f"- Interfaces the part's functions take: {interfaces['interfaces']}; naming exactly one group "
            f"{interfaces['exactly_one']}, several {interfaces['several']}, none {interfaces['none']}."
        )
    lines.append("")
    return "\n".join(lines)
