"""A unit's cached facts are reused only while every header search they rest on gives the same answer:
a header named by a macro, at an ``#include`` or an ``__has_include``, counts as a written one (C5).
The cases are the reviewers' own (LegionR6ReviewAstra findings 2 and 5, LegionReviewB finding F7)."""

from __future__ import annotations

import ast
from pathlib import Path
from typing import Any

from conftest import FIXTURE, build

from cgraph import cache, clangfacts, commands, config


def _extract(tree: Path, flags: tuple[str, ...] = ()) -> tuple[dict[str, Any], list[str]]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    arguments = commands.template_arguments((*loaded.flags, *flags), tree, "src/g.c")
    return clangfacts.extract("src/g.c", tree, arguments, loaded.vocabulary), arguments


def _stored(tree: Path, directory: Path, flags: tuple[str, ...] = ()) -> dict[str, Any]:
    """Extract the unit and store its facts in a cache, as a build does."""
    facts, arguments = _extract(tree, flags)
    store = cache.Cache(directory)
    store.store("src/g.c", arguments, "vocabulary", facts, cache.Contents(tree))
    store.save()
    return facts


def _reused(tree: Path, directory: Path, flags: tuple[str, ...] = ()) -> dict[str, Any] | None:
    """What a later build takes from the cache for the unit, or None where it parses again."""
    _, arguments = _extract(tree, flags)
    store = cache.Cache(directory)
    try:
        return store.lookup("src/g.c", arguments, "vocabulary", cache.Contents(tree))
    finally:
        store.close()


def _writes(facts: dict[str, Any]) -> set[str]:
    return {item["field"] for item in facts["functions"]["root"]["events"]["field_accesses"]}


def _calls(facts: dict[str, Any]) -> set[str]:
    return {call["callee"] for call in facts["functions"]["root"]["events"]["calls"]}


def test_a_header_a_macro_names_at_has_include_is_a_probe(tmp_path: Path) -> None:
    """LegionR6ReviewAstra finding 2: optional.h absent, then created; the cache must not reuse."""
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True)
    (tree / "src" / "g.c").write_text(
        '#define HEADER "optional.h"\n'
        "#if __has_include(HEADER)\n"
        "#include HEADER\n"
        "#else\n"
        "#define ENABLE 0\n"
        "#endif\n"
        "struct S { long pad; long field; };\n"
        "void root(struct S *p) { if (ENABLE) p->field = 7; }\n"
    )
    before = _stored(tree, tmp_path / "cache")
    assert _writes(before) == set()
    assert "src/optional.h" in before["include_probes"]

    (tree / "src" / "optional.h").write_text("#define ENABLE 1\n")
    assert _reused(tree, tmp_path / "cache") is None
    assert _writes(_extract(tree)[0]) == {"field"}


def test_a_header_a_macro_names_at_include_has_every_search_place_as_a_probe(tmp_path: Path) -> None:
    """LegionReviewB finding F7: -I hi -I lo, only lo/choice.h; a new hi/choice.h shadows it."""
    tree = tmp_path / "tree"
    for directory in ("src", "hi", "lo"):
        (tree / directory).mkdir(parents=True)
    (tree / "lo" / "choice.h").write_text("#define FLAG 0\n")
    (tree / "src" / "g.c").write_text(
        '#define HEADER "choice.h"\n'
        "#include HEADER\n"
        "void warn(void);\n"
        "void note(void);\n"
        "void root(void) { if (FLAG) warn(); else note(); }\n"
    )
    flags = (f"-I{tree}/hi", f"-I{tree}/lo")
    before = _stored(tree, tmp_path / "cache", flags)
    assert _calls(before) == {"note"}
    assert {"hi/choice.h", "lo/choice.h"} <= set(before["include_probes"])

    (tree / "hi" / "choice.h").write_text("#define FLAG 1\n")
    assert _reused(tree, tmp_path / "cache", flags) is None
    assert _calls(_extract(tree, flags)[0]) == {"warn"}


def test_a_header_a_function_like_macro_names_at_include_is_probed_by_what_it_included(tmp_path: Path) -> None:
    """#include TRACE_INCLUDE(...) builds the name with a function-like macro: the facts take the file
    it included (lo/choice.h) and probe its spelling in every search place, so a new hi/choice.h
    parses the unit again."""
    tree = tmp_path / "tree"
    for directory in ("src", "hi", "lo"):
        (tree / directory).mkdir(parents=True)
    (tree / "lo" / "choice.h").write_text("#define FLAG 0\n")
    (tree / "src" / "g.c").write_text(
        "#define HEADER(name) <name.h>\n"
        "#include HEADER(choice)\n"
        "void warn(void);\n"
        "void note(void);\n"
        "void root(void) { if (FLAG) warn(); else note(); }\n"
    )
    flags = (f"-I{tree}/hi", f"-I{tree}/lo")
    before = _stored(tree, tmp_path / "cache", flags)
    assert "reusable" not in before
    assert {"hi/choice.h", "lo/choice.h"} <= set(before["include_probes"])

    (tree / "hi" / "choice.h").write_text("#define FLAG 1\n")
    assert _reused(tree, tmp_path / "cache", flags) is None
    assert _calls(_extract(tree, flags)[0]) == {"warn"}


def test_a_comment_after_a_directive_is_no_part_of_its_spelling(tmp_path: Path) -> None:
    """``#include <linux/init.h> /* init_rootfs */`` and the same on an ``__has_include``: the spelling
    is the header, so the unit keeps its probes and stays reusable. (An ``#include`` whose spelling the
    facts cannot read falls back to what it included, so this holds either way; the direct reading
    probes every search place, which that fallback does only for the file found.)"""
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True)
    (tree / "src" / "g.h").write_text("#define ENABLE 1\n")
    (tree / "src" / "g.c").write_text(
        '#include "g.h" /* ENABLE */\n'
        '#if __has_include("other.h") /* optional */\n'
        '#include "other.h"\n'
        "#endif\n"
        "struct S { long pad; long field; };\nvoid root(struct S *p) { if (ENABLE) p->field = 7; }\n"
    )
    facts = _stored(tree, tmp_path / "cache")
    assert "reusable" not in facts
    assert {"src/g.h", "src/other.h"} <= set(facts["include_probes"])
    assert _reused(tree, tmp_path / "cache") == facts


def test_a_later_layer_keys_an_unreusable_unit_by_its_facts(tmp_path: Path) -> None:
    """A unit the cache keeps no entry for has no manifest key, so the link layer keys it by the facts
    themselves: two different facts give two keys, and the same facts give one."""
    store = cache.Cache(tmp_path / "cache")
    unreusable = {"functions": {}, "includes": [], "include_probes": [], "reusable": False}
    first = store.facts_key("src/g.c", unreusable)
    assert first == store.facts_key("src/g.c", dict(unreusable))
    assert first != store.facts_key("src/g.c", {**unreusable, "functions": {"root": {}}})


def _graph_bytes(out: Path) -> dict[str, bytes]:
    """A build's outputs, but build.json, which records cache use."""
    return {path.name: path.read_bytes() for path in sorted(out.iterdir()) if path.name != "build.json"}


def test_an_unreusable_unit_s_changes_reach_the_graph_through_a_warm_cache(copy: Path, tmp_path: Path) -> None:
    """The state where the cache and a unit disagree, built directly: src/bags.c was reusable when the
    cache was filled, then gets an ``__has_include`` the facts cannot expand, then changes again with
    every definition on its line (so the link core's other inputs stay the same). Its manifest entry
    must go (``store``), and the link layer must key it by its facts (``facts_key``): a stale entry, or
    one key for every unreusable facts, would reuse the second build's link core."""
    directory = tmp_path / "cache"
    build(copy, "both", tmp_path / "first", directory)
    bags = copy / "src" / "bags.c"
    text = "#define FD_HEADER(name) <name.h>\n#if __has_include(FD_HEADER(absent))\n#endif\n" + bags.read_text()
    bags.write_text(text)
    second = build(copy, "both", tmp_path / "second", directory)
    assert "src/bags.c" in second.parsed
    changed = text.replace("list_insert(&g->all, &all_bags);", "list_insert(&g->all, &g->all);")
    assert changed != text
    bags.write_text(changed)
    warm = build(copy, "both", tmp_path / "warm", directory)
    build(copy, "both", tmp_path / "cold")
    assert warm.parsed == ["src/bags.c"]
    assert _graph_bytes(tmp_path / "warm") == _graph_bytes(tmp_path / "cold")
    assert _graph_bytes(tmp_path / "warm") != _graph_bytes(tmp_path / "second")


def test_a_has_include_operand_the_facts_cannot_expand_is_never_reused(tmp_path: Path) -> None:
    """A function-like macro builds the header name: the places it may name are not known, so the
    unit's facts are not kept for a later build."""
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True)
    (tree / "src" / "g.c").write_text(
        "#define HEADER(name) <name.h>\n"
        "#if __has_include(HEADER(optional))\n"
        "#define ENABLE 1\n"
        "#else\n"
        "#define ENABLE 0\n"
        "#endif\n"
        "struct S { long pad; long field; };\n"
        "void root(struct S *p) { if (ENABLE) p->field = 7; }\n"
    )
    facts = _stored(tree, tmp_path / "cache")
    assert facts["reusable"] is False
    assert _reused(tree, tmp_path / "cache") is None


def test_a_literal_header_keeps_the_unit_reusable(tmp_path: Path) -> None:
    """The control: with nothing changed, a unit whose headers are all known is reused."""
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True)
    (tree / "src" / "g.h").write_text("#define ENABLE 1\n")
    (tree / "src" / "g.c").write_text(
        '#include "g.h"\nstruct S { long pad; long field; };\nvoid root(struct S *p) { if (ENABLE) p->field = 7; }\n'
    )
    facts = _stored(tree, tmp_path / "cache")
    assert "reusable" not in facts
    assert _reused(tree, tmp_path / "cache") == facts


def _package_imports(name: str) -> set[str]:
    """The modules of the package that module ``name`` imports, directly."""
    tree = ast.parse((Path(clangfacts.__file__ or "").parent / f"{name}.py").read_text())
    found: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.level == 1:
            found |= {node.module} if node.module is not None else {alias.name for alias in node.names}
    return found


def test_the_tool_digest_covers_every_module_the_facts_come_from() -> None:
    """LegionR6ReviewAstra finding 5: a change to expressions.py, which clangfacts imports, changes
    extracted facts, so the cache key must cover it. Every module of the package that clangfacts.py
    imports, directly or through another, is hashed."""
    reached, pending = {"clangfacts"}, ["clangfacts"]
    while pending:
        for module in _package_imports(pending.pop()) - reached:
            reached.add(module)
            pending.append(module)
    assert "expressions" in reached
    hashed = {Path(module.__file__ or "").stem for module in cache.TOOL_MODULES}
    assert reached <= hashed
