"""The link's image-guided rules on synthetic units and a stub image: each rule with a case it must
apply to and one it must not. Units, functions and the image are made up; nothing is parsed."""

from __future__ import annotations

from typing import Any

from conftest import FIXTURE

from cgraph import checks, config, link
from cgraph.contracts import ContractGraph
from cgraph.image import FunctionFacts, Symbol

CONFIG = config.load(FIXTURE / "cgraph.toml")
Facts = dict[str, Any]


def where(file: str = "a.c", line: int = 1) -> list[dict[str, Any]]:
    return [{"source": "c", "file": file, "line": line}]


def global_unit(name: str) -> dict[str, Any]:
    return {"kind": "global", "name": name, "def": None, "field": None, "lock": False}


def call(callee: str, *arguments: dict[str, Any], visible: bool = True, line: int = 1) -> dict[str, Any]:
    return {"callee": callee, "arguments": list(arguments), "provenance": where(line=line), "visible": visible}


def address_of(name: str) -> dict[str, Any]:
    """An argument that is the address of a global."""
    return {"unit": global_unit(name), "param": None}


def parameter(index: int) -> dict[str, Any]:
    return {"unit": None, "param": index}


def read(name: str, line: int = 1) -> dict[str, Any]:
    return {"unit": global_unit(name), "access": "read", "field": None, "provenance": where(line=line)}


def read_parameter(index: int, line: int = 1) -> dict[str, Any]:
    return {"unit": None, "param": index, "access": "read", "field": None, "provenance": where(line=line)}


def function(
    name: str,
    file: str = "a.c",
    *,
    static: bool = False,
    owned: bool = True,
    calls: tuple[dict[str, Any], ...] = (),
    accesses: tuple[dict[str, Any], ...] = (),
    pointers: tuple[str, ...] = (),
    indirect: tuple[tuple[str, str], ...] = (),
    line: int | None = None,
) -> Facts:
    return {
        "name": name,
        "file": file,
        "line": line if line is not None else 10 + len(name),
        "static": static,
        "weak": False,
        "owned": owned,
        "returns": None,
        "parameters": 1,
        "result_registers": 1,
        "events": {
            "calls": list(calls),
            "accesses": list(accesses),
            "unevaluated": [],
            "list_calls": [],
            "indirect": [{"ops_struct": struct, "field": field, "provenance": where()} for struct, field in indirect],
            "function_pointers": [{"target": target, "provenance": where()} for target in pointers],
            "field_accesses": [],
        },
    }


def unit(*functions: Facts, tables: tuple[dict[str, Any], ...] = ()) -> Facts:
    return {
        "functions": {item["name"]: item for item in functions},
        "records": {},
        "tables": list(tables),
        "witnesses": [],
        "generic_selections": {"selected": 0, "ambiguous": 0},
        "errors": [],
        "includes": [],
    }


def table(name: str, struct: str, file: str, **entries: str) -> dict[str, Any]:
    return {
        "name": name,
        "ops_struct": struct,
        "entries": [{"field": field, "target": target} for field, target in sorted(entries.items())],
        "provenance": where(file),
    }


class StubImage:
    """Function copies (name -> [(address, local, ELF FILE name)]) and the facts of each copy."""

    def __init__(self, copies: dict[str, list[tuple[int, bool, str]]], facts: dict[int, FunctionFacts]) -> None:
        self.functions = {
            name: [Symbol(address, 4, name, ".text", local, file) for address, local, file in items]
            for name, items in copies.items()
        }
        self.object_symbols: dict[str, list[Symbol]] = {}
        self.text_at = {symbol.address: name for name, items in self.functions.items() for symbol in items}
        self.map_source_names = frozenset()
        self.sized_object_names: list[str] = []
        self._facts = facts

    def facts(self, symbol: Symbol) -> FunctionFacts:
        return self._facts.get(symbol.address, FunctionFacts())


def image_calls(*targets: str) -> list[dict[str, Any]]:
    return [
        {"target": target, "instruction": "0x0", "tail": False, "provenance": [{"source": "image", "address": "0x0"}]}
        for target in targets
    ]


def image_globals(*names: str, offset: int = 0) -> list[dict[str, Any]]:
    return [
        {
            "global": name,
            "symbol": name,
            "symbol_address": "0x9000",
            "offset": offset,
            "section": ".data",
            "target_class": "state",
            "operation": "addi_address",
            "provenance": [{"source": "image", "address": "0x0"}],
        }
        for name in names
    ]


def build(tus: dict[str, Facts], image: StubImage, contracts: ContractGraph | None = None) -> dict[str, Any]:
    graph, _ = link.build(CONFIG, "p", sorted(tus), tus, image, contracts or ContractGraph(), {})  # type: ignore[arg-type]
    return graph


def edges(graph: dict[str, Any], kind: str, source: str) -> list[dict[str, Any]]:
    return [edge for edge in graph["edges"] if edge["kind"] == kind and edge["source"] == source]


def c_calls(graph: dict[str, Any], source: str) -> set[str]:
    return {item["target"] for item in graph["analysis"]["c_calls"] if item["source"] == source}


def call_pairs(graph: dict[str, Any]) -> dict[str, list[dict[str, str]]]:
    pairs: dict[str, list[dict[str, str]]] = checks.calls(graph)["pairs"]
    return pairs


# A function whose address the C passes -------------------------------------------------------


def _pointer_program(shown_calls: tuple[str, ...], shown_globals: tuple[str, ...]) -> dict[str, Any]:
    """run passes the address of cb; run itself reads shared and calls own_call. cb reads shared and
    calls cb_call. The image of run makes the given calls and addresses the given globals."""
    tus = {
        "a.c": unit(
            function(
                "run",
                calls=(call("own_call", visible=False), call("register", visible=False)),
                accesses=(read("shared"),),
                pointers=("cb",),
            ),
            function("cb", static=True, calls=(call("cb_call", visible=False),), accesses=(read("shared"),)),
        ),
        "b.c": unit(
            function("own_call", "b.c"),
            function("cb_call", "b.c"),
            function("register", "b.c"),
        ),
    }
    # cb has no copy of its own: wherever the compiler kept it, it inlined it.
    image = StubImage(
        {"run": [(0x100, False, "")], "own_call": [(0x200, False, "")], "cb_call": [(0x300, False, "")],
         "register": [(0x400, False, "")]},
        {0x100: FunctionFacts(calls=image_calls("own_call", "register", *shown_calls),
                              globals=image_globals("shared", *shown_globals))},
    )  # fmt: skip
    return build(tus, image)


def test_a_pointer_target_is_not_folded_on_evidence_the_caller_explains_itself() -> None:
    """The image addresses shared and calls own_call because run does: that shows nothing of cb."""
    graph = _pointer_program((), ())
    assert "function:cb_call" not in c_calls(graph, "function:run")
    assert not any(fold["helper"] == "function:cb@a.c" for fold in graph["analysis"]["folds"])
    assert [edge["target"] for edge in edges(graph, "call_indirect", "function:run")] == ["function:cb@a.c"]
    assert call_pairs(graph)["only_c"] == []


def test_a_pointer_target_is_folded_when_the_image_shows_its_own_call() -> None:
    graph = _pointer_program(("cb_call",), ())
    assert "function:cb_call" in c_calls(graph, "function:run")
    assert {fold["helper"] for fold in graph["analysis"]["folds"]} == {"function:cb@a.c"}
    assert call_pairs(graph) == {"only_c": [], "only_image": []}


def test_a_pointer_target_whose_address_is_formed_lends_the_calls_of_its_inlined_copy() -> None:
    """devm_add_action_or_reset: the address goes to register, and the error path calls the
    action, which the compiler inlined: the image calls the action's callee."""
    tus = {
        "a.c": unit(
            function("run", calls=(call("register", visible=False),), pointers=("release",)),
            function("release", static=True, calls=(call("free", visible=False),)),
        ),
        "b.c": unit(function("register", "b.c"), function("free", "b.c")),
    }
    copies = {"run": [(0x100, False, "")], "register": [(0x200, False, "")], "free": [(0x300, False, "")],
              "release": [(0x400, True, "a.c")]}  # fmt: skip
    formed = [{"target": "release", "provenance": [{"source": "image", "address": "0x104"}]}]
    released = {0x400: FunctionFacts(calls=image_calls("free"))}
    graph = build(tus, StubImage(copies, {0x100: FunctionFacts(calls=image_calls("register", "free"),
                                                                function_addresses=formed), **released}))  # fmt: skip
    assert c_calls(graph, "function:run") == {"function:register", "function:free"}
    (edge,) = [edge for edge in edges(graph, "call", "function:run") if edge["target"] == "function:free"]
    assert edge["provenance"][0]["via"] == ["function:release@a.c"]
    assert call_pairs(graph) == {"only_c": [], "only_image": []}
    # Without the image's call, nothing of the target is lent.
    graph = build(tus, StubImage(copies, {0x100: FunctionFacts(calls=image_calls("register"),
                                                                function_addresses=formed), **released}))  # fmt: skip
    assert c_calls(graph, "function:run") == {"function:register"}


# A header function's copy and the unit that compiled it --------------------------------------


def test_a_header_function_takes_the_facts_of_the_unit_that_compiled_its_image_copy() -> None:
    """x.c and y.c both define the header's static inline lock_rq; only y.c kept a copy, and in
    y.c the out-of-line nested is visible and inlined into it."""
    lock_rq_in_x = function(
        "lock_rq", "inc.h", static=True, owned=False, calls=(call("nested", visible=False),), line=5
    )
    lock_rq_in_y = function("lock_rq", "inc.h", static=True, owned=False, calls=(call("nested"),), line=5)
    tus = {
        "x.c": unit(function("use_x", "x.c", calls=(call("lock_rq"),)), lock_rq_in_x),
        "y.c": unit(lock_rq_in_y, function("nested", "y.c", calls=(call("raw_lock", visible=False),))),
        "z.c": unit(function("raw_lock", "z.c")),
    }
    copies = {"lock_rq": [(0x100, True, "y.c")], "use_x": [(0x200, False, "")], "nested": [(0x300, False, "")],
              "raw_lock": [(0x400, False, "")]}  # fmt: skip
    facts = {0x100: FunctionFacts(calls=image_calls("raw_lock")), 0x200: FunctionFacts(calls=image_calls("lock_rq")),
             0x300: FunctionFacts(calls=image_calls("raw_lock"))}  # fmt: skip
    graph = build(tus, StubImage(copies, facts))
    assert c_calls(graph, "function:lock_rq@inc.h") == {"function:raw_lock"}
    assert call_pairs(graph) == {"only_c": [], "only_image": []}


# Callback slots and the tables of the caller's unit --------------------------------------------


def _slot_program(contract_target: str | None) -> tuple[dict[str, Any], dict[str, Any]]:
    """u1.c and u2.c each define a static table ops of struct bus; translate in u1.c calls
    through bus->map. The contract graph names contract_target for the slot."""
    tus = {
        "u1.c": unit(
            function("translate", "u1.c", static=True, indirect=(("bus", "map"),)),
            function("map_u1", "u1.c", static=True),
            tables=(table("ops", "bus", "u1.c", map="map_u1"),),
        ),
        "u2.c": unit(
            function("map_u2", "u2.c", static=True),
            tables=(table("ops", "bus", "u2.c", map="map_u2"),),
        ),
    }
    copies = {
        "translate": [(0x100, True, "u1.c")],
        "map_u1": [(0x200, True, "u1.c")],
        "map_u2": [(0x300, True, "u2.c")],
    }
    image = StubImage(copies, {0x100: FunctionFacts(calls=image_calls("map_u1"))})
    nodes = {
        "Prog/u1/Translate": {"id": "Prog/u1/Translate", "kind": "fn", "syms": ["translate"], "addr": "0x100"},
        "Prog/u1/MapU1": {"id": "Prog/u1/MapU1", "kind": "fn", "syms": ["map_u1"], "addr": "0x200"},
        "Prog/u2/MapU2": {"id": "Prog/u2/MapU2", "kind": "fn", "syms": ["map_u2"], "addr": "0x300"},
    }
    edges = (
        [{"kind": "table", "src": "Prog/u1/Translate", "dst": contract_target, "evidence": ["table:ops"]}]
        if contract_target
        else []
    )
    graph, exclusions = link.build(CONFIG, "p", sorted(tus), tus, image, ContractGraph(nodes=nodes, edges=edges), {})  # type: ignore[arg-type]
    return graph, exclusions


def test_a_contract_slot_edge_needs_the_initializer_of_the_table_the_callers_unit_defines() -> None:
    """u2.c's ops names map_u2, but translate's unit has its own ops: that one is the table."""
    graph, exclusions = _slot_program("Prog/u2/MapU2")
    targets = {edge["target"]: edge["resolution"] for edge in edges(graph, "call_indirect", "function:translate@u1.c")}
    assert "function:map_u2@u2.c" not in targets
    assert [(item["reason"], item["target"]) for item in exclusions["items"] if item["kind"] == "callback"] == [
        ("no_c_initializer", "function:map_u2@u2.c")
    ]


def test_a_slot_the_image_calls_directly_resolves_through_the_units_own_table() -> None:
    graph, _ = _slot_program(None)
    (edge,) = edges(graph, "call_indirect", "function:translate@u1.c")
    assert (edge["target"], edge["resolution"], edge["slot"]) == ("function:map_u1@u1.c", "table_slot", "ops.map")
    assert call_pairs(graph)["only_image"] == [
        {"source": "function:translate@u1.c", "target": "function:map_u1@u1.c", "class": "devirtualized_callback"}
    ]


# A static function's parameter every caller gives the same global --------------------------


def _specialized_program(arguments: tuple[dict[str, Any], dict[str, Any]], *, taken: bool = False,
                         image_names: tuple[str, ...] = ("tk_data",),
                         accesses: tuple[dict[str, Any], ...] = (read_parameter(0),),
                         initializers: tuple[dict[str, Any], ...] = ()) -> dict[str, Any]:  # fmt: skip
    """advance reads through its parameter; a and b call it (b through a parameter of its own,
    which c fills)."""
    first, second = arguments
    tus = {
        "a.c": {
            **unit(
                function("advance", static=True, accesses=accesses),
                function("a", calls=(call("advance", first),), pointers=("advance",) if taken else ()),
                function("b", static=True, calls=(call("advance", parameter(0)),)),
                function("c", calls=(call("b", second),)),
            ),
            "global_initializers": list(initializers),
        }
    }
    copies = {"advance": [(0x100, True, "a.c")], "a": [(0x200, False, "")], "b": [(0x300, True, "a.c")],
              "c": [(0x400, False, "")]}  # fmt: skip
    facts = {0x100: FunctionFacts(globals=image_globals(*image_names)), 0x200: FunctionFacts(calls=image_calls("advance")),
             0x300: FunctionFacts(calls=image_calls("advance")), 0x400: FunctionFacts(calls=image_calls("b"))}  # fmt: skip
    return build(tus, StubImage(copies, facts))


def _reads(graph: dict[str, Any], source: str) -> set[str]:
    return {edge["target"] for edge in edges(graph, "access", source) if not edge.get("image_only")}


def test_a_parameter_every_caller_gives_one_global_is_that_global_where_the_image_addresses_it() -> None:
    graph = _specialized_program((address_of("tk_data"), address_of("tk_data")))
    assert _reads(graph, "function:advance@a.c") == {"global:tk_data"}
    assert checks.accesses(graph)["only_image"] == 0


def test_a_parameter_is_not_specialized_without_one_global_from_every_caller() -> None:
    for arguments, taken, names in (
        ((address_of("tk_data"), address_of("other")), False, ("tk_data",)),
        ((address_of("tk_data"), address_of("tk_data")), True, ("tk_data",)),
        ((address_of("tk_data"), address_of("tk_data")), False, ()),
    ):
        graph = _specialized_program(arguments, taken=taken, image_names=names)
        assert _reads(graph, "function:advance@a.c") == set(), (arguments, taken, names)


def test_a_specialized_parameter_with_no_recorded_access_is_a_read_of_its_global() -> None:
    """xwrite(…, &wfile_pos) writes *pos, tk_setup_internals reads tk->f: the C facts record no
    access to the global (a scalar dereference, a field through a struct pointer)."""
    graph = _specialized_program((address_of("tk_data"), address_of("tk_data")), accesses=())
    (edge,) = edges(graph, "access", "function:advance@a.c")
    assert (edge["target"], edge["access"]) == ("global:tk_data", "read")
    assert graph["analysis"]["specialized_parameters"] == [
        {"function": "function:advance@a.c", "parameter": 0, "global": "global:tk_data"}
    ]
    assert checks.accesses(graph)["only_image"] == 0


def _initializer(holder: str, target: str, field: str | None) -> dict[str, Any]:
    return {"global": global_unit(holder), "field": field, "offset": 0, "target": global_unit(target),
            "target_offset": 0, "provenance": where("a.c", 76)}  # fmt: skip


def test_a_global_whose_initializer_stores_the_address_the_image_forms_explains_that_access() -> None:
    """__vdso_init(&vdso_info) reads vdso_info->code_start = vdso_start: the image forms vdso_start's
    address, never vdso_info's."""
    graph = _specialized_program(
        (address_of("vdso_info"), address_of("vdso_info")), accesses=(), image_names=("vdso_start",),
        initializers=(_initializer("vdso_info", "vdso_start", "code_start"), _initializer("vdso_info", "vdso_end", "end")),
    )  # fmt: skip
    (edge,) = edges(graph, "access", "function:advance@a.c")
    assert (edge["target"], edge["initializer"]) == ("global:vdso_start", "global:vdso_info")
    assert checks.accesses(graph)["only_image"] == 0
    # Without the initializer, nothing explains the image's vdso_start.
    graph = _specialized_program(
        (address_of("vdso_info"), address_of("vdso_info")), accesses=(), image_names=("vdso_start",)
    )
    assert _reads(graph, "function:advance@a.c") == set()


def test_a_read_of_a_whole_global_takes_every_address_its_initializer_stores_that_the_image_forms() -> None:
    """clk_unregister walks all_lists = { &clk_root_list, &clk_orphan_list }, unrolled."""
    tus = {
        "a.c": {
            **unit(function("walk", accesses=(read("all_lists"),))),
            "global_initializers": [
                _initializer("all_lists", "root_list", None),
                _initializer("all_lists", "orphans", None),
            ],
        }
    }
    graph = build(
        tus, StubImage({"walk": [(0x100, False, "")]}, {0x100: FunctionFacts(globals=image_globals("root_list"))})
    )
    assert {(edge["target"], edge.get("initializer")) for edge in edges(graph, "access", "function:walk")} == {
        ("global:all_lists", None),
        ("global:root_list", "global:all_lists"),
    }


# Field offsets: a global's field and a member's address at zero ------------------------------


def _structure_graph(item: dict[str, Any], c_targets: list[str], image_globals_at: dict[str, int]) -> dict[str, Any]:
    function_id = "function:f"
    return {
        "nodes": [{"id": function_id, "kind": "function", "name": "f", "part": True, "image_addresses": ["0x1"]}],
        "edges": [],
        "analysis": {
            "field_accesses": {function_id: [item]},
            "c_targets": {function_id: [{"name": name, "target_class": "state", "image_symbol": True} for name in c_targets]},
            "image": {
                "memory_offsets": {function_id: [8, -160]},
                "addi_constants": {function_id: []},
                "globals": [{"function": function_id, "global": name, "offset": offset, "target_class": "state"}
                            for name, offset in image_globals_at.items()],
            },
            "excluded_embedding_bases": [],
        },
    }  # fmt: skip


def _field(offset: int, **extra: Any) -> dict[str, Any]:
    return {"struct": "tk", "field": "f", "offset": offset, "displacement": offset, "base": "pointer", "size": 8,
            "bitfield": False, **extra}  # fmt: skip


def test_a_member_whose_address_is_the_base_needs_no_instruction() -> None:
    offsets = checks.structure(_structure_graph(_field(0, address=True), [], {}))["field_offsets"]
    assert offsets["by_method"] == {"member_address_at_zero": 1}
    assert checks.structure(_structure_graph(_field(0), [], {}))["field_offsets"]["matched"] == 0


def test_a_container_of_member_is_matched_at_its_negative_displacement() -> None:
    """container_of(p, struct tk, link) with link at 160: tk's field at 0 is at -160 from p."""
    item = _field(0, base="container_of", displacement=-160)
    offsets = checks.structure(_structure_graph(item, [], {}))["field_offsets"]
    assert offsets["by_method"] == {"memory_displacement": 1}
    item = _field(0, base="container_of", displacement=-168)
    assert checks.structure(_structure_graph(item, [], {}))["field_offsets"]["matched"] == 0


def test_a_parameter_passed_on_is_followed_even_where_the_argument_also_names_its_struct() -> None:
    """__do_adjtimex(tkd) passes its own parameter on: the facts type the argument as its struct and
    give its parameter with displacement 0 (the pointer itself)."""
    typed = {"unit": {"kind": "struct", "name": "tk", "def": "a.c:1", "field": None}, "param": 0, "displacement": 0}
    tus = {
        "a.c": unit(
            function("advance", static=True, accesses=(read_parameter(0),)),
            function("a", calls=(call("advance", address_of("tk_data")),)),
            function("b", static=True, calls=(call("advance", typed),)),
            function("c", calls=(call("b", address_of("tk_data")),)),
        )
    }
    copies = {"advance": [(0x100, True, "a.c")], "a": [(0x200, False, "")], "b": [(0x300, True, "a.c")],
              "c": [(0x400, False, "")]}  # fmt: skip
    facts = {0x100: FunctionFacts(globals=image_globals("tk_data")), 0x200: FunctionFacts(calls=image_calls("advance")),
             0x300: FunctionFacts(calls=image_calls("advance")), 0x400: FunctionFacts(calls=image_calls("b"))}  # fmt: skip
    graph = build(tus, StubImage(copies, facts))
    assert _reads(graph, "function:advance@a.c") == {"global:tk_data"}


SIZES = {"outer": 64, "inner": 16, "elem": 8}


def _record(name: str, *fields: tuple[str, int, int, str | None]) -> dict[str, Any]:
    return {
        "name": name,
        "def": f"a.c:{SIZES[name]}",
        "size": SIZES[name],
        "fields": [
            {"name": field, "offset": offset, "size": field_size, "line": 1, "pointer": False,
             **({"record": record, "record_def": f"a.c:{SIZES[record]}"} if record else {})}
            for field, offset, field_size, record in fields
        ],
    }  # fmt: skip


def test_the_offsets_of_each_struct_inside_a_global_the_function_names_come_from_the_layouts() -> None:
    """G is an array of two outer; outer embeds inner at 8 and three elem from 32."""
    records = {
        "outer@a.c:64": _record("outer", ("head", 0, 8, None), ("inner", 8, 16, "inner"), ("arr", 32, 24, "elem")),
        "inner@a.c:16": _record("inner", ("x", 0, 8, None)),
        "elem@a.c:8": _record("elem", ("v", 0, 8, None)),
    }
    access = {"unit": {**global_unit("G"), "record": "outer"}, "access": "read", "field": None, "provenance": where()}
    f = function("f", accesses=(access,))
    f["events"]["field_accesses"] = [
        {"struct": name, "field": field, "offset": 0, "displacement": 0, "base": "pointer", "size": 8, "bitfield": False,
         "provenance": where()}
        for name, field in (("inner", "x"), ("elem", "v"), ("other", "y"))
    ]  # fmt: skip
    tus = {"a.c": {**unit(f), "records": records}}
    image = StubImage({"f": [(0x100, False, "")]}, {0x100: FunctionFacts(globals=image_globals("G"))})
    image.object_symbols = {"G": [Symbol(0x9000, 128, "G", ".data", False, "")]}
    graph = build(tus, image)
    assert graph["analysis"]["global_struct_offsets"] == {
        "function:f": [
            {"global": "G", "struct": "elem", "offsets": [32, 40, 48, 96, 104, 112]},
            {"global": "G", "struct": "inner", "offsets": [8, 72]},
        ]
    }


def _global_graph(item: dict[str, Any], places: list[int], image_offsets: list[int]) -> dict[str, Any]:
    graph = _structure_graph(item, ["tk_data"], {})
    graph["analysis"]["image"]["globals"] = [
        {"function": "function:f", "global": "tk_data", "offset": offset, "target_class": "state"}
        for offset in image_offsets
    ]
    graph["analysis"]["global_struct_offsets"] = {
        "function:f": [{"global": "tk_data", "struct": "tk", "offsets": places}]
    }
    return graph


def test_a_field_matches_inside_a_global_at_any_place_its_struct_has_there() -> None:
    exact = checks.structure(_global_graph(_field(56), [0, 120], [176]))["field_offsets"]
    assert exact["by_method"] == {"global_object_field": 1}
    inside = checks.structure(_global_graph(_field(56), [120], [181]))["field_offsets"]
    assert inside["by_method"] == {"global_object_narrower_access": 1}
    assert checks.structure(_global_graph(_field(56), [120], [184]))["field_offsets"]["matched"] == 0
    assert checks.structure(_global_graph(_field(56), [], [56]))["field_offsets"]["matched"] == 0


def test_a_folded_callee_s_field_through_its_parameter_is_rebased_by_the_argument_s_displacement() -> None:
    """wb_exit(&bdi->wb) folded into bdi_put: wb->completions@248 is 64 + 248 from bdi."""
    access = {"struct": "wb", "field": "completions", "offset": 248, "displacement": 248, "base": "pointer",
              "size": 8, "bitfield": False, "param": 0, "provenance": where()}  # fmt: skip
    child = link.Effective(field_accesses=[access])
    member = {"unit": {"kind": "struct", "name": "bdi", "def": "a.c:1", "field": "wb", "offset": 64},
              "param": 1, "displacement": 64}  # fmt: skip
    (rebased,) = link._bind(child, [member]).field_accesses
    assert (rebased["displacement"], rebased["param"]) == (312, 1)
    (plain,) = link._bind(child, [{**parameter(2), "displacement": 0}]).field_accesses
    assert (plain["displacement"], plain["param"]) == (248, 2)
    (unknown,) = link._bind(child, [{"unit": None, "param": None}]).field_accesses
    assert (unknown["displacement"], unknown.get("param")) == (248, None)


def test_a_rebased_field_also_matches_where_the_image_keeps_the_member_address_in_a_register() -> None:
    """ida_init(&pool->worker_ida) folded: xa->xa_flags is 728 + 4 from pool, or 4 from a register
    that holds &pool->worker_ida."""
    access = {"struct": "xarray", "field": "xa_flags", "offset": 4, "displacement": 4, "base": "pointer",
              "size": 4, "bitfield": False, "param": 0, "provenance": where()}  # fmt: skip
    member = {"unit": None, "param": 0, "displacement": 728}
    (rebased,) = link._bind(link.Effective(field_accesses=[access]), [member]).field_accesses
    assert (rebased["displacement"], rebased["inner_displacement"]) == (732, 4)
    item = {**_field(4), "displacement": 732, "inner_displacement": 4}
    graph = _structure_graph(item, [], {})
    graph["analysis"]["image"]["memory_offsets"] = {"function:f": [4, 200]}
    assert checks.structure(graph)["field_offsets"]["by_method"] == {"memory_displacement": 1}


def test_a_pointer_loaded_through_the_parameter_is_not_rebased_onto_it() -> None:
    """fsnotify_path(&file->f_path) calls fsnotify_parent(path->dentry): dentry->d_flags@0 is in the
    dentry the load gives, not 64 bytes into the file."""
    access = {"struct": "dentry", "field": "d_flags", "offset": 0, "displacement": 0, "base": "pointer",
              "size": 4, "bitfield": False, "param": 0, "provenance": where()}  # fmt: skip
    loaded = {"unit": {"kind": "struct", "name": "dentry", "def": "a.c:1", "field": None}, "param": 0}
    (inner,) = link._bind(link.Effective(field_accesses=[access]), [loaded]).field_accesses
    assert (inner["displacement"], inner.get("param")) == (0, None)
    member = {"unit": {"kind": "struct", "name": "file", "def": "a.c:1", "field": "f_path"}, "param": 0,
              "displacement": 64}  # fmt: skip
    (outer,) = link._bind(link.Effective(field_accesses=[inner]), [member]).field_accesses
    assert (outer["displacement"], outer.get("param")) == (0, None)
    itself = {"unit": {"kind": "struct", "name": "dentry", "def": "a.c:1", "field": None}, "param": 3,
              "displacement": 0}  # fmt: skip
    (same,) = link._bind(link.Effective(field_accesses=[access]), [itself]).field_accesses
    assert (same["displacement"], same["param"]) == (0, 3)


def test_a_chain_field_matches_inside_a_global_by_its_own_offset_in_its_struct() -> None:
    """pgdat->__lruvec.lru_lock: the displacement (4048) is from pglist_data, the field's offset (16)
    is in lruvec, which sits at 4032 in contig_page_data."""
    item = {**_field(16), "displacement": 4048}
    assert checks.structure(_global_graph(item, [4032], [4048]))["field_offsets"]["by_method"] == {
        "global_object_field": 1
    }
    assert checks.structure(_global_graph(item, [4032], [8080]))["field_offsets"]["matched"] == 0


def test_a_field_matches_where_the_image_keeps_the_element_or_the_global_s_member_in_a_register() -> None:
    """o->slots[k].b: displacement 108 (slots 96 + b 12); the image may hold &slots[k] and load 12(rK).
    tk = &tk_core.shadow; tk->f: base global at shadow + f; the image may hold &shadow, f(rK)."""
    element = {**_field(108), "offset": 12, "displacement": 108, "element_displacement": 12}
    graph = _structure_graph(element, [], {})
    graph["analysis"]["image"]["memory_offsets"] = {"function:f": [12, 200]}
    assert checks.structure(graph)["field_offsets"]["by_method"] == {"memory_displacement": 1}
    member = {**_field(232), "base": "global", "offset": 232, "displacement": 544}
    graph = _structure_graph(member, [], {})
    graph["analysis"]["image"]["memory_offsets"] = {"function:f": [232, 8]}
    assert checks.structure(graph)["field_offsets"]["by_method"] == {"memory_displacement": 1}
    plain = {**_field(108), "offset": 12, "displacement": 108}
    graph = _structure_graph(plain, [], {})
    graph["analysis"]["image"]["memory_offsets"] = {"function:f": [12, 200]}
    assert checks.structure(graph)["field_offsets"]["matched"] == 0


def test_link_carries_the_facts_field_keys_the_structure_check_reads() -> None:
    """o->slots[k].b through parameter 0: the image keeps &slots[k] and loads 12(rK)."""
    f = function("f")
    f["events"]["field_accesses"] = [
        {"struct": "o", "field": "b", "offset": 12, "displacement": 108, "base": "pointer", "size": 4,
         "bitfield": False, "param": 0, "element_displacement": 12, "provenance": where()}
    ]  # fmt: skip
    image = StubImage({"f": [(0x100, False, "")]}, {0x100: FunctionFacts(memory_offsets={12, 200})})
    graph = build({"a.c": unit(f)}, image)
    (item,) = graph["analysis"]["field_accesses"]["function:f"]
    assert (item["element_displacement"], item["param"]) == (12, 0)
    assert checks.structure(graph)["field_offsets"]["by_method"] == {"memory_displacement": 1}


def test_a_c_global_at_the_start_of_a_merged_block_the_image_forms_is_addressed() -> None:
    """workqueue_softirq_dead passes init_completion.__key's address: the image forms the start of a
    merged-globals block, where __key (and wq_power_efficient) sit. A member after the start is not."""
    block = {
        "kind": "image_access",
        "reason": "merged_globals_block_base",
        "function": "f",
        "target": "0x9000",
        "detail": {"block_bytes": 896, "names": ["key", "power"]},
        "provenance": [{"source": "image", "address": "0x104"}],
    }
    tus = {"a.c": unit(function("f", accesses=(read("key"), read("later"))))}
    image = StubImage({"f": [(0x100, False, "")]}, {0x100: FunctionFacts(exclusions=[block])})
    image.map_source_names = frozenset({"key", "later", "power"})
    graph = build(tus, image)
    assert graph["analysis"]["image"]["block_start_names"] == {"function:f": ["key", "power"]}
    layer = checks.accesses(graph)
    assert (layer["agree"], layer["only_c"]) == (1, 1)
