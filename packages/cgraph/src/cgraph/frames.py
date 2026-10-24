"""The field accesses the structure check cannot hold against an image displacement, and the local
objects it holds against the frame.

A field of a local object (``struct alloc_context ac``, or ``&ac`` passed to a helper the image
inlines) is not reached from a base register: the compiler keeps the object in registers, or in a
frame slot it addresses from sp or s0 (image.py's ``frame_*`` facts, offsets from the value sp had on
entry). An object's slot B is where the image's call passes the frame address B + d in argument
register i and the C passes ``&object + d`` as argument i to that callee (link's
``object_arguments`` against the image's ``frame_arguments``; a struct local's cleanup gets its
address too); its fields are then held at B plus their displacement.

Without such a call the frame shows nothing the fields alone decide: a frame address the code forms
where every accessed field is loaded or stored fits another struct's fields by chance 19% of the time
with two fields and 7% with four (kernel image, ``frames.decoys``' method). Such an object is
recorded as an exclusion, as is every other access no displacement can show: a zero-size member
(``struct pin_cookie {}`` without lockdep), a function whose machine code makes no memory access with
a displacement, an object that is no named local, and an access whose displacement the facts could
not compute.
"""

from __future__ import annotations

import collections
from typing import Any

KIND = "field_access"

Fields = set[tuple[int, int]]


def _inside(slot: int, size: int) -> bool:
    """A slot below the frame's top that holds the object's bytes."""
    return slot < 0 and (size <= 0 or slot + size <= 0)


def _exact(slot: int, fields: Fields, accesses: set[tuple[int, int]]) -> int:
    return sum((slot + displacement, width) in accesses for displacement, width in fields)


def evidence(name: str, function: str, analysis: dict[str, Any], image: dict[str, Any]) -> set[int]:
    """The slots the image's calls give an object: a frame address B + d in the argument register
    where the C passes ``&object + d``."""
    passed = [
        (callee, position, displacement)
        for callee, position, held, displacement in analysis.get("object_arguments", {}).get(function, [])
        if held == name
    ]
    given = image.get("frame_arguments", {}).get(function, [])
    return {
        offset - displacement
        for callee, position, displacement in passed
        for target, index, offset in given
        if target == callee and index == position
    }


def fit(fields: Fields, size: int, accesses: set[tuple[int, int]], slots: set[int]) -> int | None:
    """The frame slot of an object whose accessed fields are ``(displacement, bytes)``: of the slots
    the calls give (``evidence``), the one that holds the object's bytes and the most fields."""
    placed = [slot for slot in slots if _inside(slot, size)]
    return max(placed, key=lambda slot: (_exact(slot, fields, accesses), slot)) if placed else None


def _reason(item: dict[str, Any], displacements: set[int]) -> str | None:
    """Why the check cannot hold an access against a displacement; ``local`` for a frame candidate."""
    if item.get("size") == 0 and not item.get("address"):
        # An empty struct member (an array member of no bytes is an address, which still counts).
        return "zero_size_member"
    if not displacements - {0}:
        return "no_memory_access_in_image"
    if item["base"] in ("local", "other"):
        if item["base"] == "other" or "object" not in item:
            return "other_object"
        return "local" if item["displacement"] >= 0 else "displacement_unknown"
    displacement = item.get("displacement", -1)
    known = displacement >= 0 or (item["base"] == "container_of" and displacement < -1)
    if not known and item.get("offset", -1) < 0 and item["base"] != "container_of":
        return "displacement_unknown"
    return None


def exclusion(function: str, item: dict[str, Any], reason: str) -> dict[str, Any]:
    """The exclusions.json item of a field access, which is its detail; marks the access excluded."""
    item["excluded"] = reason
    detail = {key: value for key, value in item.items() if key not in ("excluded", "frame_slot", "frame_rule")}
    return {"kind": KIND, "reason": reason, "function": function, "target": None, "detail": detail, "provenance": []}


def frame_of(image: dict[str, Any], function: str) -> tuple[set[tuple[int, int]], set[int]]:
    accesses = {(offset, width) for offset, width in image.get("frame_accesses", {}).get(function, [])}
    return accesses, set(image.get("frame_addresses", {}).get(function, []))


def _objects(items: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    grouped: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
    for item in items:
        if item.get("excluded") in (None, "local_object_without_frame_slot") and "object" in item:
            grouped[item["object"]].append(item)
    return grouped


def _fields(members: list[dict[str, Any]]) -> Fields:
    return {(member["displacement"], member["size"]) for member in members if member["size"] > 0}


def annotate(field_accesses: dict[str, list[dict[str, Any]]], analysis: dict[str, Any]) -> list[dict[str, Any]]:
    """Mark each access the check holds against the frame with its ``frame_slot`` and each it cannot
    hold with the ``excluded`` reason; return the exclusion items, one per excluded access."""
    image = analysis["image"]
    exclusions: list[dict[str, Any]] = []
    for function, items in sorted(field_accesses.items()):
        displacements = set(image["memory_offsets"].get(function, []))
        objects: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
        for item in items:
            reason = _reason(item, displacements)
            if reason == "local":
                objects[item["object"]].append(item)
            elif reason is not None:
                exclusions.append(exclusion(function, item, reason))
        accesses, _ = frame_of(image, function)
        for name in sorted(objects):
            members = objects[name]
            slot = fit(
                _fields(members), members[0].get("object_size", 0), accesses, evidence(name, function, analysis, image)
            )
            for member in members:
                if slot is None:
                    exclusions.append(exclusion(function, member, "local_object_without_frame_slot"))
                else:
                    member["frame_slot"] = slot
    return exclusions


def method(item: dict[str, Any], accesses: set[tuple[int, int]], addresses: set[int]) -> str:
    """How the frame holds a field of an object at its slot."""
    at, size = item["frame_slot"] + item["displacement"], max(1, item.get("size", 1))
    if (at, size) in accesses:
        return "frame_slot_access"
    if any(offset <= at and at + size <= offset + width for offset, width in accesses):
        # Neighbouring constant stores merged into one (sd zero over two 4-byte fields).
        return "frame_slot_wider_access"
    if any(at <= offset < at + size for offset, _ in accesses):
        return "frame_slot_narrower_access"
    if item.get("address") and at in addresses:
        return "frame_slot_address"
    return "no_access_at_frame_slot"


def _formed(fields: Fields, size: int, accesses: set[tuple[int, int]], addresses: set[int]) -> bool:
    """The rule the frame alone would give: a formed frame address where each of two or more fields is."""
    return len(fields) >= 2 and any(
        _inside(slot, size) and _exact(slot, fields, accesses) == len(fields) for slot in addresses
    )


def decoys(field_accesses: dict[str, list[dict[str, Any]]], analysis: dict[str, Any]) -> dict[str, Any]:
    """How often a slot holds fields by chance. For each object a call places: the fields its slot
    holds, against the most another call's frame argument of the function would hold. For each object
    no call places: whether the next object of another struct, with its fields and size, fits the
    function's frame by the frame alone (a formed address where every field is), the rule this module
    does not use."""
    image = analysis["image"]
    objects = []
    for function, items in sorted(field_accesses.items()):
        grouped = _objects(items)
        for name in sorted(grouped):
            members = grouped[name]
            objects.append((function, name, members[0]["struct"], _fields(members), members[0].get("object_size", 0)))
    placed = compared = held = wrong_held = unplaced = tries = fits = 0
    for index, (function, name, struct, fields, size) in enumerate(objects):
        accesses, addresses = frame_of(image, function)
        slots = {slot for slot in evidence(name, function, analysis, image) if _inside(slot, size)}
        if slots:
            placed += 1
            slot = max(slots, key=lambda slot: (_exact(slot, fields, accesses), slot))
            others = {
                offset for _, _, offset in image.get("frame_arguments", {}).get(function, []) if offset not in slots
            }
            if others:
                compared += 1
                held += _exact(slot, fields, accesses)
                wrong_held += max(_exact(other, fields, accesses) for other in others)
            continue
        unplaced += 1
        other = next(
            (candidate for candidate in objects[index + 1 :] + objects[:index] if candidate[2] != struct),
            None,
        )
        if other is not None and len(other[3]) >= 2:
            tries += 1
            fits += _formed(other[3], other[4], accesses, addresses)
    return {
        "placed_by_a_call": placed,
        "without_a_call": unplaced,
        "call_argument": {
            "objects_compared": compared,
            "fields_held_at_the_slot": held,
            "fields_held_at_another_call_s_frame_argument": wrong_held,
        },
        "frame_alone": {"wrong_struct_tries": tries, "wrong_struct_fits": fits},
    }
