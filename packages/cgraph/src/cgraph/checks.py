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

from . import jsonio
from .config import Thresholds

# Contract C1, matching Go's `^(function|struct|global|percpu|subsystem):\S+$` exactly. Python's
# `fullmatch` avoids `$` accepting a final newline and the explicit whitespace class avoids Unicode
# whitespace that Go's RE2 `\S` accepts.
ID_PATTERN = re.compile(r"(function|struct|global|percpu|subsystem):[^\t\n\f\r ]+")
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
        "pairs": {
            side: [
                {"source": source, "target": target, "class": kind}
                for kind, items in sorted(groups.items())
                for source, target in items
            ]
            for side, groups in classes.items()
        },
        "address_taken": {
            "c_sites": len(c_pointers),
            "image_addresses": len(image_pointers),
            "agree": len(c_pointers & image_pointers),
            "only_c": len(c_pointers - image_pointers),
            "only_image": len(image_pointers - c_pointers),
            "pairs": {
                "only_c": [
                    {"source": source, "target": target, "class": "not_address_taken_by_image"}
                    for source, target in sorted(c_pointers - image_pointers)
                ],
                "only_image": [
                    {"source": source, "target": target, "class": "not_address_taken_by_c"}
                    for source, target in sorted(image_pointers - c_pointers)
                ],
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
        # A name at the start of a merged-globals block the machine code forms: its address is formed.
        image_names |= set(analysis["image"].get("block_start_names", {}).get(function, []))
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
            "generic_unconfirmed": analysis.get("excluded", {}).get("access", {}).get("generic_unconfirmed", 0),
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


def _register_relative(
    offset: int, size: int, item: dict[str, Any], displacements: set[int], constants: list[int]
) -> str | None:
    """How the machine code reaches a field ``offset`` bytes from a base register, if it does."""
    if offset in displacements:
        return "memory_displacement"
    if offset in constants:
        return "addi_field_address"
    if -offset in constants or (offset < 0 and -offset in displacements):
        return "negative_addi_container_offset"
    if any(offset < value < offset + size for value in displacements):
        return "narrower_access_inside_field"
    if item.get("bitfield") and ({offset & ~3, offset & ~7} & displacements):
        return "bitfield_storage_unit"
    return None


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
    # Each function's image addresses into each global, by every name of the global.
    image_offsets: dict[str, dict[str, set[int]]] = collections.defaultdict(lambda: collections.defaultdict(set))
    for entry in analysis["image"]["globals"]:
        global_offsets[entry["function"]].add(entry["offset"])
        for name in {entry["global"], *entry.get("aliases", [])}:
            image_offsets[entry["function"]][name].add(entry["offset"])
    # Where each struct sits inside each global the function's C names (link, from clang's layouts).
    places: dict[str, dict[str, list[tuple[str, list[int]]]]] = collections.defaultdict(
        lambda: collections.defaultdict(list)
    )
    for function, entries in analysis.get("global_struct_offsets", {}).items():
        for entry in entries:
            places[function][entry["struct"]].append((entry["global"], entry["offsets"]))
    # The sizes clang gives each struct name (a name some files define differently has several).
    struct_sizes: dict[str, set[int]] = collections.defaultdict(set)
    for node in graph["nodes"]:
        if node["kind"] == "struct" and node.get("size", -1) > 0:
            struct_sizes[node["name"]].add(node["size"])
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
            displacement = item.get("displacement", -1)
            # A container_of displacement is negative (the member's offset subtracted): match it as is.
            if displacement >= 0 or (base == "container_of" and displacement < -1):
                offset = displacement
            else:
                offset = item["offset"]
            if offset < 0 and base != "container_of":
                continue
            size = max(1, item.get("size", 1) or 1)
            # Where the image may keep an inner address in a register instead of the folded sum: the
            # element of a variable index (``element_displacement``), a rebased helper's member, a
            # global's member (the field's own offset), or the container itself where container_of
            # cancels against a pointer the code already holds (the field's own offset).
            alternates = [offset]
            if item.get("element_displacement") is not None:
                alternates.append(item["element_displacement"])
            if item.get("inner_displacement") is not None:
                alternates.append(item["inner_displacement"])
            if base in ("global", "container_of") and item.get("offset", -1) >= 0:
                alternates.append(item["offset"])
            register = next(
                (
                    found
                    for candidate_offset in alternates
                    if (
                        found := _register_relative(
                            candidate_offset, size, item, displacements, constants.get(function, [])
                        )
                    )
                ),
                None,
            )
            if base == "global" and offset in global_offsets[function]:
                method = "global_address_offset"
            elif register is not None:
                method = register
            elif offset == 0 and item.get("address"):
                # &p->m (or an array member decaying to a pointer) at offset 0 is the base pointer itself.
                method = "member_address_at_zero"
            elif item.get("address") and any(
                candidate <= value < candidate + item.get("element_size", 0)
                for candidate in alternates
                for value in displacements
            ):
                # An array member's decayed address (info = same->info) is folded into the accesses
                # the code makes through it: the first element's bytes.
                method = "array_member_first_element"
            elif any(
                own + place in image_offsets[function].get(name, ())
                for own in [item["offset"] if item.get("offset", -1) >= 0 else offset]
                for name, at in places[function][item["struct"]]
                for place in at
            ):
                # The C names a global that is (or holds) the struct, and the image addresses it where
                # the field sits: the field's own offset in its struct at a place that struct has in the
                # global (a "." chain's displacement is from an outer struct, whose place is covered).
                method = "global_object_field"
            elif any(
                own + place <= value < own + place + size
                for own in [item["offset"] if item.get("offset", -1) >= 0 else offset]
                for name, at in places[function][item["struct"]]
                for value in image_offsets[function].get(name, ())
                for place in at
            ):
                method = "global_object_narrower_access"
            elif base == "pointer" and any(
                _register_relative(own - step, size, item, displacements, [])
                or (item.get("address") and own - step in constants.get(function, []))
                for own in [item["offset"] if item.get("offset", -1) >= 0 else offset]
                for step in struct_sizes.get(item["struct"], ())
                if step in constants.get(function, [])
            ):
                # A loop walking an array (for (i = table; …; i++) i->data) whose code keeps i + 1 in a
                # register and steps it by the element's size: the field is that size before it.
                method = "next_element_displacement"
            elif offset >= 2048:
                method = "no_match_beyond_12_bit_displacement"
            else:
                method = "no_matching_offset"
            if method.startswith("no_") and item.get("conditional"):
                # Under a __builtin_constant_p choice the compiler made per call site: the image
                # decides whether this code was compiled.
                excluded["image_decided"] += 1
                continue
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
            "misses": [
                {key: item[key] for key in ("function", "struct", "field", "offset", "base", "method")}
                for item in candidates
                if not item["matched"]
            ],
            "image_evidence": {
                function: {
                    "memory_offsets": offsets[function],
                    "addi_constants": constants.get(function, []),
                }
                for function in sorted({item["function"] for item in candidates if not item["matched"]})
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
        if value.startswith("/"):
            found.append(value)
    elif isinstance(value, dict):
        for key in sorted(value):
            _absolute_paths(value[key], found)
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
    no_provenance = sorted(
        f"{item['kind']} {item.get('id') or item['source'] + '->' + item['target']}"
        for item in graph["nodes"] + graph["edges"]
        if not item["provenance"]
    )
    no_layout = sorted(
        f"{edge['source']}->{edge['target']}"
        for edge in graph["edges"]
        if edge["kind"] in ("embeds", "embeds_base") and (edge["layout"]["offset"] < 0 or edge["layout"]["size"] < 0)
    )
    c_files = set(graph["files"])
    no_site = sorted(
        f"{edge['source']}->{edge['target']}"
        for edge in graph["edges"]
        if edge["kind"] == "member_of"
        and not any(
            item.get("source") == "c" and item.get("line", 0) > 0 and item.get("file") for item in edge["provenance"]
        )
    )
    bad_ids = sorted({identity for identity in node_ids if not ID_PATTERN.fullmatch(identity)})
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
    absolute.sort()
    offsets = structure_result["field_offsets"]
    offset_share = offsets["matched"] / offsets["candidates"] if offsets["candidates"] else 1.0
    sample = offsets["sample"]
    sample_share = sample["matched"] / sample["size"] if sample["size"] else 1.0
    call_share = agreement(layers["call_layer"], "agree", "c_edges", "image_edges")
    access_share = layers["access_layer"]["state_agreement"]
    contract_duplicates = analysis.get("contract_mapping_duplicates", [])
    checks = [
        (
            "no C parse errors",
            not analysis["parse_errors"],
            f"{len(analysis['parse_errors'])} files with errors",
            [{"file": file, "errors": errors} for file, errors in sorted(analysis["parse_errors"].items())],
        ),
        (
            "every function of the part's files that the image holds is a node",
            not missing_functions,
            f"{len(part)} nodes; missing {missing_functions[:10]}",
            missing_functions,
        ),
        (
            "every global the part's image code addresses is a node",
            not missing_globals,
            f"{len(addressed)} addressed; missing {missing_globals[:10]}",
            missing_globals,
        ),
        ("every node and edge has provenance", not no_provenance, f"{len(graph['edges'])} edges; {no_provenance[:5]}", no_provenance),
        ("every embedding has a clang layout", not no_layout, f"without a layout: {no_layout[:5]}", no_layout),
        ("every membership has a C site", not no_site, f"{len(c_files)} C files; without a site: {no_site[:5]}", no_site),
        (
            "node ids are valid and unique",
            not bad_ids and not duplicates,
            f"invalid {bad_ids[:5]}; twice {duplicates[:5]}",
            [{"invalid": identity} for identity in bad_ids] + [{"duplicate": identity} for identity in duplicates],
        ),
        (
            "one owner for each function, data structure and subsystem",
            not unowned and not stray,
            f"{len(owner_counts)} owners; not exactly one {unowned[:5]}; not a node to own {stray[:5]}",
            [{"not_exactly_one": identity} for identity in unowned] + [{"not_eligible": identity} for identity in stray],
        ),
        (
            "every owner has topics, each a valid subject",
            not bad_topics and not untopiced,
            f"invalid {bad_topics[:5]}; none {untopiced[:5]}",
            [{"invalid": topic} for topic in bad_topics] + [{"none": identity} for identity in untopiced],
        ),
        ("no absolute path in any output", not absolute, f"{absolute[:5]}", absolute),
        (
            "contract_mapping_unique",
            not contract_duplicates,
            f"{len(contract_duplicates)} duplicate contract mappings: {contract_duplicates}",
            contract_duplicates,
        ),
        (
            f"call agreement at least {thresholds.call_agreement:.2f}",
            call_share >= thresholds.call_agreement,
            f"{call_share:.4f}",
            [] if call_share >= thresholds.call_agreement else [{"measured": call_share, "threshold": thresholds.call_agreement}],
        ),
        (
            f"access agreement at least {thresholds.access_agreement:.2f}",
            access_share >= thresholds.access_agreement,
            f"{access_share:.4f}",
            [] if access_share >= thresholds.access_agreement else [{"measured": access_share, "threshold": thresholds.access_agreement}],
        ),
        (
            f"field offsets at least {thresholds.field_offsets:.2f}, in all and in the first {SAMPLE}",
            offset_share >= thresholds.field_offsets and sample_share >= thresholds.field_offsets,
            f"all {offsets['matched']}/{offsets['candidates']} = {offset_share:.4f}; "
            f"first {sample['size']}: {sample['matched']} = {sample_share:.4f}",
            []
            if offset_share >= thresholds.field_offsets and sample_share >= thresholds.field_offsets
            else [
                {
                    "all": offset_share,
                    "first_sample": sample_share,
                    "threshold": thresholds.field_offsets,
                }
            ],
        ),
    ]  # fmt: skip
    return [
        {
            "name": name,
            "ok": bool(ok),
            "detail": detail,
            "count": len(items),
            "items": sorted(items, key=jsonio.canonical),
        }
        for name, ok, detail, items in checks
    ]


def valid_subject(subject: str) -> bool:
    """A subject Envoy's bus accepts: no empty token, no whitespace, no wildcard, at most 4,032 bytes."""
    return (
        bool(subject)
        and len(subject.encode()) <= 4032
        and not subject.startswith(".")
        and not subject.endswith(".")
        and ".." not in subject
        and not any(character in subject for character in " \t\r\n*>")
    )
