"""Each layer against a second source, and the hard checks that fail a run.

The layer checks are measurements: inlining, tail calls and code the compiler removes make equality
the wrong criterion for C against machine code. The hard checks are what must hold for the graph to
be used: no parse error, every function and every addressed global of the part present, provenance
on every edge, a layout for every embedding, a C site for every membership, valid and unique ids,
one owner per node, and valid topics.
"""

from __future__ import annotations

import collections
import re
from typing import Any

from .config import Thresholds

ID_PATTERN = re.compile(r"^(function|struct|global|percpu|subsystem):\S+$")
SAMPLE = 50


def _pairs_examples(values: set[tuple[str, str]], limit: int = 5) -> list[dict[str, str]]:
    return [{"source": source, "target": target} for source, target in sorted(values)[:limit]]


def _part_functions(graph: dict[str, Any]) -> set[str]:
    return {
        node["id"]
        for node in graph["nodes"]
        if node["kind"] == "function" and node.get("part") and node.get("image_addresses")
    }


def calls(graph: dict[str, Any]) -> dict[str, Any]:
    """C call sites (after image-guided folding) against ``jal``/``j``/``jalr`` targets."""
    functions = _part_functions(graph)
    analysis = graph["analysis"]
    image_calls = [item for item in analysis["image"]["calls"] if item["source"] in functions]
    image_pairs = {(item["source"], item["target"]) for item in image_calls if item["target"] in functions}
    c_pairs = {
        (item["source"], item["target"])
        for item in analysis["c_calls"]
        if item["source"] in functions and item["target"] in functions
    }
    tails = {(item["source"], item["target"]) for item in image_calls if item["tail"]}
    names = {node["id"]: node["name"] for node in graph["nodes"]}
    only_c, only_image = c_pairs - image_pairs, image_pairs - c_pairs
    pointers = {
        (edge["source"], edge["target"])
        for edge in graph["edges"]
        if edge["kind"] == "call_indirect" and edge["source"] in functions
    }
    weak = {node["id"] for node in graph["nodes"] if node.get("weak_definitions")}
    classes: dict[str, dict[str, list[tuple[str, str]]]] = {
        "only_c": collections.defaultdict(list),
        "only_image": collections.defaultdict(list),
    }
    for pair in sorted(only_c):
        cause = "compiler_builtin" if names.get(pair[1], "").startswith("__builtin_") else "no_image_call"
        classes["only_c"][cause].append(pair)
    for pair in sorted(only_image):
        if pair in pointers:
            # A function pointer the C passes or stores, which the compiler turned into a direct call.
            cause = "devirtualized_callback"
        elif pair[0] in weak:
            cause = "caller_has_weak_alternate"
        elif pair in tails:
            cause = "tail_call"
        else:
            cause = "other"
        classes["only_image"][cause].append(pair)
    c_pointers = {
        (edge["source"], edge["target"])
        for edge in graph["edges"]
        if edge["kind"] == "call_indirect" and edge.get("resolution") == "address_taken" and edge["source"] in functions
    }
    image_pointers = {
        (item["source"], item["target"])
        for item in analysis["image"]["function_addresses"]
        if item["source"] in functions
    }
    sites = sum(
        item["sites"] for item in analysis["c_calls"] if item["source"] in functions and item["target"] in functions
    )
    return {
        "method": "C calls after folding the helpers the image inlines, against direct image calls (jal, j, and jalr "
        "through an auipc-set register), between image functions of the part.",
        "image_functions": len(functions),
        "c_edges": len(c_pairs),
        "c_call_sites": sites,
        "image_edges": len(image_pairs),
        "agree": len(c_pairs & image_pairs),
        "only_c": len(only_c),
        "only_image": len(only_image),
        "disagreements": {
            side: {
                kind: {"count": len(items), "examples": _pairs_examples(set(items))}
                for kind, items in sorted(groups.items())
            }
            for side, groups in classes.items()
        },
        "address_taken": {
            "c_sites": len(c_pointers),
            "image_addresses": len(image_pointers),
            "agree": len(c_pointers & image_pointers),
            "only_c": len(c_pointers - image_pointers),
            "only_image": len(image_pointers - c_pointers),
            "examples": {
                "only_c": _pairs_examples(c_pointers - image_pointers),
                "only_image": _pairs_examples(image_pointers - c_pointers),
            },
        },
        "callback_tables": len(analysis["resolved_callbacks"]),
    }


def accesses(graph: dict[str, Any]) -> dict[str, Any]:
    """C accesses to globals against image addresses inside sized ELF objects named by System.map.

    An image address with several names (a zero-size symbol at the start of another object, such as
    jiffies at jiffies_64) matches a C access to any of them. A C global that has no symbol in the
    image at all (the compiler removed it: an empty lock_class_key, a folded constant) is counted
    apart, because no address can show it."""
    analysis = graph["analysis"]
    functions = sorted(_part_functions(graph))
    c_targets = {
        function: {item["name"]: item for item in analysis["c_targets"].get(function, [])} for function in functions
    }
    image_entries: dict[str, dict[str, tuple[str, frozenset[str]]]] = collections.defaultdict(dict)
    for item in analysis["image"]["globals"]:
        names = frozenset([item["global"], *item.get("aliases", [])])
        known = image_entries[item["function"]].get(item["global"])
        image_entries[item["function"]][item["global"]] = (item["target_class"], names | (known[1] if known else names))
    sized = set(analysis["image"]["sized_symbol_names"])
    all_c = {name for targets in c_targets.values() for name in targets}
    c_total: set[tuple[str, str]] = set()
    matched: set[tuple[str, str]] = set()
    image_total: set[tuple[str, str]] = set()
    c_only: dict[str, list[tuple[str, str]]] = collections.defaultdict(list)
    image_only: dict[str, list[tuple[str, str]]] = collections.defaultdict(list)
    apart: dict[str, list[tuple[str, str]]] = collections.defaultdict(list)
    c_state: set[tuple[str, str]] = set()
    image_only_state = 0
    for function in functions:
        c_values, entries = c_targets.get(function, {}), image_entries.get(function, {})
        image_names = set().union(*(names for _, names in entries.values())) if entries else set()
        for name, item in sorted(c_values.items()):
            pair = (function, name)
            kind = item["target_class"]
            if kind in ("state", "compiler_symbol") and not item.get("image_symbol", True):
                apart[f"{kind}_without_image_symbol"].append(pair)
                continue
            c_total.add(pair)
            if kind == "state":
                c_state.add(pair)
            if name in image_names:
                matched.add(pair)
                continue
            if kind == "state":
                kind = "state_not_addressed_by_image" if name in sized else "state_without_sized_elf_symbol"
            c_only[kind].append(pair)
        for primary, (kind, names) in sorted(entries.items()):
            image_total.add((function, primary))
            if names & set(c_values):
                continue
            if kind == "state":
                image_only_state += 1
                kind = "state_named_by_another_c_function" if names & all_c else "state_not_named_in_c"
            image_only[kind].append((function, primary))
    outside = analysis["image"]["outside_sized_symbols"]
    outside_kinds = collections.Counter(item.get("section", "") for item in outside)
    # Agreement over state on both sides: per-CPU state, compiler symbols and constant tables are
    # named differently by C and the image by design, and are counted apart.
    matched_state = len(c_state & matched)
    either = len(c_state) + image_only_state
    only_image = sum(len(items) for items in image_only.values())
    return {
        "method": "C state units (a field path rooted at a global is that global) against addresses inside the "
        "declared size of an ELF object that System.map names (or a zero-size symbol at the address); other "
        "addresses are counted apart.",
        "c_accesses": len(c_total),
        "image_accesses": len(image_total),
        "agree": len(matched),
        "only_c": len(c_total - matched),
        "only_image": only_image,
        "state_agreement": round(matched_state / either, 6) if either else 1.0,
        "counted_apart": {kind: len(items) for kind, items in sorted(apart.items())},
        "disagreements": {
            "only_c": {
                kind: {"count": len(items), "examples": _pairs_examples(set(items), 3)}
                for kind, items in sorted(c_only.items())
            },
            "only_image": {
                kind: {"count": len(items), "examples": _pairs_examples(set(items), 3)}
                for kind, items in sorted(image_only.items())
            },
            "outside_sized_symbol": {"count": len(outside), "by_section": dict(sorted(outside_kinds.items()))},
            "generic_unconfirmed": len(analysis["generic_unconfirmed"]),
        },
        "pairs": {
            "only_c": [
                {"source": source, "target": target, "class": kind}
                for kind, items in sorted(c_only.items())
                for source, target in items
            ],
            "only_image": [
                {"source": source, "target": target, "class": kind}
                for kind, items in sorted(image_only.items())
                for source, target in items
            ],
            "counted_apart": [
                {"source": source, "target": target, "class": kind}
                for kind, items in sorted(apart.items())
                for source, target in items
            ],
        },
    }


def structure(graph: dict[str, Any]) -> dict[str, Any]:
    """Embeddings against clang's record layout, memberships against their C site, field offsets against the image."""
    analysis = graph["analysis"]
    embeds = [edge for edge in graph["edges"] if edge["kind"] == "embeds"]
    members = [edge for edge in graph["edges"] if edge["kind"] == "member_of"]
    confirmed = [edge for edge in embeds if edge["layout"]["offset"] >= 0 and edge["layout"]["size"] >= 0]
    witnessed = [
        edge
        for edge in members
        if any(item.get("source") == "c" and item.get("line", 0) > 0 for item in edge["provenance"])
    ]
    offsets, constants = analysis["image"]["memory_offsets"], analysis["image"]["addi_constants"]
    global_offsets: dict[str, set[int]] = collections.defaultdict(set)
    for entry in analysis["image"]["globals"]:
        global_offsets[entry["function"]].add(entry["offset"])
    candidates = []
    excluded: collections.Counter[str] = collections.Counter()
    for function, fields in sorted(analysis["field_accesses"].items()):
        if function not in offsets:
            continue
        displacements = set(offsets[function])
        if not displacements - {0}:
            # The machine code makes no memory access with a displacement: nothing to match.
            excluded["no_memory_access_in_image"] += len(fields)
            continue
        for item in fields:
            base = item.get("base", "pointer")
            if base in ("local", "other"):
                # An object on the stack or in registers: the compiler keeps it in registers.
                excluded[f"{base}_object"] += 1
                continue
            offset = item["displacement"] if item.get("displacement", -1) >= 0 else item["offset"]
            if offset < 0 and base != "container_of":
                continue
            size = max(1, item.get("size", 1) or 1)
            if base == "global" and offset in global_offsets[function]:
                method = "global_address_offset"
            elif offset in displacements:
                method = "memory_displacement"
            elif offset in constants.get(function, []):
                method = "addi_field_address"
            elif -offset in constants.get(function, []) or (offset < 0 and -offset in displacements):
                method = "negative_addi_container_offset"
            elif any(offset < value < offset + size for value in displacements):
                method = "narrower_access_inside_field"
            elif item.get("bitfield") and ({offset & ~3, offset & ~7} & displacements):
                method = "bitfield_storage_unit"
            elif offset >= 2048:
                method = "no_match_beyond_12_bit_displacement"
            else:
                method = "no_matching_offset"
            candidate = {
                "function": function,
                "struct": item["struct"],
                "field": item["field"],
                "offset": offset,
                "base": base,
                "matched": not method.startswith("no_"),
                "method": method,
            }
            if not candidate["matched"]:
                candidate["image_memory_offsets"] = offsets[function]
                candidate["image_addi_constants"] = constants.get(function, [])
            candidates.append(candidate)
    sample = candidates[:SAMPLE]
    subsystems = [node for node in graph["nodes"] if node["kind"] == "subsystem"]
    guarded = {lock["lock"] for node in subsystems for lock in node.get("guard_locks", [])}
    locks = sorted(node["id"] for node in graph["nodes"] if node.get("lock") and node["id"] not in guarded)
    return {
        "embeds": {
            "total": len(embeds),
            "layout_confirmed": len(confirmed),
            "unconfirmed": len(embeds) - len(confirmed),
        },
        "member_of": {
            "total": len(members),
            "with_c_site": len(witnessed),
            "without_c_site": len(members) - len(witnessed),
            "by_kind": dict(sorted(collections.Counter(edge["membership"] for edge in members).items())),
        },
        "field_offsets": {
            "candidates": len(candidates),
            "matched": sum(item["matched"] for item in candidates),
            "by_method": dict(sorted(collections.Counter(item["method"] for item in candidates).items())),
            "excluded": dict(sorted(excluded.items())),
            "miss_examples": {
                method: [
                    {key: item[key] for key in ("function", "struct", "field", "offset", "base")}
                    for item in candidates
                    if item["method"] == method
                ][:3]
                for method in sorted({item["method"] for item in candidates if not item["matched"]})
            },
            "sample": {"size": len(sample), "matched": sum(item["matched"] for item in sample), "items": sample},
        },
        "excluded_embedding_bases": analysis["excluded_embedding_bases"],
        "unguarded_global_locks": locks,
        "largest_subsystems": [
            {"id": node["id"], "members": len(node["members"])}
            for node in sorted(subsystems, key=lambda node: (-len(node["members"]), node["id"]))[:10]
        ],
        "method": "A field offset matches when it is a load/store displacement, an addi constant (the field's "
        "address), or a negative addi constant (container_of) in the same image function.",
    }


def eligible_owners(graph: dict[str, Any]) -> set[str]:
    """The nodes that must have exactly one owner, read from the graph alone: every function of the
    part, every subsystem, and every data structure or per-CPU state an edge joins to a function of
    the part or to another data structure."""
    part = {node["id"] for node in graph["nodes"] if node["kind"] == "function" and node.get("part")}
    state = {node["id"] for node in graph["nodes"] if node["kind"] in ("struct", "global", "percpu")}
    joined = set()
    for edge in graph["edges"]:
        if edge["kind"] == "access" and edge["source"] in part and edge["target"] in state:
            joined.add(edge["target"])
        elif edge["source"] in state and edge["target"] in state and edge["source"] != edge["target"]:
            joined |= {edge["source"], edge["target"]}
    return part | joined | {node["id"] for node in graph["nodes"] if node["kind"] == "subsystem"}


def agreement(layer: dict[str, Any], agree: str, left: str, right: str) -> float:
    """|both| / |either| of a layer check: 1.0 when both sides are empty."""
    union = layer[left] + layer[right] - layer[agree]
    return layer[agree] / union if union else 1.0


def _absolute_paths(value: Any, found: list[str]) -> None:
    if isinstance(value, str):
        if value.startswith("/") and len(found) < 5:
            found.append(value)
    elif isinstance(value, dict):
        for item in value.values():
            _absolute_paths(item, found)
    elif isinstance(value, list):
        for item in value:
            _absolute_paths(item, found)


def hard(
    graph: dict[str, Any],
    owners: dict[str, Any],
    topics: dict[str, Any],
    layers: dict[str, dict[str, Any]],
    thresholds: Thresholds,
) -> list[dict[str, Any]]:
    analysis = graph["analysis"]
    structure_result = layers["structure_layer"]
    node_ids = [node["id"] for node in graph["nodes"]]
    part = _part_functions(graph)
    missing_functions = sorted(set(analysis["expected_functions"]) - part)
    state_names = {node["name"] for node in graph["nodes"] if node["kind"] in ("global", "percpu")}
    addressed_entries = [
        frozenset([item["global"], *item.get("aliases", [])])
        for item in analysis["image"]["globals"]
        if item["target_class"] == "state"
    ]
    addressed = {min(names) for names in addressed_entries}
    missing_globals = sorted({min(names) for names in addressed_entries if not names & state_names})
    no_provenance = [
        f"{item['kind']} {item.get('id') or item['source'] + '->' + item['target']}"
        for item in graph["nodes"] + graph["edges"]
        if not item["provenance"]
    ]
    no_layout = [
        f"{edge['source']}->{edge['target']}"
        for edge in graph["edges"]
        if edge["kind"] in ("embeds", "embeds_base") and (edge["layout"]["offset"] < 0 or edge["layout"]["size"] < 0)
    ]
    c_files = set(graph["files"])
    no_site = [
        f"{edge['source']}->{edge['target']}"
        for edge in graph["edges"]
        if edge["kind"] == "member_of"
        and not any(
            item.get("source") == "c" and item.get("line", 0) > 0 and item.get("file") for item in edge["provenance"]
        )
    ]
    bad_ids = sorted({identity for identity in node_ids if not ID_PATTERN.match(identity)})
    duplicates = sorted(identity for identity, count in collections.Counter(node_ids).items() if count > 1)
    owner_counts = collections.Counter(owner["id"] for owner in owners["owners"])
    must_own = eligible_owners(graph)
    unowned = sorted(identity for identity in must_own if owner_counts[identity] != 1)
    stray = sorted(identity for identity in owner_counts if identity not in must_own)
    bad_topics = sorted({topic for items in topics["owners"].values() for topic in items if not valid_subject(topic)})
    untopiced = sorted(identity for identity in owner_counts if not topics["owners"].get(identity))
    absolute: list[str] = []
    for document in (graph, owners, topics, layers):
        _absolute_paths(document, absolute)
    offsets = structure_result["field_offsets"]
    offset_share = offsets["matched"] / offsets["candidates"] if offsets["candidates"] else 1.0
    sample = offsets["sample"]
    sample_share = sample["matched"] / sample["size"] if sample["size"] else 1.0
    call_share = agreement(layers["call_layer"], "agree", "c_edges", "image_edges")
    access_share = layers["access_layer"]["state_agreement"]
    checks = [
        ("no C parse errors", not analysis["parse_errors"], f"{len(analysis['parse_errors'])} files with errors"),
        (
            "every function of the part's files that the image holds is a node",
            not missing_functions,
            f"{len(part)} nodes; missing {missing_functions[:10]}",
        ),
        (
            "every global the part's image code addresses is a node",
            not missing_globals,
            f"{len(addressed)} addressed; missing {missing_globals[:10]}",
        ),
        ("every node and edge has provenance", not no_provenance, f"{len(graph['edges'])} edges; {no_provenance[:5]}"),
        ("every embedding has a clang layout", not no_layout, f"without a layout: {no_layout[:5]}"),
        ("every membership has a C site", not no_site, f"{len(c_files)} C files; without a site: {no_site[:5]}"),
        ("node ids are valid and unique", not bad_ids and not duplicates, f"invalid {bad_ids[:5]}; twice {duplicates[:5]}"),
        (
            "one owner for each function, data structure and subsystem",
            not unowned and not stray,
            f"{len(owner_counts)} owners; not exactly one {unowned[:5]}; not a node to own {stray[:5]}",
        ),
        ("every owner has topics, each a valid subject", not bad_topics and not untopiced,
         f"invalid {bad_topics[:5]}; none {untopiced[:5]}"),
        ("no absolute path in any output", not absolute, f"{absolute[:5]}"),
        (
            f"call agreement at least {thresholds.call_agreement:.2f}",
            call_share >= thresholds.call_agreement,
            f"{call_share:.4f}",
        ),
        (
            f"access agreement at least {thresholds.access_agreement:.2f}",
            access_share >= thresholds.access_agreement,
            f"{access_share:.4f}",
        ),
        (
            f"field offsets at least {thresholds.field_offsets:.2f}, in all and in the first {SAMPLE}",
            offset_share >= thresholds.field_offsets and sample_share >= thresholds.field_offsets,
            f"all {offsets['matched']}/{offsets['candidates']} = {offset_share:.4f}; "
            f"first {sample['size']}: {sample['matched']} = {sample_share:.4f}",
        ),
    ]  # fmt: skip
    return [{"name": name, "ok": bool(ok), "detail": detail} for name, ok, detail in checks]


def valid_subject(subject: str) -> bool:
    """A subject Envoy's bus accepts: no empty token, no whitespace, no wildcard."""
    return (
        bool(subject)
        and not subject.startswith(".")
        and not subject.endswith(".")
        and ".." not in subject
        and not any(character in subject for character in " \t\r\n*>")
    )
