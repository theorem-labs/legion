"""The C layer of one translation unit."""

from __future__ import annotations

import json
from typing import Any

import pytest
from conftest import FIXTURE

from cgraph import clangfacts, commands, config
from cgraph.errors import ExtractionError


@pytest.fixture(scope="module")
def units() -> dict[str, dict[str, Any]]:
    loaded = config.load(FIXTURE / "cgraph.toml")
    return {
        source: clangfacts.extract(
            source,
            loaded.inputs.c_tree,
            commands.template_arguments(loaded.flags, loaded.inputs.c_tree, source),
            loaded.vocabulary,
        )
        for source in ("src/store.c", "src/bags.c")
    }


def accesses(units: dict[str, dict[str, Any]], source: str, function: str) -> set[tuple[str, str, str | None]]:
    events = units[source]["functions"][function]["events"]
    return {
        (item["unit"]["name"] if item.get("unit") else f"param{item['param']}", item["access"], item.get("field"))
        for item in events["accesses"]
    }


def unevaluated(units: dict[str, dict[str, Any]], source: str, function: str) -> set[tuple[str, str]]:
    return {
        (item["unit"]["name"], item["reason"])
        for item in units[source]["functions"][function]["events"]["unevaluated"]
        if item.get("unit")
    }


def test_parses_without_errors(units: dict[str, dict[str, Any]]) -> None:
    assert all(not unit["errors"] for unit in units.values())


def test_a_unit_holds_its_functions_and_the_header_helpers_they_reach(units: dict[str, dict[str, Any]]) -> None:
    store = units["src/store.c"]["functions"]
    assert store["box_new"]["owned"] and not store["list_insert"]["owned"]
    assert {"hash_insert", "bucket_of", "write_seqlk", "class_tree_constructor", "class_tree_destructor"} <= set(store)
    assert "list_insert" in units["src/bags.c"]["functions"]


def test_sizeof_operand_is_not_an_access(units: dict[str, dict[str, Any]]) -> None:
    found = accesses(units, "src/store.c", "box_new")
    assert not any(name == "last" for name, _, _ in found)
    assert ("box_count", "sizeof") in unevaluated(units, "src/store.c", "box_new")
    assert ("last", "sizeof") in unevaluated(units, "src/store.c", "box_new")
    assert ("box_count", "write", None) in found


def test_branch_the_compiler_removes_is_not_an_access(units: dict[str, dict[str, Any]]) -> None:
    assert ("last", "dead_branch") in unevaluated(units, "src/store.c", "box_new")


def test_typeof_expression_is_not_an_access(units: dict[str, dict[str, Any]]) -> None:
    assert not any(name == "tree_sem" for name, _, _ in accesses(units, "src/bags.c", "bag_count"))
    assert ("tree_sem", "type") in unevaluated(units, "src/bags.c", "bag_count")


def test_generic_association_types_are_read_from_the_macro(units: dict[str, dict[str, Any]]) -> None:
    events = units["src/bags.c"]["functions"]["bag_count"]["events"]
    assert ("all_bags", "read", None) in accesses(units, "src/bags.c", "bag_count")
    assert ("box_table", "generic_unselected") in unevaluated(units, "src/bags.c", "bag_count")
    assert not any(item.get("generic") for item in events["accesses"])
    assert units["src/bags.c"]["generic_selections"] == {"selected": 1, "ambiguous": 0}


def test_cleanup_attribute_names_the_destructor(units: dict[str, dict[str, Any]]) -> None:
    calls = units["src/store.c"]["functions"]["box_scoped"]["events"]["calls"]
    implied = {call["callee"]: call for call in calls if call.get("implied") == "cleanup"}
    assert set(implied) == {"tree_unlock", "__free_obj"}
    assert implied["tree_unlock"]["arguments"][0]["unit"]["name"] == "tree_sem"
    assert implied["__free_obj"]["arguments"][0]["unit"]["name"] == "box"


def test_weak_definition_is_marked(units: dict[str, dict[str, Any]]) -> None:
    assert units["src/store.c"]["functions"]["arch_hook"]["weak"]
    assert not units["src/bags.c"]["functions"]["arch_hook"]["weak"]


def test_a_field_through_a_member_chain_has_its_displacement(units: dict[str, dict[str, Any]]) -> None:
    fields = units["src/bags.c"]["functions"]["crate_size"]["events"]["field_accesses"]
    size = next(item for item in fields if item["field"] == "size")
    assert (size["offset"], size["displacement"]) == (56, 64)


def test_unit_facts_name_no_absolute_path(units: dict[str, dict[str, Any]]) -> None:
    assert "/home/" not in json.dumps(units) and FIXTURE.as_posix() not in json.dumps(units)


def test_include_probes_cover_main_and_included_file_search_paths(units: dict[str, dict[str, Any]]) -> None:
    probes = set(units["src/bags.c"]["include_probes"])
    assert {
        "src/mini.h",
        "src/bags.h",
        "include/override/mini.h",
        "include/override/bags.h",
        "include/mini.h",
        "include/bags.h",
        "include/optional-bags.h",
        "include/override/optional-bags.h",
        "include/next-bags.h",
        "include/override/next-bags.h",
    } <= probes


def test_function_local_static_uses_the_compiler_name(units: dict[str, dict[str, Any]]) -> None:
    assert ("next_id.counter", "write", None) in accesses(units, "src/store.c", "next_id")


def test_kit_type_fields_belong_to_the_object_that_holds_them(units: dict[str, dict[str, Any]]) -> None:
    found = accesses(units, "src/bags.c", "bag_add")
    assert ("item", "read", "link") in found and ("bag", "read", "all") in found
    assert not any(name == "list_node" for name, _, _ in found)


def test_lock_through_a_parameter_waits_for_the_caller(units: dict[str, dict[str, Any]]) -> None:
    assert accesses(units, "src/store.c", "write_seqlk") >= {("param0", "lock", None)}
    assert accesses(units, "src/store.c", "class_tree_constructor") == {("param0", "lock", None)}


def test_cleanup_local_calls_its_destructor_with_the_constructor_arguments(units: dict[str, dict[str, Any]]) -> None:
    calls = units["src/store.c"]["functions"]["box_tree_add"]["events"]["calls"]
    destructor = next(call for call in calls if call["callee"] == "class_tree_destructor")
    assert destructor["implied"] == "cleanup"
    assert destructor["arguments"][0]["unit"]["name"] == "tree_sem"


def test_allocation_takes_the_type_of_its_result_and_free_its_argument(units: dict[str, dict[str, Any]]) -> None:
    assert ("box", "allocate", None) in accesses(units, "src/store.c", "box_new")
    assert accesses(units, "src/store.c", "box_free") == {("box", "free", None)}


def test_direct_nested_calls_are_not_function_pointer_sites(units: dict[str, dict[str, Any]]) -> None:
    pointers = units["src/bags.c"]["functions"]["bag_count"]["events"]["function_pointers"]
    assert {item["target"] for item in pointers} == {"visit"}


def test_indirect_call_through_an_ops_field(units: dict[str, dict[str, Any]]) -> None:
    slots = units["src/store.c"]["functions"]["run_get"]["events"]["indirect"]
    assert [(slot["ops_struct"], slot["field"]) for slot in slots] == [("ops", "get")]


def test_callback_table_initializer(units: dict[str, dict[str, Any]]) -> None:
    tables = units["src/store.c"]["tables"]
    assert [
        (table["name"], table["ops_struct"], [(entry["field"], entry["target"]) for entry in table["entries"]])
        for table in tables
    ] == [("box_ops", "ops", [("get", "box_get"), ("put", "box_put")])]


def test_record_layouts_and_plain_values(units: dict[str, dict[str, Any]]) -> None:
    records = {record["name"]: record for record in units["src/store.c"]["records"].values()}
    box = records["box"]
    fields = {field["name"]: field for field in box["fields"]}
    assert box["size"] == 64 and fields["size"]["offset"] == 56
    assert fields["b"]["record"] == "base" and not fields["b"]["pointer"]
    assert fields["first"]["record"] == "item" and fields["first"]["pointer"]
    assert records["uid_v"]["plain"] and not box["plain"]


def test_membership_witness_lines(units: dict[str, dict[str, Any]]) -> None:
    assert [item["provenance"][0]["line"] for item in units["src/bags.c"]["witnesses"]] == [55]


def test_const_scalar_global_is_a_constant_not_state(units: dict[str, dict[str, Any]]) -> None:
    assert not any(name == "tree_step" for name, _, _ in accesses(units, "src/store.c", "box_tree_add"))
    assert ("tree_step", "constant") in unevaluated(units, "src/store.c", "box_tree_add")


def test_negative_record_layout_size_keeps_its_layout_error() -> None:
    class Layout:
        def get_size(self) -> int:
            return -2

    class Record:
        spelling = "incomplete_record"
        type = Layout()

    extractor = object.__new__(clangfacts._Extractor)
    extractor.record_fields = lambda _: []
    extractor.where = lambda _: "src/test.c:1"
    extractor.is_plain = lambda _: False
    facts = extractor.record_facts(Record())

    assert facts["size"] == -2
    assert facts["layout_error"] == "incomplete"


def test_negative_access_field_size_keeps_its_layout_error() -> None:
    class FieldType:
        def get_size(self) -> int:
            return -2

    class Field:
        type = FieldType()

        def is_bitfield(self) -> bool:
            return False

    class Where:
        kind = clangfacts.K.MEMBER_REF_EXPR
        referenced = Field()
        hash = 7

    class Extractor:
        def location(self, _: object) -> dict[str, object]:
            return {"source": "c", "file": "src/test.c", "line": 1}

    walk = object.__new__(clangfacts._FunctionWalk)
    walk.x = Extractor()
    walk.events = {"accesses": [], "field_accesses": [], "unevaluated": []}
    walk.conditional = 0
    walk.fields, walk.inner, walk.addressed = [], set(), set()
    # The member chain's address, as the walk records it at the chain's top.
    walk.field_address = {Where.hash: (clangfacts._Address(32, "pointer"), False)}
    walk.add_access(
        {"kind": "struct", "name": "incomplete_record", "def": "src/test.c:1", "field": "member", "offset": 4},
        "read",
        Where(),
        None,
    )
    walk.finish()

    (access,) = walk.events["field_accesses"]
    assert (access["displacement"], access["size"], access["layout_error"]) == (4, -2, "incomplete")


def test_unknown_result_register_size_is_an_exclusion(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(clangfacts, "_result_registers", lambda _: (2, "incomplete"))
    loaded = config.load(FIXTURE / "cgraph.toml")
    facts = clangfacts.extract(
        "src/store.c",
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, "src/store.c"),
        loaded.vocabulary,
    )

    exclusion = next(item for item in facts["exclusions"] if item["function"] == "box_new")
    assert exclusion == {
        "kind": "function",
        "reason": "result_registers_unknown",
        "function": "box_new",
        "target": None,
        "detail": {"layout_error": "incomplete"},
        "provenance": [{"source": "c", "file": "src/store.c", "line": 39}],
    }


def test_callback_table_with_unavailable_tokens_is_an_exclusion(monkeypatch: pytest.MonkeyPatch) -> None:
    class File:
        name = str(FIXTURE / "src" / "store.c")

    class Location:
        file = File()
        line = 7

    class Function:
        kind = clangfacts.K.FUNCTION_DECL
        spelling = "box_get"

    class Reference:
        kind = clangfacts.K.DECL_REF_EXPR
        referenced = Function()

    class Initializer:
        def walk_preorder(self) -> list[Reference]:
            return [Reference()]

    class Table:
        kind = clangfacts.K.VAR_DECL
        spelling = "fake_ops"
        location = Location()
        extent = object()
        type = object()

    class Root:
        def get_children(self) -> list[Table]:
            return [Table()]

    class TranslationUnit:
        cursor = Root()
        diagnostics: tuple[object, ...] = ()

        def get_includes(self) -> list[object]:
            return []

        def get_tokens(self, *, extent: object) -> list[object]:
            raise RuntimeError("tokens unavailable")

    class Index:
        def parse(self, *_: object, **__: object) -> TranslationUnit:
            return TranslationUnit()

    class Record:
        spelling = "ops"

    monkeypatch.setattr(clangfacts.ci.Index, "create", staticmethod(Index))
    monkeypatch.setattr(clangfacts, "_canonical_record", lambda _: (Record(), False, False))
    monkeypatch.setattr(clangfacts.libclang, "var_initializer", lambda _: Initializer())
    # The fake table models only what the table reader reads.
    monkeypatch.setattr(clangfacts, "_global_initializers", lambda *_: [])
    loaded = config.load(FIXTURE / "cgraph.toml")
    facts = clangfacts.extract(
        "src/store.c",
        loaded.inputs.c_tree,
        commands.template_arguments(loaded.flags, loaded.inputs.c_tree, "src/store.c"),
        loaded.vocabulary,
    )

    assert facts["tables"] == []
    assert facts["exclusions"] == [
        {
            "kind": "callback_table",
            "reason": "tokens_unavailable",
            "function": None,
            "target": "fake_ops",
            "detail": {"table": "fake_ops", "ops_struct": "ops"},
            "provenance": [{"source": "c", "file": "src/store.c", "line": 7}],
        }
    ]


def test_unavailable_written_tokens_name_the_unit_and_location() -> None:
    class File:
        name = str(FIXTURE / "src" / "store.c")

    class Location:
        file = File()
        line = 7
        column = 1

    class Cursor:
        location = Location()
        extent = object()

    class TranslationUnit:
        def get_tokens(self, *, extent: object) -> list[object]:
            raise RuntimeError("tokens unavailable")

    extractor = object.__new__(clangfacts._Extractor)
    extractor.tu = TranslationUnit()
    extractor.source = "src/store.c"
    extractor.where = lambda _: "src/store.c:7"

    with pytest.raises(ExtractionError, match=r"src/store\.c:7"):
        extractor.written_tokens(Cursor(), Cursor())
