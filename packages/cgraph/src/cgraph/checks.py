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
    classes: dict[str, dict[str, list[tuple[str, str]]]] = {
        "only_c": collections.defaultdict(list),
        "only_image": collections.defaultdict(list),
    }
    for pair in sorted(only_c):
        classes["only_c"][
            "compiler_builtin" if names.get(pair[1], "").startswith("__builtin_") else "no_image_call"
        ].append(pair)
    for pair in sorted(only_image):
        classes["only_image"]["tail_call" if pair in tails else "other"].append(pair)
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
    """C accesses to globals against image addresses inside sized ELF objects named by System.map."""
    analysis = graph["analysis"]
    functions = sorted(_part_functions(graph))
    c_targets = {
        function: {item["name"]: item["target_class"] for item in analysis["c_targets"].get(function, [])}
        for function in functions
    }
    image_targets: dict[str, dict[str, str]] = collections.defaultdict(dict)
    for item in analysis["image"]["globals"]:
        image_targets[item["function"]][item["global"]] = item["target_class"]
    sized = set(analysis["image"]["sized_symbol_names"])
    all_c = {name for targets in c_targets.values() for name in targets}
    c_total: set[tuple[str, str]] = set()
    image_total: set[tuple[str, str]] = set()
    c_only: dict[str, list[tuple[str, str]]] = collections.defaultdict(list)
    image_only: dict[str, list[tuple[str, str]]] = collections.defaultdict(list)
    for function in functions:
        c_values, image_values = c_targets.get(function, {}), image_targets.get(function, {})
        c_total |= {(function, name) for name in c_values}
        image_total |= {(function, name) for name in image_values}
        for name in sorted(set(c_values) - set(image_values)):
            kind = c_values[name]
            if kind == "state":
                kind = "state_not_addressed_by_image" if name in sized else "state_without_sized_elf_symbol"
            c_only[kind].append((function, name))
        for name in sorted(set(image_values) - set(c_values)):
            kind = image_values[name]
            if kind == "state":
                kind = "state_named_by_another_c_function" if name in all_c else "state_not_named_in_c"
            image_only[kind].append((function, name))
    outside = analysis["image"]["outside_sized_symbols"]
    outside_kinds = collections.Counter(item.get("section", "") for item in outside)
    return {
        "method": "C state units (a field path rooted at a global is that global) against addresses inside the "
        "declared size of an ELF object that System.map names; other addresses are counted apart.",
        "c_accesses": len(c_total),
        "image_accesses": len(image_total),
        "agree": len(c_total & image_total),
        "only_c": len(c_total - image_total),
        "only_image": len(image_total - c_total),
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
    candidates = []
    for function, fields in sorted(analysis["field_accesses"].items()):
        if function not in offsets:
            continue
        for item in fields:
            offset = item["offset"]
            if offset < 0:
                continue
            if offset in offsets[function]:
                method = "memory_displacement"
            elif offset in constants.get(function, []):
                method = "addi_field_address"
            elif -offset in constants.get(function, []):
                method = "negative_addi_container_offset"
            else:
                method = "no_matching_offset"
            candidate = {
                "function": function,
                "struct": item["struct"],
                "field": item["field"],
                "offset": offset,
                "matched": method != "no_matching_offset",
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


def hard(
    graph: dict[str, Any], owners: dict[str, Any], topics: dict[str, Any], structure_result: dict[str, Any]
) -> list[dict[str, Any]]:
    analysis = graph["analysis"]
    node_ids = [node["id"] for node in graph["nodes"]]
    part = _part_functions(graph)
    missing_functions = sorted(set(analysis["expected_functions"]) - part)
    state_names = {node["name"] for node in graph["nodes"] if node["kind"] in ("global", "percpu")}
    addressed = {item["global"] for item in analysis["image"]["globals"] if item["target_class"] == "state"}
    missing_globals = sorted(addressed - state_names)
    no_provenance = [
        f"{edge['kind']} {edge['source']}->{edge['target']}" for edge in graph["edges"] if not edge["provenance"]
    ]
    bad_ids = sorted({identity for identity in node_ids if not ID_PATTERN.match(identity)})
    duplicates = sorted(identity for identity, count in collections.Counter(node_ids).items() if count > 1)
    owner_ids = [owner["id"] for owner in owners["owners"]]
    owner_counts = collections.Counter(owner_ids)
    unowned = sorted(set(owners["eligible"]) - set(owner_ids))
    twice = sorted(identity for identity, count in owner_counts.items() if count > 1)
    bad_topics = sorted({topic for items in topics["owners"].values() for topic in items if not valid_subject(topic)})
    sample = structure_result["field_offsets"]["sample"]
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
        ("every edge has provenance", not no_provenance, f"{len(graph['edges'])} edges; {no_provenance[:5]}"),
        (
            "every embedding has a clang layout",
            structure_result["embeds"]["unconfirmed"] == 0,
            str(structure_result["embeds"]),
        ),
        (
            "every membership has a C site",
            structure_result["member_of"]["without_c_site"] == 0,
            str({key: value for key, value in structure_result["member_of"].items() if key != "by_kind"}),
        ),
        (
            "field offsets match the image",
            sample["size"] == 0 or sample["matched"] > 0,
            f"{sample['matched']}/{sample['size']} sampled",
        ),
        (
            "node ids are valid and unique",
            not bad_ids and not duplicates,
            f"invalid {bad_ids[:5]}; twice {duplicates[:5]}",
        ),
        (
            "one owner for each function, data structure and subsystem",
            not unowned and not twice,
            f"{len(owner_ids)} owners; unowned {unowned[:5]}; twice {twice[:5]}",
        ),
        ("every topic is a valid subject", not bad_topics, f"invalid {bad_topics[:5]}"),
    ]
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
