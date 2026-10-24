"""One translation unit to facts: plain JSON that depends on that unit's text and nothing else.

A unit's facts hold every function the unit owns (those its file or a C file it includes defines, and
those with external linkage that a header it includes defines), and every function of an included
header that those functions reach (by a call, an implied cleanup call, or an address) or that a
file-scope initializer names. For each function: its calls, its accesses to state, its list and hash
insertions, its calls through a function-pointer field, the functions it uses as values (passes,
stores or compares), and the byte offsets of the fields it names. The unit also gives the layout of
each record those accesses name, its callback tables (designated initializers of function-pointer
fields), and the lines where a membership macro appears.

What the compiler does not evaluate is not an access: the operand of ``sizeof`` and ``_Alignof``,
the controlling expression and the unselected associations of ``_Generic``, the type expressions
of a declaration or a cast (``typeof``), and the branch of an ``if`` or ``?:`` whose condition clang
folds to a constant. Those references are kept as ``unevaluated`` with the reason. Where a branch
depends on the function's parameters, its events give ``guards``: indexes into the function's table
of conditions on those parameters, which a caller's constant arguments decide at a fold (link._bind).
"""

from __future__ import annotations

import collections
import re
import sys
from collections.abc import Callable, Iterator
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, ClassVar, TypeVar

import clang.cindex as ci
from clang.cindex import CursorKind as K
from clang.cindex import TypeKind as TK

from . import expressions, jsonio, libclang
from .config import Vocabulary
from .errors import ExtractionError

FORMAT = "cgraph/tu-facts/v1"
Facts = dict[str, Any]
Unit = dict[str, Any]
# A kept answer of the extractor's analyses (``_Extractor.reuse``).
_T = TypeVar("_T")

_RECORD_KINDS = (K.STRUCT_DECL, K.UNION_DECL)
_TABLE_ENTRY = re.compile(r"\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*&?\s*([A-Za-z_][A-Za-z0-9_]*)")
_INCLUDE_DIRECTIVE = re.compile(r'(?m)^\s*#\s*include(?:_next)?\s*(?:"([^"]+)"|<([^>]+)>)')
_HAS_INCLUDE = re.compile(r'__has_include(?:_next)?\s*\(\s*(?:"([^"]+)"|<([^>]+)>)\s*\)')
_INCLUDE_DIR_FLAGS = ("-iquote", "-I", "-isystem", "-idirafter")

_LAYOUT_ERRORS = {
    -1: "invalid",
    -2: "incomplete",
    -3: "dependent",
    -4: "not_constant_size",
    -5: "invalid_field_name",
    -6: "undeduced",
}


def _layout_error(size: int) -> str | None:
    return _LAYOUT_ERRORS.get(size)


def _include_spellings(text: str) -> list[tuple[str, bool]]:
    """Literal include spellings and whether each has quoted-header search semantics."""
    result: list[tuple[str, bool]] = []
    for pattern in (_INCLUDE_DIRECTIVE, _HAS_INCLUDE):
        for match in pattern.finditer(text):
            quoted, angled = match.groups()
            result.append((quoted or angled, quoted is not None))
    return result


def _include_directories(arguments: list[str], c_tree: Path) -> list[Path]:
    """Search directories in the preprocessor's quoted-header order."""
    grouped: dict[str, list[str]] = {flag: [] for flag in _INCLUDE_DIR_FLAGS}
    index = 0
    while index < len(arguments):
        argument = arguments[index]
        if argument in grouped and index + 1 < len(arguments):
            grouped[argument].append(arguments[index + 1])
            index += 2
            continue
        flag = next(
            (candidate for candidate in _INCLUDE_DIR_FLAGS if argument.startswith(candidate) and argument != candidate),
            None,
        )
        if flag is not None:
            grouped[flag].append(argument[len(flag) :])
        index += 1
    return [
        (c_tree / directory).resolve() if not Path(directory).is_absolute() else Path(directory).resolve()
        for flag in _INCLUDE_DIR_FLAGS
        for directory in grouped[flag]
    ]


def _include_probes(tu: ci.TranslationUnit, main: Path, arguments: list[str], extractor: _Extractor) -> list[str]:
    """Every location libclang could consult for literal include and ``__has_include`` spellings."""
    directories = _include_directories(arguments, extractor.tree)
    files = {main, *(Path(include.include.name) for include in tu.get_includes())}
    probes: set[str] = set()
    for file in files:
        if not file.is_file():
            continue
        for spelling, quoted in _include_spellings(file.read_text(encoding="utf-8", errors="replace")):
            candidates = [file.parent / spelling] if quoted else []
            candidates.extend(directory / spelling for directory in directories)
            probes.update(extractor.relative(str(candidate)) for candidate in candidates)
    return sorted(probes)


def _named_struct(cursor: ci.Cursor | None) -> ci.Cursor | None:
    """The innermost named record that contains ``cursor`` (anonymous members belong to it)."""
    while cursor is not None and cursor.kind in _RECORD_KINDS and (not cursor.spelling or cursor.is_anonymous()):
        cursor = cursor.semantic_parent
    if cursor is None or cursor.kind not in _RECORD_KINDS or not cursor.spelling:
        return None
    return cursor


def _canonical_record(typ: ci.Type) -> tuple[ci.Cursor | None, bool, bool]:
    """The record a type reaches, whether through a pointer, and whether through an array."""
    typ = typ.get_canonical()
    pointer = array = False
    while typ.kind in (TK.POINTER, TK.CONSTANTARRAY, TK.INCOMPLETEARRAY):
        if typ.kind == TK.POINTER:
            pointer = True
            typ = typ.get_pointee().get_canonical()
        else:
            array = True
            typ = typ.element_type.get_canonical()
    if typ.kind != TK.RECORD:
        return None, pointer, array
    declaration = typ.get_declaration()
    if not declaration.spelling or declaration.is_anonymous():
        return _named_struct(declaration), pointer, array
    return declaration, pointer, array


def _is_function_pointer(typ: ci.Type) -> bool:
    canonical = typ.get_canonical()
    return canonical.kind == TK.POINTER and canonical.get_pointee().kind in (TK.FUNCTIONPROTO, TK.FUNCTIONNOPROTO)


def _is_global_var(cursor: ci.Cursor | None) -> bool:
    if cursor is None or cursor.kind != K.VAR_DECL:
        return False
    parent = cursor.semantic_parent
    if parent is not None and parent.kind == K.TRANSLATION_UNIT:
        return True
    return bool(cursor.storage_class == ci.StorageClass.STATIC)


def _children(cursor: ci.Cursor) -> list[ci.Cursor]:
    return list(cursor.get_children())


def _operand(cursor: ci.Cursor) -> ci.Cursor | None:
    """The one expression a parenthesis, a cast or an implicit conversion wraps, or None for a
    construct libclang does not expose and gives several expressions (``__builtin_choose_expr``,
    whose last child is an alternative it may not select)."""
    operands = [child for child in _children(cursor) if child.kind.is_expression()]
    return operands[0] if len(operands) == 1 else None


def _is_unnamed_member(field: ci.Cursor) -> bool:
    """An anonymous struct or union member: libclang spells it ``(anonymous ...)`` or not at all."""
    return not field.spelling or "(anonymous" in field.spelling or "(unnamed" in field.spelling


def _strip(node: ci.Cursor) -> ci.Cursor:
    while node.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR, K.CSTYLE_CAST_EXPR) and _children(node):
        node = _children(node)[-1]
    return node


def _strip_parens(node: ci.Cursor) -> ci.Cursor:
    while node.kind == K.PAREN_EXPR and _children(node):
        node = _children(node)[0]
    return node


def _evaluated_nodes(cursor: ci.Cursor) -> Iterator[ci.Cursor]:
    """The nodes of an expression in preorder, without the parts that are never evaluated: sizeof
    and alignof operands, cast and compound-literal types, the types of declarations (typeof), the
    operands of __builtin_constant_p, and the statements of ``({ ... })`` before its value.
    runtime_const_ptr(__dentry_cache) names its symbol only in ``typeof(sym) __ret``."""
    pending = [cursor]
    while pending:
        node = pending.pop()
        yield node
        children = _children(node)
        kind = node.kind
        if kind == K.CXX_UNARY_EXPR or (kind == K.CALL_EXPR and node.spelling in _IMAGE_DECIDES):
            continue
        if kind == K.StmtExpr:
            statements = _children(children[0]) if children else []
            children = statements[-1:] if statements and statements[-1].kind.is_expression() else []
        elif kind == K.VAR_DECL:
            initializer = libclang.var_initializer(node)
            children = [initializer] if initializer is not None else []
        elif kind in (K.CSTYLE_CAST_EXPR, K.COMPOUND_LITERAL_EXPR):
            children = children[-1:]
        pending.extend(reversed(children))


_ARRAY_TYPES = (TK.CONSTANTARRAY, TK.INCOMPLETEARRAY, TK.VARIABLEARRAY)
# CXUnaryOperatorKind Deref.
_DEREF = 6
# Bases that name an object the function itself names; a pointer value (given, loaded, or less a
# container_of offset) is "pointer" or "container_of".
_OBJECT_BASES = ("global", "local", "other", "unknown")
# What an argument says of the address it passes (argument()), which a value standing for it lacks.
_ADDRESS_KEYS = frozenset({"displacement", "base", "object", "object_size", "element", "index", "inner"})


def _bare(node: ci.Cursor) -> ci.Cursor:
    """An expression without its parentheses and implicit conversions."""
    node = _strip_parens(node)
    while node.kind == K.UNEXPOSED_EXPR and len(_children(node)) == 1:
        node = _strip_parens(_children(node)[0])
    return node


@dataclass(frozen=True)
class _Address:
    """Where an address points: ``bits`` from the value the base names, what that base is, the
    function's parameter it is when it is one, past a variable index the bits from the start of that
    indexed element, the bits from a register the machine code may hold instead (the value a helper
    the image may call returns, or the address a loop keeps reassigning), and the variable indices
    that are parameters: each parameter's position, the bits one step adds, and the constant the index
    adds to the parameter (``sig - 1``: -1), whose steps ``bits`` counts while the parameter's count
    as 0; a caller that passes a constant fixes the term (__sb_end_write(sb, SB_FREEZE_WRITE)). A
    local object's address also names that object and its bytes, so its fields can be found in the
    frame."""

    bits: int
    base: str
    param: int | None = None
    element: int | None = None
    inner: int | None = None
    index: tuple[tuple[int, int, int], ...] = ()
    object: tuple[str, int] | None = None

    def shifted(self, bits: int) -> _Address:
        element = self.element + bits if self.element is not None else None
        inner = self.inner + bits if self.inner is not None else None
        return replace(self, bits=self.bits + bits, element=element, inner=inner)


def _indexed(address: _Address, indexed: tuple[int, int] | None, scale: int) -> _Address:
    """An address past a variable index, with the index's term when the index is a parameter plus a
    constant (``index_of``)."""
    if indexed is None or address.base == "unknown":
        return address
    return replace(address, index=(*address.index, (indexed[0], scale, indexed[1])))


def _index_terms(address: _Address) -> list[dict[str, int]]:
    """An address's parameter indices as the facts give them: the parameter, the bytes a step adds,
    and the constant the index adds to the parameter where it is not 0 (the displacement counts its
    steps; the machine code may keep it in the index register instead: kernel_sigaction's
    ``action[sig - 1]``)."""
    return [
        {"param": param, "scale": scale // 8, **({"constant": constant} if constant else {})}
        for param, scale, constant in address.index
    ]


def _agree(addresses: set[_Address]) -> _Address | None:
    """The one address several definitions give: they agree on the base, the bits and the parameter
    indices; a parameter, an element or an inner register they disagree on is dropped. Definitions
    that each name a local object (sched_balance_find_dst_cpu's sgs = &local_sgs or &tmp_sgs) give
    the choice of them, ``a|b``, as large as the largest."""
    objects = {address.object for address in addresses}
    if len(objects) > 1 and None not in objects and {address.base for address in addresses} == {"local"}:
        names = sorted({part for held in objects if held is not None for part in held[0].split("|")})
        choice = ("|".join(names), max(held[1] for held in objects if held is not None))
        addresses = {replace(address, object=choice) for address in addresses}
    agreed = {(address.bits, address.base, address.index, address.object) for address in addresses}
    if len(agreed) != 1:
        return None
    (bits, base, index, held) = agreed.pop()
    params = {address.param for address in addresses}
    elements = {address.element for address in addresses}
    inners = {address.inner for address in addresses}
    return _Address(
        bits,
        base,
        params.pop() if len(params) == 1 else None,
        elements.pop() if len(elements) == 1 else None,
        inners.pop() if len(inners) == 1 else None,
        index,
        held,
    )


def _object_keys(address: _Address) -> dict[str, Any]:
    """The local object an address is in, as the facts give it: its name and its bytes."""
    if address.object is None:
        return {}
    name, size = address.object
    return {"object": name, "object_size": size}


def _element_bytes(pointer: ci.Cursor) -> int:
    """The bytes one step of pointer arithmetic moves: 1 for ``void *`` and ``char *``."""
    pointee = pointer.type.get_canonical().get_pointee().get_canonical()
    size = pointee.get_size() if pointee.kind != TK.VOID else 1
    return size if size > 0 else 1


# Where a construct is expanded: its file and the byte offsets its text spans there.
_Place = tuple[str, int, int]
_LOOPS = (K.FOR_STMT, K.WHILE_STMT, K.DO_STMT)


def _place(cursor: ci.Cursor) -> _Place | None:
    """The file and byte range of a construct where it is expanded (a macro's whole invocation for
    what the macro writes), or None when its extent does not lie in one file."""
    extent = cursor.extent
    start, end = extent.start, extent.end
    if start.file is None or end.file is None or start.file.name != end.file.name:
        return None
    return start.file.name, start.offset, end.offset


def _is_null(expression: ci.Cursor) -> bool:
    """A null pointer constant: ``0`` or ``((void *)0)``."""
    node = _strip(expression)
    return node.kind == K.INTEGER_LITERAL and libclang.constant_int(node) == 0


def _container(node: ci.Cursor) -> tuple[ci.Cursor, int] | None:
    """container_of's pointer and the bytes it subtracts: ``(type *)(__mptr - offsetof(type, member))``,
    also as the value of the statement expression include/linux/container_of.h writes."""
    node = _strip(node)
    if node.kind == K.StmtExpr:
        body = _children(node)
        statements = _children(body[0]) if body else []
        return _container(statements[-1]) if statements else None
    if node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == libclang.SUBTRACT:
        operands = _children(node)
        if len(operands) == 2 and operands[0].type.get_canonical().kind == TK.POINTER:
            value = libclang.constant_int(operands[1])
            if value is not None:
                return operands[0], value * _element_bytes(operands[0])
    return None


def _member_bits(node: ci.Cursor) -> int | None:
    """The bits a member address ``&p->a.b`` adds to the pointer ``p`` it is reached through: the
    offsets of its members up to the first ``->``, or None for another expression."""
    node, bits = _strip(node), 0
    while node.kind == K.MEMBER_REF_EXPR and node.referenced is not None and node.referenced.kind == K.FIELD_DECL:
        children = _children(node)
        if not children:
            return None
        base = _unnamed_base(children[0])
        typ = base.type.get_canonical()
        through = typ.kind == TK.POINTER
        record = typ.get_pointee().get_canonical() if through else typ
        offset = record.get_offset(node.referenced.spelling) if record.kind == TK.RECORD else -1
        if offset < 0:
            return None
        bits += offset
        if through:
            return bits
        node = _strip(base)
    return None


def _static_address(node: ci.Cursor) -> tuple[ci.Cursor, int] | None:
    """The global and the bits into it a file-scope initializer's address constant names: ``&g``,
    ``&g.a.b``, ``&g.a[2]``, or a global array that decays to its address."""
    node = _strip(node)
    bits = 0
    if node.kind == K.UNARY_OPERATOR and libclang.is_address_of(node) and _children(node):
        node = _strip(_children(node)[0])
    elif not (node.kind == K.DECL_REF_EXPR and node.type.get_canonical().kind in _ARRAY_TYPES):
        return None
    while True:
        if node.kind == K.DECL_REF_EXPR and _is_global_var(node.referenced):
            return node.referenced, bits
        children = _children(node)
        if node.kind == K.MEMBER_REF_EXPR and children and node.referenced is not None:
            base = _unnamed_base(children[0])
            record = base.type.get_canonical()
            offset = record.get_offset(node.referenced.spelling) if record.kind == TK.RECORD else -1
            if offset < 0:
                return None
            bits += offset
            node = _strip(base)
        elif node.kind == K.ARRAY_SUBSCRIPT_EXPR and len(children) == 2:
            value, size = libclang.constant_int(children[1]), node.type.get_canonical().get_size()
            array = _strip(children[0])
            if value is None or size <= 0 or array.type.get_canonical().kind not in _ARRAY_TYPES:
                return None
            bits += value * size * 8
            node = array
        else:
            return None


def _unnamed_base(base: ci.Cursor) -> ci.Cursor:
    """The base below the unnamed struct and union members a named field is reached through: clang
    gives the field's offset from that record (also for a tagged member without a name, as
    pipe_inode_info's struct pipe_index under -fms-extensions)."""
    while True:
        inner = _strip(base)
        if (
            inner.kind == K.MEMBER_REF_EXPR
            and inner.referenced is not None
            and _is_unnamed_member(inner.referenced)
            and _children(inner)
        ):
            base = _children(inner)[0]
            continue
        return base


_SCALAR_KINDS = (
    TK.BOOL,
    TK.CHAR_U,
    TK.UCHAR,
    TK.CHAR_S,
    TK.SCHAR,
    TK.USHORT,
    TK.UINT,
    TK.ULONG,
    TK.ULONGLONG,
    TK.SHORT,
    TK.INT,
    TK.LONG,
    TK.LONGLONG,
    TK.ENUM,
)


def _is_constant_scalar(variable: ci.Cursor) -> bool:
    """A const integer or enum variable whose definition has an initializer."""
    typ = variable.type
    if not typ.is_const_qualified() or typ.get_canonical().kind not in _SCALAR_KINDS:
        return False
    definition = variable.get_definition() or variable
    return libclang.var_initializer(definition) is not None


class _Extractor:
    def __init__(self, tu: ci.TranslationUnit, source: str, c_tree: Path, vocabulary: Vocabulary) -> None:
        self.tu = tu
        self.source = source
        self.tree = c_tree.resolve()
        self.vocabulary = vocabulary
        self.transparent = vocabulary.kit_types | vocabulary.lock_types
        self.records: dict[str, ci.Cursor] = {}
        self.relative_cache: dict[str, str] = {}
        self.generic = {"selected": 0, "ambiguous": 0}
        self.plain: dict[str, bool] = {}
        # The unit's macro definitions (the last definition of a name wins), read on demand.
        self.macros: dict[str, ci.Cursor] = {
            cursor.spelling: cursor for cursor in tu.cursor.get_children() if cursor.kind == K.MACRO_DEFINITION
        }
        self.macro_cache: dict[str, tuple[frozenset[str], list[str]] | None] = {}
        self.weak_cache: dict[int, bool] = {}
        self.evaluator = _Evaluator(
            {cursor.spelling: cursor for cursor in tu.cursor.get_children()
             if cursor.kind == K.FUNCTION_DECL and cursor.is_definition() and cursor.spelling},
            self.is_weak,
        )  # fmt: skip
        self.ignored_cache: dict[str, tuple[frozenset[int], int | None]] = {}
        self.returned: dict[str, _Address | None] = {}
        self.returned_bits: dict[str, int | None] = {}
        self.cancel_cache: dict[tuple[str, int, int], bool] = {}
        self.null_helpers: set[str] = set()  # the helpers whose every return is a null pointer
        self.nullable_helpers: set[str] = set()  # the helpers that return a null pointer or an address
        self.texts: dict[Path, bytes] = {}
        # Each function's settled walk, the functions whose walk is under way, the functions whose
        # calls compute only their result, and each _Generic's selection, read once per node.
        # Each kept answer goes with its horizon (see ``horizon``).
        self.analyses: dict[str, tuple[_FunctionWalk, int | None]] = {}
        self.unused_analyses: dict[str, tuple[_FunctionWalk, int | None]] = {}  # returns discarded
        self.analyzing: set[str] = set()
        self.effect_free_cache: dict[str, tuple[bool, int | None]] = {}
        self.nesting = 0  # analyses under way, one inside another: bounded by _EFFECT_DEPTH
        self.depth_cuts = 0  # calls not followed at the depth bound, and kept answers resting on one
        self.selections: dict[int, set[int] | None] = {}
        self.static_locals: dict[int, ci.Cursor] = {}  # the static locals walked: their initializers are data
        self.typedefs: dict[str, str] | None = None  # the unit's typedef names, read on demand

    # Locations -----------------------------------------------------------------------------

    def relative(self, name: str | None) -> str:
        if not name:
            return ""
        cached = self.relative_cache.get(name)
        if cached is None:
            path = Path(name)
            try:
                cached = str(path.resolve().relative_to(self.tree))
            except ValueError:
                cached = str(path)
            self.relative_cache[name] = cached
        return cached

    def location(self, cursor: ci.Cursor) -> dict[str, Any]:
        location = cursor.location
        return {
            "source": "c",
            "file": self.relative(location.file.name if location.file else None),
            "line": location.line,
        }

    def where(self, cursor: ci.Cursor) -> str:
        location = self.location(cursor)
        return f"{location['file']}:{location['line']}"

    def object_name(self, variable: ci.Cursor) -> str:
        """A local object's name in the facts: its spelling and where it is declared."""
        location = variable.location
        return f"{variable.spelling}@{self.relative(location.file.name if location.file else None)}:{location.line}:{location.column}"

    def token_error(self, cursor: ci.Cursor, error: RuntimeError) -> ExtractionError:
        return ExtractionError(f"{self.source}: {self.where(cursor)}: cannot read tokens: {error}")

    def is_weak(self, definition: ci.Cursor) -> bool:
        key = definition.hash
        if key not in self.weak_cache:
            self.weak_cache[key] = _is_weak(self, definition)
        return self.weak_cache[key]

    def attribute_name(self, attribute: ci.Cursor) -> str | None:
        """The identifier an attribute is written with where it is used: the attribute itself
        (``weak`` in ``__attribute__((weak))``) or the macro that writes it (``__weak``)."""
        path, offset = libclang.spelling_location(attribute.extent.start)
        if path is None:
            return None
        text = self.texts.get(path)
        if text is None:
            text = self.texts[path] = path.read_bytes()
        match = _IDENTIFIER_BYTES.match(text, offset)
        return match.group().decode() if match else None

    def spells_weak(self, name: str, depth: int = 0) -> bool:
        """``name`` is a weak attribute, or a macro whose expansion writes one
        (include/linux/compiler_attributes.h: ``#define __weak __attribute__((__weak__))``)."""
        if name in _WEAK_ATTRIBUTES:
            return True
        macro = self.macro(name) if depth < 8 else None
        return macro is not None and any(
            self.spells_weak(token, depth + 1) for token in macro[1] if token != name and _IDENTIFIER.match(token)
        )

    def returned_address(self, name: str) -> _Address | None:
        """Where a helper of the unit returns a pointer to, from the parameter it derives it from when
        it has one (``return &desc->irq_data``, ``return container_of(mnt, ...)``), or None when its
        returns disagree; ``return NULL`` is no address. A call the compiler inlines adds those
        offsets to the argument; the compiler also inlines static helpers not declared inline
        (inode_to_wb_and_lock_list), and the address is the same when it calls them."""
        if name in self.returned:
            return self.returned[name]
        self.returned[name] = None
        definition = self.evaluator.definitions.get(name)
        if definition is None or self.is_weak(definition):
            return None
        walk = _FunctionWalk(self, definition)
        found: set[_Address] = set()
        null = False
        for node in definition.walk_preorder():
            if node.kind == K.RETURN_STMT:
                values = _children(node)
                if not values:
                    return None
                if _is_null(values[0]):
                    null = True
                else:
                    found.add(walk.pointer(values[0], 0, [], 0))
        if null and not found:
            self.null_helpers.add(name)
        elif null:
            self.nullable_helpers.add(name)
        address = _agree(found)
        if address is None or address.base in ("unknown", "local"):
            return None
        self.returned[name] = address
        return address

    def returns_null(self, expression: ci.Cursor) -> bool:
        """A call of a helper of the unit whose every return is a null pointer (group_cfs_rq without
        group scheduling)."""
        node = _strip(expression)
        if node.kind != K.CALL_EXPR or node.referenced is None or node.referenced.kind != K.FUNCTION_DECL:
            return False
        self.returned_address(node.referenced.spelling)
        return node.referenced.spelling in self.null_helpers

    def returned_member(self, name: str) -> int | None:
        """The bits a helper's returned member address adds to the pointer it is reached through,
        where every return of the helper is such an address of the same bits (dl_rq_of_se's ``return
        &rq_of_dl_se(dl_se)->dl;``), else None."""
        if name in self.returned_bits:
            return self.returned_bits[name]
        self.returned_bits[name] = None
        definition = self.evaluator.definitions.get(name)
        if definition is None or self.is_weak(definition):
            return None
        found: set[int | None] = set()
        for node in definition.walk_preorder():
            if node.kind == K.RETURN_STMT:
                values = _children(node)
                address = _strip(values[0]) if values else None
                if address is None or address.kind != K.UNARY_OPERATOR or not libclang.is_address_of(address):
                    return None
                found.add(_member_bits(_children(address)[0]) if _children(address) else None)
        bits = found.pop() if len(found) == 1 else None
        self.returned_bits[name] = bits
        return bits

    def cancels(self, name: str, position: int, bits: int) -> bool:
        """Every use of a helper's parameter takes ``bits`` back off it with container_of (fs/mount.h
        is_mounted's real_mount(mnt), kernel/sched/sched.h cfs_rq_of's p = task_of(se)): where the
        compiler inlines the helper, a member address of those bits passed there is never formed."""
        key = (name, position, bits)
        if key in self.cancel_cache:
            return self.cancel_cache[key]
        # A helper that reaches itself again takes nothing back on that way.
        self.cancel_cache[key] = False
        definition = self.evaluator.definitions.get(name)
        parameters = list(definition.get_arguments() or []) if definition is not None else []
        if definition is None or self.is_weak(definition) or position >= len(parameters):
            return False
        result = _FunctionWalk(self, definition).variable_cancelled(parameters[position], bits)
        self.cancel_cache[key] = result
        return result

    # State units ---------------------------------------------------------------------------

    def is_lock_type(self, typ: ci.Type) -> bool:
        record, _, _ = _canonical_record(typ)
        if record is not None and record.spelling in self.vocabulary.lock_types:
            return True
        spelling = typ.spelling
        return any(word in spelling for word in self.vocabulary.lock_type_words)

    def is_cleanup_class(self, name: str) -> bool:
        pattern = self.vocabulary.cleanup_class
        return pattern is not None and pattern.fullmatch(name) is not None

    def record_unit(self, record: ci.Cursor, field: ci.Cursor | None, lock: bool = False) -> Unit | None:
        name = record.spelling
        if not name or name in self.transparent or self.is_cleanup_class(name):
            return None
        definition = record.get_definition() or record
        key = f"{name}@{self.where(definition)}"
        self.records[key] = definition
        unit: Unit = {
            "kind": "struct",
            "name": name,
            "def": self.where(definition),
            "field": field.spelling if field is not None else None,
            "lock": lock,
        }
        if field is not None:
            # The offset in the named struct, through any anonymous struct or union on the way.
            offset = record.type.get_offset(field.spelling)
            unit["offset"] = offset // 8 if offset >= 0 else -1
        return unit

    def global_unit(self, referenced: ci.Cursor) -> Unit:
        parent = referenced.semantic_parent
        while parent is not None and parent.kind not in (K.FUNCTION_DECL, K.TRANSLATION_UNIT):
            parent = parent.semantic_parent
        local = parent is not None and parent.kind == K.FUNCTION_DECL
        name = f"{parent.spelling}.{referenced.spelling}" if local and parent is not None else referenced.spelling
        definition = referenced.get_definition() or referenced
        internal = local or referenced.linkage == ci.LinkageKind.INTERNAL
        record, pointer, _ = _canonical_record(referenced.type)
        return {
            "kind": "global",
            "name": name,
            "def": self.where(definition) if internal else None,
            "field": None,
            "lock": self.is_lock_type(referenced.type),
            "record": record.spelling if record is not None and not pointer else None,
        }

    def global_from_ref(self, cursor: ci.Cursor) -> Unit | None:
        """The global whose own storage an expression names, through ``.``, ``[]`` on an array (also
        where the array decays to a pointer, as ``timekeeper_data[0].shadow_timekeeper``) and ``&``,
        and through ``(&g)->m`` (static_key_enable(&(x)->key) of a global key); any other ``->`` or
        ``*`` leaves the global for the object it points to."""
        node = cursor
        while True:
            children = _children(node)
            if node.kind == K.DECL_REF_EXPR:
                return self.global_unit(node.referenced) if _is_global_var(node.referenced) else None
            if node.kind in (K.MEMBER_REF_EXPR, K.ARRAY_SUBSCRIPT_EXPR) and children:
                if children[0].type.get_canonical().kind == TK.POINTER:
                    base = _strip(children[0])
                    if node.kind == K.ARRAY_SUBSCRIPT_EXPR and base.type.get_canonical().kind in _ARRAY_TYPES:
                        # g[i]: the array decays to a pointer to its own storage.
                        node = base
                        continue
                    if not (node.kind == K.MEMBER_REF_EXPR and base.kind == K.UNARY_OPERATOR
                            and libclang.is_address_of(base) and _children(base)):  # fmt: skip
                        return None
                    node = _children(base)[0]
                    continue
                node = children[0]
            elif (node.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR) and len(children) == 1) or (
                node.kind == K.UNARY_OPERATOR and children and libclang.is_address_of(node)
            ):
                node = children[0]
            else:
                return None

    def unit_from_ref(self, cursor: ci.Cursor) -> Unit | None:
        """The state an expression names: a global (with its field) or a field of a struct type."""
        root = self.global_from_ref(cursor)
        for child in _evaluated_nodes(cursor):
            referenced = child.referenced
            if (
                child.kind == K.MEMBER_REF_EXPR
                and referenced is not None
                and referenced.kind == K.FIELD_DECL
                and not _is_unnamed_member(referenced)
            ):
                if root is not None:
                    root["field"] = referenced.spelling
                    return root
                record = _named_struct(referenced.semantic_parent)
                if record is not None:
                    unit = self.record_unit(record, referenced, lock=self.is_lock_type(referenced.type))
                    if unit is not None:
                        return unit
            if child.kind == K.DECL_REF_EXPR and _is_global_var(referenced):
                return self.global_unit(referenced)
        # Look through implicit conversions (to void * for a free, say) to the expression's own type.
        typed = cursor
        while typed.kind == K.UNEXPOSED_EXPR:
            inner = _children(typed)
            if len(inner) != 1 or not inner[0].kind.is_expression():
                break
            typed = inner[0]
        record, _, _ = _canonical_record(typed.type)
        return self.record_unit(record, None) if record is not None else None

    def lock_unit_from_ref(self, cursor: ci.Cursor) -> Unit | None:
        """The state that owns a lock an argument names: a global, or a lock field of a struct."""
        root = self.global_from_ref(cursor)
        if root is not None:
            root["lock"] = True
            return root
        for child in _evaluated_nodes(cursor):
            referenced = child.referenced
            if (
                child.kind == K.MEMBER_REF_EXPR
                and referenced is not None
                and referenced.kind == K.FIELD_DECL
                and self.is_lock_type(referenced.type)
            ):
                record = _named_struct(referenced.semantic_parent)
                if record is not None:
                    unit = self.record_unit(record, referenced, lock=True)
                    if unit is not None:
                        return unit
        return None

    # Records -------------------------------------------------------------------------------

    def record_fields(self, record: ci.Cursor) -> list[ci.Cursor]:
        """The named fields of a record, each once; the fields of an anonymous struct or union
        member are the record's own (C11), and the unnamed member itself is not a field."""
        fields: list[ci.Cursor] = []

        def descend(node: ci.Cursor) -> None:
            for child in node.get_children():
                if child.kind != K.FIELD_DECL:
                    continue
                inner = child.type.get_canonical().get_declaration()
                if _is_unnamed_member(child) and inner is not None and inner.kind in _RECORD_KINDS:
                    descend(inner)
                else:
                    fields.append(child)

        descend(record)
        return fields

    def is_plain(self, record: ci.Cursor) -> bool:
        """True when no field, at any depth, is a pointer: a value such as an id or a time."""
        key = f"{record.spelling}@{self.where(record)}"
        if key in self.plain:
            return self.plain[key]
        self.plain[key] = False  # a record that contains itself is not a value
        result = True
        for field in self.record_fields(record):
            typ = field.type.get_canonical()
            while typ.kind in (TK.CONSTANTARRAY, TK.INCOMPLETEARRAY):
                typ = typ.element_type.get_canonical()
            if typ.kind in (TK.POINTER, TK.BLOCKPOINTER) or (
                typ.kind == TK.RECORD and not self.is_plain(typ.get_declaration())
            ):
                result = False
                break
        self.plain[key] = result
        return result

    def record_facts(self, record: ci.Cursor) -> dict[str, Any]:
        size = record.type.get_size()
        layout_error = _layout_error(size)
        fields = []
        for field in self.record_fields(record):
            child, pointer, array = _canonical_record(field.type)
            # The offset in this record, through any anonymous member on the way.
            offset = record.type.get_offset(field.spelling)
            offset = offset // 8 if offset >= 0 else -1
            field_size = field.type.get_size()
            field_layout_error = _layout_error(field_size)
            if field_size < 0 and field.type.get_canonical().kind == TK.INCOMPLETEARRAY:
                field_size = 0  # a flexible array member: it starts at its offset and has no fixed size
            entry: dict[str, Any] = {
                "name": field.spelling,
                "offset": offset,
                "size": field_size,
                "pointer": pointer,
                "array": array,
                "line": field.location.line,
                "function_pointer": _is_function_pointer(field.type),
                "lock": self.is_lock_type(field.type),
            }
            if field_layout_error is not None:
                entry["layout_error"] = field_layout_error
            if child is not None and child.spelling:
                definition = child.get_definition() or child
                entry["record"] = child.spelling
                entry["record_def"] = self.where(definition)
                entry["record_plain"] = self.is_plain(definition) if child.get_definition() is not None else False
            fields.append(entry)
        result: dict[str, Any] = {
            "name": record.spelling,
            "def": self.where(record),
            "size": size,
            "plain": self.is_plain(record),
            "fields": fields,
        }
        if layout_error is not None:
            result["layout_error"] = layout_error
        return result

    # Macros --------------------------------------------------------------------------------

    def macro(self, name: str) -> tuple[frozenset[str], list[str]] | None:
        """A macro's parameters and the tokens of its body, or None when ``name`` is no macro."""
        if name not in self.macro_cache:
            cursor = self.macros.get(name)
            if cursor is None:
                self.macro_cache[name] = None
            else:
                tokens = [token.spelling for token in self.tu.get_tokens(extent=cursor.extent)][1:]
                parameters: frozenset[str] = frozenset()
                if libclang.is_macro_function_like(cursor) and tokens[:1] == ["("] and ")" in tokens:
                    end = tokens.index(")")
                    parameters = frozenset(token for token in tokens[1:end] if token != ",")
                    tokens = tokens[end + 1 :]
                self.macro_cache[name] = (parameters, tokens)
        return self.macro_cache[name]

    def written_tokens(self, cursor: ci.Cursor, bound: ci.Cursor) -> list[str]:
        """The tokens of a construct as the source writes it. Inside a macro, libclang gives the
        construct no tokens, so the tokens from its expansion site to the end of that statement
        (a ``;`` outside parentheses, within ``bound``) stand in."""
        try:
            tokens = [token.spelling for token in self.tu.get_tokens(extent=cursor.extent)]
            if tokens:
                return tokens
            # The location of a construct inside a macro is a macro location, which has no tokens;
            # its file, line and column are those of the expansion site.
            site = cursor.location
            if site.file is None:
                return []
            start = ci.SourceLocation.from_position(self.tu, site.file, site.line, site.column)
            extent = ci.SourceRange.from_locations(start, bound.extent.end)
            written: list[str] = []
            depth = 0
            for token in self.tu.get_tokens(extent=extent):
                spelling = token.spelling
                depth += {"(": 1, "[": 1, ")": -1, "]": -1}.get(spelling, 0)
                if (depth <= 0 and spelling in (";", "{", "}")) or depth < 0:
                    break
                written.append(spelling)
            return written
        except RuntimeError as error:
            raise self.token_error(cursor, error) from error

    def expansion_bodies(
        self, cursor: ci.Cursor, bound: ci.Cursor, limit: int = 256
    ) -> list[tuple[frozenset[str], list[str]]]:
        """The text of a construct as written, then the bodies of the macros it expands, transitively."""
        seeds = self.written_tokens(cursor, bound)
        bodies: list[tuple[frozenset[str], list[str]]] = [(frozenset(), seeds)]
        queue = [token for token in seeds if _IDENTIFIER.match(token)]
        seen: set[str] = set()
        while queue and len(seen) < limit:
            name = queue.pop(0)
            if name in seen:
                continue
            seen.add(name)
            macro = self.macro(name)
            if macro is None:
                continue
            bodies.append(macro)
            queue.extend(token for token in macro[1] if _IDENTIFIER.match(token) and token not in seen)
        return bodies

    def selected_associations(self, node: ci.Cursor, bound: ci.Cursor) -> set[int] | None:
        """The index of the association ``_Generic`` selects, or None when it cannot be told.

        libclang gives the association expressions but not their types, and a ``_Generic`` the
        kernel writes sits in a macro. So the macros the expression expands are read, the
        ``_Generic`` association lists in their bodies are parsed, and the list whose expressions
        name what the AST's associations name gives each association its type. Where the text
        cannot tell (associations a macro pastes together, as seqlock.h's ``__seqprop_case``), the
        selection's own type can: it is the type of the expression it selects, so an association
        that alone has that type is the selected one."""
        selected = self._text_selection(node, bound)
        if selected is None:
            selected = _type_selection(node)
        self.generic["selected" if selected is not None else "ambiguous"] += 1
        return selected

    def _text_selection(self, node: ci.Cursor, bound: ci.Cursor) -> set[int] | None:
        children = _children(node)
        control, arms = children[0], children[1:]
        candidates = {_normal_type(control.type.spelling), _normal_type(control.type.get_canonical().spelling)}
        arm_names = [
            {child.spelling for child in arm.walk_preorder() if child.kind in _NAMING_KINDS and child.spelling}
            for arm in arms
        ]
        tables = set()
        for parameters, body in self.expansion_bodies(node, bound):
            for table in _generic_tables(body, parameters, self):
                if len(table) == len(arms) and all(
                    identifiers <= names for (_, identifiers), names in zip(table, arm_names, strict=True)
                ):
                    tables.add(tuple(kind for kind, _ in table))
        if len(tables) != 1:
            return None
        names = list(next(iter(tables)))
        # An association may name a typedef (local_lock_t *), and the control's type may be spelled
        # with typeof (this_cpu_ptr): compare canonical types too.
        matches = {index for index, name in enumerate(names, 1) if {name, self.canonical_name(name)} & candidates}
        if len(matches) == 1:
            return matches
        if not matches and "default" in names:
            return {names.index("default") + 1}
        return None

    def canonical_name(self, name: str) -> str:
        """The canonical spelling of a type name as an association writes it: a typedef of the unit
        (``local_lock_t *``) is replaced by the type it names (``struct local_lock *``)."""
        stars = len(name) - len(name.rstrip("*"))
        base = name[: len(name) - stars].strip()
        if self.typedefs is None:
            self.typedefs = {
                cursor.spelling: cursor.underlying_typedef_type.get_canonical().spelling
                for cursor in self.tu.cursor.get_children()
                if cursor.kind == K.TYPEDEF_DECL and cursor.spelling
            }
        canonical = self.typedefs.get(base)
        return _normal_type(canonical + "*" * stars) if canonical is not None else name

    def ignored_parameters(self, callee: ci.Cursor | None) -> frozenset[int]:
        """The parameters a function of the unit never reads in code that is compiled, by position:
        not at all, only in code no control reaches, only in unevaluated operands, only in discarded
        values, or only as arguments its own callees ignore (from_kuid_munged passes its namespace
        only to from_kuid, which never reads it). A weak definition may be replaced, and a recursion
        or a chain past _EFFECT_DEPTH is not followed: none of these ignores a parameter."""
        if callee is None or callee.kind != K.FUNCTION_DECL:
            return frozenset()
        name = callee.spelling
        cached = self.reuse(self.ignored_cache, name)
        if cached is not None:
            return cached
        definition = self.evaluator.definitions.get(name)
        if definition is None or self.is_weak(definition) or not self.may_follow(name):
            return frozenset()
        start = (self.nesting, self.depth_cuts)
        self.nesting += 1
        try:
            walk = self.analysis(definition)
        finally:
            self.nesting -= 1
        never_read = walk.never_read()
        parameters = list(definition.get_arguments() or [])
        result = frozenset(position for position, parameter in enumerate(parameters) if parameter.hash in never_read)
        self.ignored_cache[name] = (result, self.horizon(start))
        return result

    def may_follow(self, name: str) -> bool:
        """A call may be followed into its callee: not a recursion, and not past _EFFECT_DEPTH. A cut
        at the depth bound is counted, so the answers above it are kept only for as deep a horizon."""
        if name in self.analyzing:
            return False
        if self.nesting >= _EFFECT_DEPTH:
            self.depth_cuts += 1
            return False
        return True

    def horizon(self, start: tuple[int, int]) -> int | None:
        """None when no cut at the depth bound happened since ``start``; otherwise the nesting at
        which the answer was computed: it holds wherever the analysis is asked at least that deep."""
        nesting, cuts = start
        return None if self.depth_cuts == cuts else nesting

    def reuse(self, cache: dict[str, tuple[_T, int | None]], name: str) -> _T | None:
        """A kept answer that holds at the current nesting. One computed under a cut is computed
        again when asked less deep, so each function is analyzed at most _EFFECT_DEPTH + 1 times;
        reusing it counts as the cut it rests on, so the answer that reuses it is bounded too."""
        entry = cache.get(name)
        if entry is None:
            return None
        value, horizon = entry
        if horizon is None:
            return value
        if self.nesting < horizon:
            return None
        self.depth_cuts += 1
        return value

    def ignores_variadic(self, callee: ci.Cursor | None) -> bool:
        """A variadic function of the unit that never reads its arguments past the named ones: no
        va_start in its body (include/linux/sysfs.h without CONFIG_SYSFS: ``static inline int
        sysfs_emit(char *buf, const char *fmt, ...) { return 0; }``). A weak definition may be
        replaced, so it reads them."""
        if callee is None or callee.kind != K.FUNCTION_DECL:
            return False
        definition = self.evaluator.definitions.get(callee.spelling)
        if definition is None or self.is_weak(definition):
            return False
        if definition.type.kind != TK.FUNCTIONPROTO or not definition.type.is_function_variadic():
            return False
        return not any(node.kind == K.CALL_EXPR and node.spelling in _VA_START for node in definition.walk_preorder())

    def branches_without_effect(self, statement: ci.Cursor, dead: frozenset[int] = frozenset()) -> bool:
        """An ``if`` whose branches have no effect and leave only by their end: only what its
        condition does is code (__device_attach's ``if (dev->parent) pm_runtime_get_sync(dev->parent)``
        without CONFIG_PM). A store to a variable in ``dead``, which nothing reads, is no effect, and
        neither is code control does not reach (clk_core_is_enabled's goto under
        ``!pm_runtime_active(dev)``, which gives true without CONFIG_PM)."""
        children = _children(statement)
        return (
            statement.kind == K.IF_STMT
            and len(children) >= 2
            and not _jumps_out(statement, 0, self.live_children)
            and all(self.effect_free(branch, dead) for branch in children[1:])
        )

    def live_children(self, node: ci.Cursor, children: list[ci.Cursor]) -> list[ci.Cursor]:
        """The children of ``node`` control reaches, as the walk decides them: the branch a constant
        condition selects, the left operand alone of ``&&`` or ``||`` where it decides, and a block's
        statements up to one that exits. A part that holds a label stays, as a goto may enter it."""
        kind = node.kind
        if kind == K.COMPOUND_STMT:
            for index, child in enumerate(children[:-1]):
                if child.kind in (*_EXITS, K.COMPOUND_STMT, K.IF_STMT, K.DO_STMT) and self.evaluator.exits(child):
                    rest = children[index + 1 :]
                    return children if any(_holds_label(part) for part in rest) else children[: index + 1]
            return children
        if len(children) < 2 or _mentions(children[0], _IMAGE_DECIDES):
            return children
        if kind in (K.IF_STMT, K.CONDITIONAL_OPERATOR):
            value = self.evaluator.evaluate(children[0], truth=True)
            if value is None:
                return children
            selected = 1 if value else 2
            kept = [child for index, child in enumerate(children) if index in (0, selected)]
            return children if any(_holds_label(child) for child in children if child not in kept) else kept
        if kind == K.BINARY_OPERATOR and libclang.binary_kind(node) in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
            left = self.evaluator.evaluate(children[0], truth=True)
            if left is not None and bool(left) == (libclang.binary_kind(node) == libclang.LOGICAL_OR):
                return children[:1]
        return children

    def loop_without_effect(self, loop: ci.Cursor, function: ci.Cursor, dead: frozenset[int]) -> bool:
        """A loop that has no effect: it may be assumed to end (not a constant controlling
        expression), control leaves it only by its end (no return or goto in a part it reaches, as
        __aggregate_find's ``list_for_each_entry(...) if (...) return m;``), and it stores only to
        locals nothing outside it reads. flush_all_rcu_sheaves' ``list_for_each_entry(s,
        &slab_caches, list) { if (!cache_has_sheaves(s)) continue; ... }`` with CONFIG_SLUB_TINY
        stores only its cursor; the compiler removes the whole loop."""
        if loop.kind not in _LOOPS or self.loop_may_not_end(loop) or _jumps_out(loop, 0, self.live_children):
            return False
        stored: set[int] = set()
        for node in _effect_nodes(loop, self.live_children):
            target = _store_target(node)
            variable = _stored_variable(target) if target is not None else None
            if variable is not None and _is_local(variable):
                stored.add(variable.hash)
        declared = {node.hash for node in _effect_nodes(loop) if node.kind == K.VAR_DECL and _is_local(node)}
        return self._effect_free(loop, stored | declared | dead, inlined=True) and not _referenced_outside(
            function, loop, stored - declared
        )

    def outcome(self, statements: list[ci.Cursor], dead: frozenset[int], falls_off: bool) -> tuple[int | None] | None:
        """What running ``statements`` gives, where they have no effect and end in a return: the
        returned constant, or (None,) for ``return;`` or for falling off the end of a function that
        returns nothing (``falls_off``). None where they do anything else."""
        if statements and statements[-1].kind == K.RETURN_STMT:
            body, last = statements[:-1], statements[-1]
        elif falls_off:
            body, last = statements, None
        else:
            return None
        if any(_jumps_out(part, 0, self.live_children) or not self.effect_free(part, dead) for part in body):
            return None
        value = _children(last) if last is not None else []
        if not value:
            return (None,)
        if not self.effect_free(value[0], dead):
            return None
        result = self.evaluator.evaluate(value[0])
        return None if result is None else (result,)

    def analysis(self, definition: ci.Cursor, returns_discarded: bool = False) -> _FunctionWalk:
        """The walk of a function whose labels and dead stores are settled.

        A label is code only when control falls into it or a goto that is code jumps to it
        (__slab_free's ``slab_empty:`` is reached only by a goto behind an exit), so the walk is
        repeated until the labels that live gotos target stop growing. Then a store to a local or
        a parameter nothing reads in compiled code (local_lock's ``__tl``) is not code either, and a
        store to one read only where the image decides (an argument a callee that is not inline
        ignores) is compiled only where the image has it; the walk is repeated until those
        variables stop growing. With ``returns_discarded``, the walk of a call whose value nothing
        uses: each return's value is discarded, so a local that only makes it is not computed."""
        name = definition.spelling
        analyses = self.unused_analyses if returns_discarded else self.analyses
        cached = self.reuse(analyses, name)
        if cached is not None:
            return cached
        self.analyzing.add(name)
        start = (self.nesting, self.depth_cuts)
        labels: frozenset[str] = frozenset()
        dead: frozenset[int] = frozenset()
        decided: frozenset[int] = frozenset()
        overwritten: frozenset[int] = frozenset()
        settled = False
        for _ in range(_ANALYSIS_ROUNDS):
            walk = _FunctionWalk(self, definition, labels, dead, decided, overwritten, returns_discarded)
            walk.walk(definition, None, 0, None)
            walk.finish()
            if not settled:
                if walk.goto_targets <= labels:
                    settled = True
                else:
                    labels |= walk.goto_targets
                    continue
            never_read, image_read = walk.never_read(), walk.read_only_where_the_image_decides()
            stored_again = walk.overwritten_stores()
            if never_read <= dead and image_read <= decided and stored_again <= overwritten:
                break
            dead |= never_read
            decided |= image_read - dead
            overwritten |= stored_again
        self.analyzing.discard(name)
        # A walk that met the depth bound is kept for as deep a horizon as it saw, so a function's
        # own facts (asked at nesting 0) come from a walk that saw its callees whole.
        analyses[name] = (walk, self.horizon(start))
        return walk

    def inline_function(self, name: str) -> bool:
        """The unit defines the function and declares it inline."""
        definition = self.evaluator.definitions.get(name)
        return definition is not None and _is_inline(definition)

    def effect_free(self, expression: ci.Cursor, dead: frozenset[int] = frozenset()) -> bool:
        """Computing ``expression`` changes nothing but its value, so the compiler removes it when
        the value is not used: no asm that is volatile or has no output, no volatile access, no store
        except to a variable the expression declares or one in ``dead`` (which nothing reads), no
        loop that may not end, and calls only to the builtins that compute a value and to functions
        of the unit that are themselves effect-free. Operands that are never evaluated (sizeof,
        typeof) do not count."""
        own = {node.hash for node in _effect_nodes(expression) if node.kind == K.VAR_DECL and _is_local(node)}
        return self._effect_free(expression, own | dead, inlined=True)

    def function_effect_free(self, name: str) -> bool:
        """A call of the function changes nothing but its result: its compiled code, as its settled
        walk tells (dead branches, code after an exit and unselected associations left out, such as
        the WARN_ON_ONCE under IS_ENABLED(CONFIG_DEBUG_ATOMIC) in instrument_atomic_check_alignment),
        has no effect. A function that is not inline and not static may not be inlined, and the
        compiler knows such a call returns only when the function has no loop. A function that
        calls itself again may not return, and a chain of calls deeper than _EFFECT_DEPTH is not
        followed: both count as an effect."""
        cached = self.reuse(self.effect_free_cache, name)
        if cached is not None:
            return cached
        definition = self.evaluator.definitions.get(name)
        if definition is None or self.is_weak(definition):
            self.effect_free_cache[name] = (False, None)
            return False
        if not self.may_follow(name):
            return False
        start = (self.nesting, self.depth_cuts)
        self.nesting += 1
        try:
            walk = self.analysis(definition)
        finally:
            self.nesting -= 1
        inlined = _is_inline(definition) or definition.linkage == ci.LinkageKind.INTERNAL
        result = walk.effects == 0 and (inlined or walk.compiled_loops == 0)
        self.effect_free_cache[name] = (result, self.horizon(start))
        return result

    def _effect_free(self, root: ci.Cursor, own: set[int], inlined: bool) -> bool:
        for node in _effect_nodes(root, self.live_children):
            kind = node.kind
            if kind == K.CALL_EXPR:
                callee = node.referenced
                if callee is None or callee.kind != K.FUNCTION_DECL:
                    return False
                if _is_overflow_builtin(callee.spelling):
                    arguments = list(node.get_arguments() or [])
                    if len(arguments) != 3 or not _address_of_local(arguments[2], own):
                        return False
                elif callee.spelling not in _VALUE_BUILTINS and not self.function_effect_free(callee.spelling):
                    return False
            elif (
                (kind == K.ASM_STMT and not self.asm_removable(node))
                or (kind in _LOOPS and (not inlined or self.loop_may_not_end(node)))
                or (kind.is_expression() and node.type.is_volatile_qualified())
            ):
                return False
            else:
                target = _store_target(node)
                if target is not None:
                    variable = _stored_variable(target)
                    if variable is None or variable.hash not in own:
                        return False
        return True

    def asm_removable(self, node: ci.Cursor) -> bool:
        """An asm statement that is not volatile, has no goto labels and has an output operand: it
        only computes its outputs, so the compiler removes it when nothing reads them (GCC manual,
        Extended Asm, Volatile). RELOC_HIDE under this_cpu_ptr is such an asm. The statement is read
        as written and from the macros it expands; where those hold several asm statements, every
        one must be removable."""
        found = [
            removable for _, body in self.expansion_bodies(node, node, limit=64) for removable in _asm_statements(body)
        ]
        return bool(found) and all(found)

    def loop_may_not_end(self, loop: ci.Cursor) -> bool:
        """A loop whose controlling expression is absent or a constant other than 0. Any other loop
        that has no effect may be assumed to end (C11 6.8.5p6): the kernel is built with
        -std=gnu11, and the compiler removes such a loop when nothing reads what it computes."""
        children = _children(loop)
        if loop.kind == K.WHILE_STMT:
            condition: ci.Cursor | None = children[0] if children else None
        elif loop.kind == K.DO_STMT:
            condition = children[-1] if len(children) == 2 else None
        else:
            return self.for_condition_constant(loop, children)
        value = libclang.constant_int(condition) if condition is not None else None
        return condition is None or (value is not None and value != 0)

    def for_condition_constant(self, loop: ci.Cursor, children: list[ci.Cursor]) -> bool:
        """``for (init; condition; step)`` may not end: its condition is absent or a constant other
        than 0. With all four parts libclang gives the condition second. Otherwise it leaves out the
        parts that are absent, so the condition is read from the tokens between the two semicolons
        of the header as written; a header a macro writes (list_for_each_entry) counts as one that
        may not end unless libclang gives all four parts."""
        if len(children) == 4:
            value = libclang.constant_int(children[1])
            return value is not None and value != 0
        tokens = self.written_tokens(loop, loop)
        if not tokens or tokens[0] != "for":
            return True
        depth, part, middle = 0, 0, []
        for token in tokens[1:]:
            if token in ("(", "[", "{"):
                depth += 1
            elif token in (")", "]", "}"):
                depth -= 1
                if depth == 0:
                    break
            if depth == 1 and token == ";":
                part += 1
            elif part == 1 and depth >= 1:
                middle.append(token)
        return not middle or (len(middle) == 1 and middle[0].isdigit() and int(middle[0]) != 0)

    def cleanup_function(self, node: ci.Cursor, bound: ci.Cursor) -> str | None:
        """The function a ``cleanup`` attribute on a local names, read from the declaration as
        written and from the macros it expands (``__free(x)``, ``LOCK_MOUNT(...)``). A declaration
        of several cleanup locals (scoped_seqlock_read's ``struct ss_tmp _s __cleanup(a) = ...,
        *ctx __cleanup(b) = ...``) names one per declarator, in order: the local's rank among them
        picks its own."""
        templates = self.vocabulary.cleanup_macros
        rank = self.cleanup_rank(node, bound)
        for parameters, body in self.expansion_bodies(node, bound, limit=32):
            found: list[str] = []
            for index, token in enumerate(body[:-3]):
                if body[index + 1] != "(" or body[index + 3] != ")" or not _IDENTIFIER.match(body[index + 2]):
                    continue
                argument = body[index + 2]
                if argument in parameters:
                    continue
                if token in ("cleanup", "__cleanup__"):
                    found.append(self.macro_alias(argument))
                elif token in templates:
                    found.append(self.macro_alias(templates[token].format(arg=argument)))
            if len(found) > rank:
                return found[rank]
        return None

    @staticmethod
    def cleanup_rank(node: ci.Cursor, bound: ci.Cursor) -> int:
        """The local's position among the cleanup locals its declaration statement declares."""
        for statement in bound.walk_preorder():
            if statement.kind != K.DECL_STMT:
                continue
            declared = [
                child.hash
                for child in statement.get_children()
                if child.kind == K.VAR_DECL and any(part.kind == K.UNEXPOSED_ATTR for part in child.get_children())
            ]
            if node.hash in declared:
                return declared.index(node.hash)
        return 0

    def macro_alias(self, name: str) -> str:
        """The function an object-like macro that is one identifier names (include/linux/path.h:
        ``#define __free_path_put path_put``), followed through further aliases."""
        seen = {name}
        while True:
            macro = self.macro(name)
            if macro is None or macro[0] or len(macro[1]) != 1 or not _IDENTIFIER.match(macro[1][0]):
                return name
            cursor = self.macros[name]
            if libclang.is_macro_function_like(cursor) or macro[1][0] in seen:
                return name
            name = macro[1][0]
            seen.add(name)


_IDENTIFIER = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_NAMING_KINDS = (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR, K.CALL_EXPR)
_C_WORDS = frozenset({
    "struct", "union", "enum", "const", "volatile", "unsigned", "signed", "char", "short", "int", "long", "void",
    "float", "double", "_Bool", "bool", "sizeof", "typeof", "__typeof__", "default", "NULL",
    # Statement keywords, which a statement expression in an association may hold (WRITE_ONCE's do
    # ... while (0) written out).
    "do", "while", "for", "if", "else", "switch", "case", "break", "continue", "return", "goto",
    "static", "register", "auto", "extern", "inline", "__inline__", "asm", "__asm__", "__volatile__",
})  # fmt: skip


def _split_top(tokens: list[str], separator: str) -> list[list[str]]:
    parts: list[list[str]] = [[]]
    depth = 0
    for token in tokens:
        if token in "([{":
            depth += 1
        elif token in ")]}":
            depth -= 1
        if token == separator and depth == 0:
            parts.append([])
        else:
            parts[-1].append(token)
    return parts


def _generic_tables(
    body: list[str], parameters: frozenset[str], extractor: _Extractor
) -> list[list[tuple[str, frozenset[str]]]]:
    """Each ``_Generic(...)`` in a token list: per association, its type and the identifiers of its
    expression that are neither parameters, C words nor macros."""
    tables = []
    for start, token in enumerate(body):
        if token != "_Generic" or body[start + 1 : start + 2] != ["("]:
            continue
        depth, end = 0, start + 1
        for end in range(start + 1, len(body)):
            depth += {"(": 1, ")": -1}.get(body[end], 0)
            if depth == 0:
                break
        parts = _split_top(body[start + 2 : end], ",")
        table: list[tuple[str, frozenset[str]]] = []
        for arm in parts[1:]:
            colon, pending = None, 0
            nesting = 0
            for index, item in enumerate(arm):
                nesting += {"(": 1, "[": 1, "{": 1, ")": -1, "]": -1, "}": -1}.get(item, 0)
                if nesting == 0 and item == "?":
                    pending += 1
                elif nesting == 0 and item == ":":
                    if pending:
                        pending -= 1
                    else:
                        colon = index
                        break
            if colon is None:
                table = []
                break
            expression = arm[colon + 1 :]
            identifiers = frozenset(
                item for position, item in enumerate(expression)
                if _IDENTIFIER.match(item) and item not in parameters and item not in _C_WORDS
                and (position == 0 or expression[position - 1] not in ("struct", "union", "enum"))
                and extractor.macro(item) is None
            )  # fmt: skip
            table.append((_normal_type(" ".join(arm[:colon])), identifiers))
        if table:
            tables.append(table)
    return tables


def _normal_type(text: str) -> str:
    text = re.sub(r"\s+", " ", text).strip()
    return re.sub(r"\s*\*\s*", "*", text)


def _type_selection(node: ci.Cursor) -> set[int] | None:
    """The one association whose type is the ``_Generic``'s own, or None when none or several are."""
    if node.type.kind == TK.INVALID:
        return None
    wanted = node.type.get_canonical().spelling
    matches = {
        index
        for index, association in enumerate(_children(node)[1:], 1)
        if association.type.get_canonical().spelling == wanted
    }
    return matches if len(matches) == 1 else None


# Builtins whose answer the compiler gives only while it optimizes: the image decides the branch.
_IMAGE_DECIDES = frozenset({"__builtin_constant_p", "__builtin_object_size", "__builtin_dynamic_object_size"})
_UNREACHABLE = frozenset({"__builtin_unreachable", "__builtin_trap"})
_ENTRIES = (K.LABEL_STMT, K.CASE_STMT, K.DEFAULT_STMT)
_EXITS = (K.RETURN_STMT, K.GOTO_STMT, K.BREAK_STMT, K.CONTINUE_STMT)
_LOOP_EXITS = (K.BREAK_STMT, K.CONTINUE_STMT)
_BRANCHES = (K.IF_STMT, K.CONDITIONAL_OPERATOR, K.SWITCH_STMT, K.WHILE_STMT, K.FOR_STMT, K.DO_STMT, K.GOTO_STMT)
# CXBinaryOperatorKind Comma.
_COMMA = 33
# CXBinaryOperatorKind Assign: ``v = e``, which stores without reading v.
_ASSIGN = 22
# CXBinaryOperatorKind Mul and And: with an operand 0, the result is 0 whatever the other one is.
_ZEROING = (3, 17)
# CXBinaryOperatorKind EQ and NE.
_EQUALITY = (15, 16)


def _is_plain_assignment(node: ci.Cursor) -> bool:
    return node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == _ASSIGN


def _is_static_local(variable: ci.Cursor) -> bool:
    """A variable a function declares static: the compiler lays it out as data."""
    parent = variable.semantic_parent
    return variable.storage_class == ci.StorageClass.STATIC and parent is not None and parent.kind != K.TRANSLATION_UNIT


def _holds_label(node: ci.Cursor) -> bool:
    """A label or a case inside, where control may enter other than by the start."""
    return any(part.kind in _ENTRIES for part in node.walk_preorder())


def _function_body(definition: ci.Cursor) -> ci.Cursor | None:
    return next((child for child in _children(definition) if child.kind == K.COMPOUND_STMT), None)


def _referenced_outside(root: ci.Cursor, inside: ci.Cursor, variables: set[int]) -> bool:
    """A reference to one of ``variables`` in ``root`` that is not in its subtree ``inside``."""
    pending = [root]
    while pending:
        node = pending.pop()
        if node.hash == inside.hash:
            continue
        if node.kind == K.DECL_REF_EXPR and node.referenced is not None and node.referenced.hash in variables:
            return True
        pending.extend(_children(node))
    return False


def _mentions(cursor: ci.Cursor, names: frozenset[str]) -> bool:
    return any(
        node.kind in (K.CALL_EXPR, K.DECL_REF_EXPR) and node.spelling in names for node in cursor.walk_preorder()
    )


def _is_inline(definition: ci.Cursor) -> bool:
    """A definition declared ``inline`` (``static inline``, ``__always_inline``, or by a macro that
    writes the definition, as DECLARE_TRACE's stubs)."""
    return libclang.is_function_inlined(definition)


def _is_noreturn(function: ci.Cursor | None) -> bool:
    """A function declared ``__noreturn`` (``__attribute__((noreturn))``: panic, do_exit)."""
    return function is not None and function.kind == K.FUNCTION_DECL and "noreturn" in function.type.spelling


def _case_values(statement: ci.Cursor) -> set[int | None] | None:
    """The values of a chain of labels (``case 1: case 2: stmt``; None for ``default``), or None
    when the statement is not a label of the switch."""
    if statement.kind not in (K.CASE_STMT, K.DEFAULT_STMT):
        return None
    values: set[int | None] = set()
    node: ci.Cursor | None = statement
    while node is not None and node.kind in (K.CASE_STMT, K.DEFAULT_STMT):
        parts = _children(node)
        if node.kind == K.DEFAULT_STMT:
            values.add(None)
        else:
            values.add(libclang.constant_int(parts[0]) if parts else None)
        node = parts[-1] if parts else None
    return values


def _switch_labels_top_level(body: ci.Cursor) -> bool:
    """Every label of the switch heads a statement of its body (no case range, no label inside a
    nested statement as in Duff's device), so the cases can be followed statement by statement."""
    heads = 0
    for statement in _children(body):
        node: ci.Cursor | None = statement
        while node is not None and node.kind in (K.CASE_STMT, K.DEFAULT_STMT):
            parts = _children(node)
            if node.kind == K.CASE_STMT and (len(parts) != 2 or libclang.constant_int(parts[0]) is None):
                return False
            heads += 1
            node = parts[-1] if parts else None
    stack, total = list(_children(body)), 0
    while stack:
        node = stack.pop()
        if node.kind == K.SWITCH_STMT:
            continue
        if node.kind in (K.CASE_STMT, K.DEFAULT_STMT):
            total += 1
        stack.extend(_children(node))
    return heads == total


def _branches_on(node: ci.Cursor) -> bool:
    return node.kind in _BRANCHES or (
        node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) in (libclang.LOGICAL_AND, libclang.LOGICAL_OR)
    )


def _exits_under_builtin(statement: ci.Cursor, evaluator: _Evaluator) -> bool:
    """An ``if`` whose condition only the compiler decides (``__builtin_constant_p``) and whose
    branch leaves the block: what follows was compiled only when the condition was false."""
    if statement.kind != K.IF_STMT:
        return False
    parts = _children(statement)
    return len(parts) >= 2 and _mentions(parts[0], _IMAGE_DECIDES) and evaluator.exits(parts[1])


# Builtins that only compute a value from their operands (the return and frame addresses from the
# frame: kmalloc_nolock_noprof's _RET_IP_).
_VALUE_BUILTINS = frozenset(
    {
        "__builtin_expect", "__builtin_expect_with_probability", "__builtin_constant_p",
        "__builtin_object_size", "__builtin_dynamic_object_size", "__builtin_choose_expr",
        "__builtin_clz", "__builtin_clzl", "__builtin_clzll", "__builtin_ctz", "__builtin_ctzl",
        "__builtin_ctzll", "__builtin_ffs", "__builtin_ffsl", "__builtin_ffsll", "__builtin_popcount",
        "__builtin_popcountl", "__builtin_popcountll", "__builtin_bswap16", "__builtin_bswap32",
        "__builtin_bswap64", "__builtin_return_address", "__builtin_frame_address",
    }
)  # fmt: skip
# Walks of one function until its labels and dead stores settle, and analyses nested one inside
# another (a call followed into its callee to tell its effects or the parameters it ignores).
_ANALYSIS_ROUNDS = 8
_EFFECT_DEPTH = 16
_RECURSION_LIMIT = 100_000
# What va_start expands to: a function that calls neither never reads its variable arguments.
_VA_START = frozenset({"__builtin_va_start", "__builtin_c23_va_start", "__builtin_stdarg_start"})
# The statements whose children are statements: an expression there is an expression statement.
_STATEMENT_PARENTS = (K.COMPOUND_STMT, K.LABEL_STMT, K.CASE_STMT, K.DEFAULT_STMT)


def _is_local(variable: ci.Cursor | None) -> bool:
    """A parameter, or a variable of a function that is not static: a copy the function owns."""
    if variable is None:
        return False
    if variable.kind == K.PARM_DECL:
        return True
    return (
        variable.kind == K.VAR_DECL
        and not _is_global_var(variable)
        and variable.storage_class not in (ci.StorageClass.STATIC, ci.StorageClass.EXTERN)
    )


_OVERFLOW_BUILTIN = re.compile(r"__builtin_[su]?(add|sub|mul)(l|ll)?_overflow")


def _is_overflow_builtin(name: str) -> bool:
    """``__builtin_mul_overflow(a, b, &r)`` and its typed forms: a value, and a store into ``*r``."""
    return _OVERFLOW_BUILTIN.fullmatch(name) is not None


def _address_of_local(expression: ci.Cursor, own: set[int] | None) -> bool:
    """``&v`` (or ``&v.m``, ``&v[i]``) of a local: of one in ``own``, or, without ``own``, of any
    variable the function itself owns (a parameter, or a local that is not static)."""
    node = _strip(expression)
    if node.kind != K.UNARY_OPERATOR or not libclang.is_address_of(node) or not _children(node):
        return False
    variable = _stored_variable(_children(node)[0])
    if variable is None:
        return False
    return variable.hash in own if own is not None else _is_local(variable)


_LOOP_OR_SWITCH = (K.WHILE_STMT, K.FOR_STMT, K.DO_STMT, K.SWITCH_STMT)
_LEAVES = (K.RETURN_STMT, K.GOTO_STMT, K.INDIRECT_GOTO_STMT, K.LABEL_STMT, K.CASE_STMT, K.DEFAULT_STMT)


def _statement_position(parent: ci.Cursor, index: int) -> bool:
    """The child at ``index`` of ``parent`` is a statement: one of a block, a labeled statement, an
    if's branch, or a loop's body."""
    kind = parent.kind
    if kind in _STATEMENT_PARENTS:
        return True
    if kind == K.IF_STMT:
        return index >= 1
    if kind == K.WHILE_STMT:
        return index == 1
    if kind == K.DO_STMT:
        return index == 0
    if kind == K.FOR_STMT:
        return index == len(_children(parent)) - 1
    return False


# The parts of a node control reaches, as the evaluator decides them (_Extractor.live_children).
_Live = Callable[[ci.Cursor, list[ci.Cursor]], list[ci.Cursor]]


def _jumps_out(node: ci.Cursor, enclosing: int, live: _Live | None = None) -> bool:
    """Control may leave ``node`` other than by its end, or enter it other than by its start: a
    return, a goto, a label, a case, or a break or continue that no loop or switch inside it takes.
    With ``live``, only the parts control reaches count (clk_core_is_enabled's goto under
    ``!pm_runtime_active(dev)``, a stub that gives true)."""
    kind = node.kind
    if kind in _LEAVES or (kind in (K.BREAK_STMT, K.CONTINUE_STMT) and enclosing == 0):
        return True
    inner = enclosing + 1 if kind in _LOOP_OR_SWITCH else enclosing
    children = _children(node)
    if live is not None:
        children = live(node, children)
    return any(_jumps_out(child, inner, live) for child in children)


def _stored_variable(target: ci.Cursor) -> ci.Cursor | None:
    """The variable an assignment stores into: ``v``, ``v.m`` or ``v[i]`` of a variable ``v`` (not
    through a pointer), or None. A parameter of array type is a pointer (fast_mix's
    ``unsigned long s[4]``), so ``s[i]`` stores through it."""
    node = _strip_parens(target)
    selected = False
    while True:
        children = _children(node)
        if node.kind == K.DECL_REF_EXPR:
            variable = node.referenced
            if (
                selected
                and variable is not None
                and variable.kind == K.PARM_DECL
                and variable.type.get_canonical().kind in _ARRAY_TYPES
            ):
                return None
            return variable
        if node.kind in (K.MEMBER_REF_EXPR, K.ARRAY_SUBSCRIPT_EXPR) and children:
            base = children[0]
            if base.type.get_canonical().kind == TK.POINTER:
                return None
            selected = True
            node = _strip_parens(base)
        elif node.kind == K.UNEXPOSED_EXPR and len(children) == 1:
            node = children[0]
        else:
            return None


def _effect_nodes(root: ci.Cursor, live: _Live | None = None) -> Iterator[ci.Cursor]:
    """The nodes of a function or an expression in preorder, without the operands that are never
    evaluated: sizeof and alignof operands, the operands of __builtin_constant_p, cast and
    compound-literal types, and the types of declarations (typeof). With ``live``, without the
    parts control does not reach either."""
    pending = [root]
    while pending:
        node = pending.pop()
        yield node
        children = _children(node)
        kind = node.kind
        if kind == K.CXX_UNARY_EXPR or (kind == K.CALL_EXPR and node.spelling in _IMAGE_DECIDES):
            continue
        if kind == K.VAR_DECL:
            initializer = libclang.var_initializer(node)
            children = [child for child in children if initializer is not None and child == initializer]
        elif kind in (K.CSTYLE_CAST_EXPR, K.COMPOUND_LITERAL_EXPR):
            children = children[-1:]
        elif live is not None:
            children = live(node, children)
        pending.extend(reversed(children))


_ASM_KEYWORDS = frozenset({"asm", "__asm", "__asm__"})
_ASM_QUALIFIERS = frozenset({"volatile", "__volatile__", "__volatile", "goto", "inline", "__inline__", "__inline"})
_ASM_KEEPS = frozenset({"volatile", "__volatile__", "__volatile", "goto"})


def _asm_statements(tokens: list[str]) -> Iterator[bool]:
    """For each asm statement in a token list, whether it only computes its outputs: no volatile or
    goto qualifier, and a non-empty output list (the operands between the first and the second
    ``:`` at the top level of its parentheses)."""
    for start, token in enumerate(tokens):
        if token not in _ASM_KEYWORDS:
            continue
        index, kept = start + 1, False
        while index < len(tokens) and tokens[index] in _ASM_QUALIFIERS:
            kept = kept or tokens[index] in _ASM_KEEPS
            index += 1
        if index >= len(tokens) or tokens[index] != "(":
            yield False
            continue
        depth, colons, outputs = 0, 0, 0
        for spelling in tokens[index:]:
            if spelling in ("(", "[", "{"):
                depth += 1
            elif spelling in (")", "]", "}"):
                depth -= 1
                if depth == 0:
                    break
            elif depth == 1 and spelling in (":", "::"):
                colons += len(spelling)
                if colons >= 2:
                    break
                continue
            if colons == 1 and depth >= 1:
                outputs += 1
        yield not kept and colons >= 1 and outputs > 0


def _store_target(node: ci.Cursor) -> ci.Cursor | None:
    """The expression an assignment, a compound assignment or an increment stores into."""
    children = _children(node)
    if not children:
        return None
    if node.kind == K.COMPOUND_ASSIGNMENT_OPERATOR or (node.kind == K.BINARY_OPERATOR and libclang.is_assignment(node)):
        return children[0]
    if node.kind == K.UNARY_OPERATOR and libclang.is_increment(node):
        return children[0]
    return None


_INTEGER_TYPES = {
    TK.CHAR_U: False, TK.UCHAR: False, TK.CHAR16: False, TK.CHAR32: False, TK.USHORT: False,
    TK.UINT: False, TK.ULONG: False, TK.ULONGLONG: False, TK.UINT128: False, TK.CHAR_S: True,
    TK.SCHAR: True, TK.WCHAR: True, TK.SHORT: True, TK.INT: True, TK.LONG: True, TK.LONGLONG: True,
    TK.INT128: True,
}  # fmt: skip
# CXBinaryOperatorKind values the guard expressions express, and CXUnaryOperatorKind ones.
_BINARY_NAMES = {
    3: "*", 4: "/", 5: "%", 6: "+", 7: "-", 8: "<<", 9: ">>", 11: "<", 12: ">", 13: "<=", 14: ">=",
    15: "==", 16: "!=", 17: "&", 18: "^", 19: "|", 20: "&&", 21: "||",
}  # fmt: skip
_UNARY_NAMES = {8: "neg", 9: "~", 10: "!"}


def _integer_type(typ: ci.Type) -> dict[str, Any] | None:
    """An integer or pointer type as guard expressions state it: its width in bits ``w``, whether it is
    signed ``s``, and ``b`` for _Bool (any non-zero value converts to 1)."""
    canonical = typ.get_canonical()
    kind = canonical.kind
    if kind == TK.ENUM:
        declaration = canonical.get_declaration()
        canonical = declaration.enum_type.get_canonical() if declaration is not None else canonical
        kind = canonical.kind
    size = canonical.get_size()
    if kind == TK.BOOL:
        return {"w": 8, "s": False, "b": True}
    if kind == TK.POINTER:
        return {"w": size * 8, "s": False} if size > 0 else None
    if kind in _INTEGER_TYPES and size > 0:
        return {"w": size * 8, "s": _INTEGER_TYPES[kind]}
    return None


def _is_record(typ: ci.Type) -> bool:
    return bool(typ.get_canonical().kind == TK.RECORD)


def _is_struct(expression: dict[str, Any]) -> bool:
    """An expression of a struct value (expressions: a parameter passed by value, a member that is a
    struct, a constant struct), which carries no integer type."""
    return "struct" in expression or ("w" not in expression and ("p" in expression or expression.get("op") == "."))


# CXBinaryOperatorKind AndAssign, XorAssign, OrAssign: a variable they change holds a few values.
_COMBINING = {30: "&", 31: "^", 32: "|"}
# A variable changed by more combining assignments than this holds too many values to state.
_COMBINED_LIMIT = 3
# The statements a template follows, and the nodes of the expression it gives, at most.
_FLOW_STEPS = 256
_TEMPLATE_NODES = 256
# The nesting of an expression the facts state, at most: IS_ERR's
# __builtin_expect(!!((unsigned long)(void *)(ptr) >= (unsigned long)-MAX_ERRNO), 0) is 12 deep.
_EXPRESSION_DEPTH = 48
# A constant struct with more scalar members than this is not stated (expressions: ``{"struct": …}``).
_STRUCT_MEMBERS = 64
# A template refuses a label or a jump: the returns then take another shape.
_JUMPS = (K.LABEL_STMT, K.GOTO_STMT, K.INDIRECT_GOTO_STMT)
# Statements that may leave a statement around them for code elsewhere in the function.
_TRANSFERS = (K.RETURN_STMT, K.BREAK_STMT, K.CONTINUE_STMT)
# Expressions whose parts may each hold several values that still decide them (``chosen``).
_CHOOSING = (K.BINARY_OPERATOR, K.UNARY_OPERATOR, K.CALL_EXPR, K.CONDITIONAL_OPERATOR, K.DECL_REF_EXPR)
# A template's value where no path returns (each calls a noreturn function).
_NEVER: Any = object()


@dataclass(frozen=True)
class _Writes:
    """How a function writes its variables, by each one's hash: the values plain assignments store,
    the ``|=``, ``&=`` and ``^=`` it makes (the operator and the value), and the variables it changes
    otherwise (``+=``, ``++``) or whose address it takes."""

    stores: dict[int, list[ci.Cursor]]
    combined: dict[int, list[tuple[str, ci.Cursor]]]
    opaque: frozenset[int]

    def changed(self, key: int) -> bool:
        """Written other than by plain assignments."""
        return key in self.opaque or key in self.combined


@dataclass(frozen=True)
class _Resume:
    """Among the statements a template follows: where a ``break`` continues again once a switch ends."""

    after_break: list[Any] | None


def _size(expression: dict[str, Any] | None) -> int:
    if expression is None or "a" not in expression:
        return 1
    return 1 + sum(_size(operand) for operand in expression["a"])


def _choose(test: dict[str, Any] | None, taken: Any, skipped: Any) -> Any:
    """The value of ``test ? taken : skipped`` where either may never be given (``_NEVER``)."""
    if taken is None or skipped is None:
        return None
    if taken is _NEVER:
        return skipped
    if skipped is _NEVER:
        return taken
    return {"op": "?:", "a": [test, taken, skipped]}


# How a guard expression states a variable at a reference: (the variable's declaration, the reference,
# the depth) to the values it may hold there, or None.
_Resolve = Callable[[ci.Cursor, ci.Cursor, int], "dict[str, Any] | None"]


class _Evaluator:
    """What the compiler folds that clang's evaluator alone does not: inline helpers that return one
    value (or one truth value, where only the truth is tested), locals that keep their initializer,
    ``x & 0`` and ``x * 0``, ``__builtin_expect``."""

    _OPERATIONS: ClassVar[dict[int, Any]] = {
        3: lambda a, b: a * b, 6: lambda a, b: a + b, 7: lambda a, b: a - b,
        8: lambda a, b: a << b if 0 <= b < 64 else None, 9: lambda a, b: a >> b if 0 <= b < 64 else None,
        11: lambda a, b: int(a < b), 12: lambda a, b: int(a > b), 13: lambda a, b: int(a <= b),
        14: lambda a, b: int(a >= b), 15: lambda a, b: int(a == b), 16: lambda a, b: int(a != b),
        17: lambda a, b: a & b, 18: lambda a, b: a ^ b, 19: lambda a, b: a | b,
    }  # fmt: skip

    def __init__(self, definitions: dict[str, ci.Cursor], weak: Callable[[ci.Cursor], bool] = lambda _: False) -> None:
        self.definitions = definitions
        # A weak definition's answer is not the program's: another unit may replace it.
        self.weak = weak
        self.helpers: dict[str, tuple[int | None, int | None]] = {}
        self.locals: dict[int, int | None] = {}
        self.values: dict[tuple[int, bool], int | None] = {}
        self.deciding: dict[int, bool] = {}
        self.written: dict[int, _Writes] = {}
        self.templates: dict[str, dict[str, Any] | None] = {}
        self.choices: dict[tuple[int, bool], int | None] = {}
        self.transferring: dict[int, bool] = {}
        self.structs: dict[int, dict[str, Any] | None] = {}  # each const global struct's value, by its hash
        # Variables as the unit's own values: a local as the values stored in it, a parameter unknown.
        self.known = self.resolver({})

    def image_decides(self, cursor: ci.Cursor) -> bool:
        """``__builtin_constant_p`` or ``__builtin_object_size`` inside: the image decides. Once per cursor."""
        key = cursor.hash
        if key not in self.deciding:
            self.deciding[key] = _mentions(cursor, _IMAGE_DECIDES)
        return self.deciding[key]

    def evaluate(self, cursor: ci.Cursor, depth: int = 0, truth: bool = False) -> int | None:
        """The value clang or this evaluator folds the expression to. Where ``truth``, only the truth
        of the value is tested (a condition, an operand of ``!``, ``&&`` or ``||``), and an inline
        helper whose returns differ but agree in truth gives that truth value."""
        if depth == 0:
            # Guards and argument values evaluate each subexpression again: once per cursor.
            key = (cursor.hash, truth)
            if key not in self.values:
                self.values[key] = self.typed_value(cursor, truth)
            return self.values[key]
        return self.typed_value(cursor, truth, depth)

    def typed_value(self, cursor: ci.Cursor, truth: bool, depth: int = 0) -> int | None:
        """The value as the expression's type holds it: an unsigned type has no negative value, and
        each width wraps (libclang gives a constant as a signed long long)."""
        value = self.evaluate_uncached(cursor, truth, depth)
        typed = _integer_type(cursor.type) if value is not None and cursor.kind.is_expression() else None
        if value is None or typed is None:
            return value
        if typed.get("b"):
            return int(value != 0)
        width = int(typed["w"])
        value &= (1 << width) - 1
        return value - (1 << width) if typed["s"] and value >= 1 << (width - 1) else value

    def evaluate_uncached(self, cursor: ci.Cursor, truth: bool, depth: int = 0) -> int | None:
        if self.image_decides(cursor):
            return None
        value = libclang.constant_int(cursor)
        if value is not None or depth > 6:
            return value
        value = self.folded(cursor, truth, depth)
        if value is None and cursor.kind in _CHOOSING:
            return self.chosen(cursor, truth)
        return value

    def folded(self, cursor: ci.Cursor, truth: bool, depth: int) -> int | None:
        """What the expression folds to from the one value of each of its parts."""
        kind, inner = cursor.kind, _children(cursor)
        if kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR, K.CSTYLE_CAST_EXPR) and inner:
            operand = _operand(cursor)
            return None if operand is None else self.evaluate(operand, depth, truth)
        if kind == K.UNARY_OPERATOR and inner and libclang.unary_kind(cursor) == libclang.LOGICAL_NOT:
            value = self.evaluate(inner[0], depth, truth=True)
            return None if value is None else int(not value)
        if kind == K.CONDITIONAL_OPERATOR and len(inner) == 3:
            condition = self.evaluate(inner[0], depth, truth=True)
            return None if condition is None else self.evaluate(inner[1] if condition else inner[2], depth, truth)
        if kind == K.BINARY_OPERATOR and len(inner) == 2:
            operator = libclang.binary_kind(cursor)
            if operator == _COMMA:
                # cpu_to_node(cpu) without NUMA: ((void)(cpu), 0).
                return self.evaluate(inner[1], depth, truth)
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
                left = self.evaluate(inner[0], depth, truth=True)
                if left is not None and bool(left) == (operator == libclang.LOGICAL_OR):
                    return int(bool(left))
                right = self.evaluate(inner[1], depth, truth=True)
                if right is not None and bool(right) == (operator == libclang.LOGICAL_OR):
                    # oom's `cur() && sysctl_oom_kill_allocating_task && ...`: the right side decides,
                    # whatever the left gives (its calls stay code; the walk keeps them).
                    return int(bool(right))
                return None if left is None or right is None else int(bool(right))
            left = self.evaluate(inner[0], depth)
            right = self.evaluate(inner[1], depth)
            if operator in (3, 17) and (left == 0 or right == 0):
                return 0
            operation = self._OPERATIONS.get(operator)
            return None if left is None or right is None or operation is None else operation(left, right)
        if kind == K.DECL_REF_EXPR and cursor.referenced is not None and cursor.referenced.kind == K.VAR_DECL:
            parent = cursor.referenced.semantic_parent
            if parent is not None and parent.kind == K.FUNCTION_DECL:
                return self.local(cursor.referenced, depth)
            return self.static_value(cursor.referenced, depth)
        if kind == K.CALL_EXPR and cursor.referenced is not None:
            if cursor.referenced.spelling == "__builtin_expect":
                arguments = list(cursor.get_arguments() or [])
                return self.evaluate(arguments[0], depth, truth) if arguments else None
            if cursor.referenced.kind == K.FUNCTION_DECL:
                exact, truth_value = self.helper(cursor.referenced.spelling, depth)
                return exact if exact is not None or not truth else truth_value
        return None

    def helper(self, name: str, depth: int) -> tuple[int | None, int | None]:
        """What an inline helper always returns: the value, when every return control can reach gives
        the same one, and the truth value, when every one agrees in truth. A return after one the
        helper always takes (``if (IS_ENABLED(CONFIG_SLUB_TINY) || …) return 0;``) is not reached."""
        if name in self.helpers:
            return self.helpers[name]
        definition = self.definitions.get(name)
        if depth > 3:
            # Too deep to fold here; a shallower use may still fold it, so nothing is kept.
            return (None, None)
        self.helpers[name] = (None, None)
        if definition is None or self.weak(definition):
            return (None, None)
        body = next((child for child in _children(definition) if child.kind == K.COMPOUND_STMT), None)
        returns = self.live_returns(body) if body is not None else []
        if not returns:
            return (None, None)
        values: set[int | None] = set()
        truths: set[int | None] = set()
        for node in returns:
            inner = _children(node)
            if not inner:
                return (None, None)
            values.add(self.evaluate(inner[0], depth + 1))
            truth = self.evaluate(inner[0], depth + 1, truth=True)
            truths.add(None if truth is None else int(bool(truth)))
        exact = values.pop() if len(values) == 1 else None
        truth_value = truths.pop() if len(truths) == 1 else None
        self.helpers[name] = (exact, truth_value)
        return self.helpers[name]

    def live_returns(self, statement: ci.Cursor) -> list[ci.Cursor]:
        """The return statements control can reach in a statement: none in an if's branch a constant
        condition rules out, or after a statement that exits (until a label or a case)."""
        kind = statement.kind
        if kind == K.RETURN_STMT:
            return [statement]
        inner = _children(statement)
        if kind == K.COMPOUND_STMT:
            found: list[ci.Cursor] = []
            dead = False
            for child in inner:
                if child.kind in _ENTRIES:
                    dead = False
                if not dead:
                    found += self.live_returns(child)
                    dead = self.exits(child)
            return found
        if kind == K.IF_STMT and len(inner) >= 2:
            value = self.evaluate(inner[0], truth=True)
            if value is not None:
                branch = inner[1] if value else (inner[2] if len(inner) > 2 else None)
                return self.live_returns(branch) if branch is not None else []
        if kind.is_statement():
            return [node for child in inner for node in self.live_returns(child)]
        # Inside an expression only a statement expression (``({ … })``) holds a return.
        return [node for node in statement.walk_preorder() if node.kind == K.RETURN_STMT]

    def static_value(self, variable: ci.Cursor, depth: int) -> int | None:
        """A file-scope static scalar's value where nothing in the unit writes it or takes its address:
        its initializer, or 0 (mm/oom_kill.c's sysctl_oom_kill_allocating_task without CONFIG_SYSCTL)."""
        if variable.storage_class != ci.StorageClass.STATIC or variable.type.is_volatile_qualified():
            return None
        if variable.type.get_canonical().kind not in _SCALAR_KINDS:
            return None
        # A static declared before its definition (kernel/rcu/tree.h's uses of tree.c's statics): the
        # canonical declaration names it, and the definition holds its initializer.
        writes = self.stores_in(variable.translation_unit.cursor)
        if writes.changed(variable.canonical.hash) or writes.stores.get(variable.canonical.hash):
            return None
        initializer = libclang.var_initializer(variable.get_definition() or variable)
        return self.evaluate(initializer, depth + 1) if initializer is not None else 0

    def kept_initializer(self, variable: ci.Cursor) -> ci.Cursor | None:
        """A local's initializer when nothing writes the local or takes its address."""
        initializer = libclang.var_initializer(variable)
        function = variable.semantic_parent
        if initializer is None or function is None:
            return None
        writes = self.stores_in(function)
        return None if writes.changed(variable.hash) or writes.stores.get(variable.hash) else initializer

    def stores_in(self, function: ci.Cursor) -> _Writes:
        """How a function writes its variables (see ``_Writes``)."""
        key = function.hash
        if key not in self.written:
            stores: dict[int, list[ci.Cursor]] = collections.defaultdict(list)
            combined: dict[int, list[tuple[str, ci.Cursor]]] = collections.defaultdict(list)
            opaque: set[int] = set()
            for node in function.walk_preorder():
                if node.kind not in (K.BINARY_OPERATOR, K.COMPOUND_ASSIGNMENT_OPERATOR, K.UNARY_OPERATOR):
                    continue
                operands = _children(node)
                target = operands[0] if operands else None
                while target is not None and target.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR) and _children(target):
                    target = _children(target)[0]
                if target is None or target.kind != K.DECL_REF_EXPR or target.referenced is None:
                    continue
                variables = {target.referenced.hash, target.referenced.canonical.hash}
                if node.kind == K.BINARY_OPERATOR and libclang.is_assignment(node) and len(operands) == 2:
                    # A file-scope static declared before its definition is one variable under both.
                    for variable in variables:
                        stores[variable].append(operands[1])
                elif node.kind == K.COMPOUND_ASSIGNMENT_OPERATOR:
                    operator = _COMBINING.get(libclang.binary_kind(node)) if len(operands) == 2 else None
                    for variable in variables:
                        if operator is None:
                            opaque.add(variable)
                        else:
                            combined[variable].append((operator, operands[1]))
                elif node.kind == K.UNARY_OPERATOR and (libclang.is_increment(node) or libclang.is_address_of(node)):
                    opaque.update(variables)
            self.written[key] = _Writes(dict(stores), dict(combined), frozenset(opaque))
        return self.written[key]

    def local(self, variable: ci.Cursor, depth: int) -> int | None:
        """A local's value when every value stored in it (its initializer and each plain assignment)
        evaluates to one value, and nothing else writes it or takes its address. A read before every
        store reads an indeterminate value, so the stores decide."""
        key = variable.hash
        if key in self.locals:
            return self.locals[key]
        self.locals[key] = None
        function = variable.semantic_parent
        if function is None:
            return None
        writes = self.stores_in(function)
        if writes.changed(key):
            return None
        stores = writes.stores
        initializer = libclang.var_initializer(variable)
        if stores.get(key) and variable.storage_class in (ci.StorageClass.STATIC, ci.StorageClass.EXTERN):
            # A static local keeps its value between calls: its first value is any earlier store.
            return None
        sources = ([initializer] if initializer is not None else []) + stores.get(key, [])
        values = {self.evaluate(source, depth + 1) for source in sources}
        value = values.pop() if len(values) == 1 else None
        self.locals[key] = value
        return value

    def expression(
        self, cursor: ci.Cursor, resolve: _Resolve, depth: int = 0, truth: bool = False, fold: bool = True
    ) -> dict[str, Any] | None:
        """The expression as link evaluates it at a fold (expressions.values): a constant, an operation,
        each variable as ``resolve`` states the values it may hold (a parameter, or a choice among
        values), a helper's call as what it returns for the arguments (``template``), and a member of a
        struct passed by value or of a constant struct (``member``); or None where a value is unknown (a
        field read through a pointer, a call that does not fold). Without ``fold`` the expression itself
        is not first evaluated (``chosen`` states what evaluating it did not give)."""
        typed = _integer_type(cursor.type) if cursor.kind.is_expression() else None
        value = self.evaluate(cursor, truth=truth) if fold else None
        if value is not None:
            return {"c": value, **(typed or {})}
        if depth > _EXPRESSION_DEPTH or self.image_decides(cursor):
            return None
        kind, inner = cursor.kind, _children(cursor)
        if kind == K.PAREN_EXPR and inner:
            return self.expression(inner[-1], resolve, depth + 1, truth)
        if kind in (K.UNEXPOSED_EXPR, K.CSTYLE_CAST_EXPR) and inner:
            single = _operand(cursor)
            operand = self.expression(single, resolve, depth + 1, truth) if single is not None else None
            if operand is not None and typed is None and _is_record(cursor.type) and _is_struct(operand):
                # A struct passed on by value (rhashtable_insert_fast(…, pidfs_ino_ht_params)).
                return operand
            if operand is None or typed is None:
                return None
            same = all(operand.get(key) == typed.get(key) for key in ("w", "s", "b"))
            return operand if same else {"op": "cast", "a": [operand], **typed}
        if kind == K.DECL_REF_EXPR and cursor.referenced is not None and typed is None:
            return self.struct_reference(cursor, resolve, depth)
        if kind == K.DECL_REF_EXPR and cursor.referenced is not None and typed is not None:
            referenced = cursor.referenced
            parent = referenced.semantic_parent
            if referenced.kind == K.PARM_DECL or (
                referenced.kind == K.VAR_DECL and parent is not None and parent.kind == K.FUNCTION_DECL
            ):
                held = resolve(referenced, cursor, depth + 1)
                if held is None:
                    return None
                if "p" in held:
                    return {**held, **typed}
                return {"op": "cast", "a": [held], **typed}
            return None
        if kind == K.MEMBER_REF_EXPR and inner:
            return self.member(cursor, inner[0], resolve, depth, typed)
        if typed is None:
            return None
        if kind == K.UNARY_OPERATOR and inner and libclang.is_address_of(cursor):
            return self.object_address(inner[0], typed)
        if kind == K.UNARY_OPERATOR and inner:
            name = _UNARY_NAMES.get(libclang.unary_kind(cursor))
            operand = self.expression(inner[0], resolve, depth + 1, truth=name == "!")
            return None if name is None or operand is None else {"op": name, "a": [operand], **typed}
        if kind == K.BINARY_OPERATOR and len(inner) == 2:
            if libclang.binary_kind(cursor) == _COMMA:
                return self.expression(inner[1], resolve, depth + 1, truth)
            name = _BINARY_NAMES.get(libclang.binary_kind(cursor))
            if name is None:
                return None
            logical = name in ("&&", "||")
            operands = [self.expression(part, resolve, depth + 1, truth=logical) for part in inner]
            return None if operands == [None, None] else {"op": name, "a": operands, **typed}
        if kind == K.CONDITIONAL_OPERATOR and len(inner) == 3:
            operands = [self.expression(part, resolve, depth + 1, truth=index == 0) for index, part in enumerate(inner)]
            return None if operands == [None, None, None] else {"op": "?:", "a": operands, **typed}
        if kind == K.CALL_EXPR and cursor.referenced is not None and cursor.referenced.spelling == "__builtin_expect":
            arguments = list(cursor.get_arguments() or [])
            return self.expression(arguments[0], resolve, depth + 1, truth) if arguments else None
        if kind == K.CALL_EXPR and cursor.referenced is not None and cursor.referenced.kind == K.FUNCTION_DECL:
            template = self.template(cursor.referenced.spelling)
            if template is None:
                return None
            arguments = [self.expression(argument, resolve, depth + 1) for argument in cursor.get_arguments() or []]
            returned = expressions.substitute(template, arguments)
            found = expressions.values(returned)
            if found is not None and len(found) == 1:
                return {"c": next(iter(found)), **typed}
            return returned if _size(returned) <= _TEMPLATE_NODES else None
        return None

    def member(
        self, cursor: ci.Cursor, base: ci.Cursor, resolve: _Resolve, depth: int, typed: dict[str, Any] | None
    ) -> dict[str, Any] | None:
        """``s.m`` of a struct the expressions state (a parameter passed by value, a constant struct, a
        member that is a struct of one): rht_head_hashfn's ``params.obj_hashfn``, which a caller's
        constant params decides. A bit-field, or a member reached through a pointer, is unknown."""
        field = cursor.referenced
        if field is None or field.kind != K.FIELD_DECL or field.is_bitfield():
            return None
        if typed is None and not _is_record(cursor.type):
            return None
        below = _unnamed_base(base)
        record = below.type.get_canonical()
        offset = record.get_offset(field.spelling) if record.kind == TK.RECORD else -1
        struct = self.expression(below, resolve, depth + 1) if offset >= 0 else None
        if struct is None or not _is_struct(struct):
            return None
        return {"op": ".", "a": [struct], "o": offset, **(typed or {})}

    def object_address(self, operand: ci.Cursor, typed: dict[str, Any]) -> dict[str, Any] | None:
        """``&g`` of a global no declaration the unit sees makes weak: an address that is not 0
        (pagetable_pte_ctor's ``mm != &init_mm`` holds for NULL), named so that two are compared alike."""
        node = _strip_parens(operand)
        variable = node.referenced if node.kind == K.DECL_REF_EXPR else None
        if variable is None or not _is_global_var(variable):
            return None
        declarations = (variable, variable.canonical, variable.get_definition())
        if any(self.weak(declaration) for declaration in declarations if declaration is not None):
            return None
        return {"g": variable.get_usr(), **typed}

    def struct_reference(self, cursor: ci.Cursor, resolve: _Resolve, depth: int) -> dict[str, Any] | None:
        """A struct a reference names: a parameter passed by value, as itself, or a const global, as its
        initializer gives it (``struct_value``)."""
        referenced = cursor.referenced
        if not _is_record(cursor.type):
            return None
        if _is_global_var(referenced):
            return self.struct_value(referenced)
        if referenced.kind != K.PARM_DECL:
            return None
        held = resolve(referenced, cursor, depth + 1)
        return {"p": held["p"]} if held is not None and set(held) == {"p"} else None

    def struct_value(self, variable: ci.Cursor) -> dict[str, Any] | None:
        """A const global struct's value (fs/pidfs.c's pidfs_ino_ht_params): the bits of each scalar
        member its initializer gives, with the value (None where it cannot be stated), as
        ``{"struct": [[bits, value], ...]}``; a member the initializer does not give is 0. None where the
        global may change, or its initializer gives a union, an array or a bit-field."""
        key = variable.hash
        if key not in self.structs:
            definition = variable.get_definition() or variable
            typ = definition.type
            initializer = libclang.var_initializer(definition)
            members: dict[int, dict[str, Any] | None] = {}
            complete = (
                typ.is_const_qualified()
                and not typ.is_volatile_qualified()
                and initializer is not None
                and self.record_values(initializer, typ.get_canonical(), 0, members)
                and len(members) <= _STRUCT_MEMBERS
            )
            self.structs[key] = {"struct": [[bits, members[bits]] for bits in sorted(members)]} if complete else None
        return self.structs[key]

    def record_values(
        self, node: ci.Cursor, record: ci.Type, bits: int, members: dict[int, dict[str, Any] | None]
    ) -> bool:
        """The scalar members a struct's initializer list gives, by their bits from the global's start
        (a later designator of a member replaces an earlier one); False for another shape."""
        declaration = record.get_declaration()
        if node.kind != K.INIT_LIST_EXPR or declaration is None or declaration.kind != K.STRUCT_DECL:
            return False
        fields = [field for field in record.get_fields() if field.spelling or not field.is_bitfield()]
        position = 0
        for child in _children(node):
            designators, value = _designation(child)
            if designators:
                designator, *rest = designators
                if rest or designator.kind != K.MEMBER_REF or designator.referenced not in fields:
                    return False
                position = fields.index(designator.referenced)
            if position >= len(fields):
                return False
            field = fields[position]
            position += 1
            offset = field.get_field_offsetof()
            typ = field.type.get_canonical()
            if offset < 0 or field.is_bitfield():
                return False
            if typ.kind == TK.RECORD:
                if not self.record_values(value, typ, bits + offset, members):
                    return False
                continue
            typed = _integer_type(typ)
            if typed is None:
                return False
            number = 0 if _is_null(value) else self.evaluate(value)
            members[bits + offset] = {"c": number, **typed} if number is not None else None
        return True

    def exits(self, statement: ci.Cursor, loop: bool = False) -> bool:
        """Control does not fall through: a return, goto, break or continue, a block that ends in
        one, an if whose constant-true branch exits, a ``do { ... } while (0)`` whose body exits (a
        break or continue in it only leaves the loop), or a noreturn builtin or function called with
        no branch around it."""
        kind = statement.kind
        if kind in _EXITS:
            return not (loop and kind in _LOOP_EXITS)
        inner = _children(statement)
        if kind == K.COMPOUND_STMT or kind in _ENTRIES:
            # A block, or a labeled statement (``case 4: ... break;``), exits when its last part does.
            return bool(inner) and self.exits(inner[-1], loop)
        if kind == K.IF_STMT and len(inner) >= 2:
            value = self.evaluate(inner[0], truth=True)
            if value is not None:
                branch = inner[1] if value else (inner[2] if len(inner) > 2 else None)
                return branch is not None and self.exits(branch, loop)
            return False
        if kind == K.DO_STMT and len(inner) == 2:
            # BUG(): do { __BUG_FLAGS(0); unreachable(); } while (0).
            return self.evaluate(inner[1], truth=True) == 0 and self.exits(inner[0], loop=True)
        if kind in _BRANCHES:
            return False
        found = False
        for node in statement.walk_preorder():
            if _branches_on(node):
                return False
            if node.kind == K.CALL_EXPR and (node.spelling in _UNREACHABLE or _is_noreturn(node.referenced)):
                found = True
        return found

    def chosen(self, cursor: ci.Cursor, truth: bool) -> int | None:
        """The value an expression takes whichever value each of its parts holds (a helper whose returns
        are several constants, a local several stores give): where every choice gives one value, or,
        where only the truth is tested, one truth value. softleaf_type returns SOFTLEAF_NONE, _SWAP or
        _MARKER without CONFIG_DEVICE_PRIVATE, so ``softleaf_type(entry) == SOFTLEAF_DEVICE_EXCLUSIVE``
        is false."""
        key = (cursor.hash, truth)
        if key not in self.choices:
            self.choices[key] = None
            found = expressions.values(self.expression(cursor, self.known, truth=truth, fold=False))
            truths = {int(value != 0) for value in found} if found is not None else set()
            if found is not None and len(found) == 1:
                self.choices[key] = next(iter(found))
            elif truth and len(truths) == 1:
                self.choices[key] = truths.pop()
        return self.choices[key]

    def template(self, name: str) -> dict[str, Any] | None:
        """What a function of the unit returns, as an expression on its parameters (``{"p": i}``): the
        value of each return control can reach, under the conditions it is reached by (an ``if``, a
        ``switch`` case), as the compiler folds it once a call is inlined with its arguments
        (dma_go_direct(dev, mask, NULL) is true; pgtable_level_has_pxx_special(PGTABLE_LEVEL_PTE) is
        IS_ENABLED(CONFIG_ARCH_HAS_PTE_SPECIAL)). None where a label or a jump makes the returns
        another shape, or one cannot be stated."""
        if name in self.templates:
            return self.templates[name]
        self.templates[name] = None
        definition = self.definitions.get(name)
        if definition is None or self.weak(definition):
            return None
        typed = _integer_type(definition.result_type)
        body = next((child for child in _children(definition) if child.kind == K.COMPOUND_STMT), None)
        if typed is None or body is None or any(node.kind in _JUMPS for node in body.walk_preorder()):
            return None
        positions = {parameter.hash: index for index, parameter in enumerate(definition.get_arguments() or [])}
        returned = self.flow([body], None, self.resolver(positions), [_FLOW_STEPS])
        if returned is None or returned is _NEVER:
            return None
        template = {"op": "cast", "a": [returned], **typed}
        self.templates[name] = template if _size(template) <= _TEMPLATE_NODES else None
        return self.templates[name]

    def flow(self, items: list[Any], after_break: list[Any] | None, resolve: _Resolve, budget: list[int]) -> Any:
        """The value control returns from the statements ``items`` (a ``_Resume`` among them restores
        where a ``break`` continues, once a switch ends): an expression, ``_NEVER`` where no path
        returns, or None where a return cannot be stated. A statement that is no branch only changes
        variables, and ``resolve`` states each by every value stored in it."""
        budget[0] -= 1
        if budget[0] < 0 or not items:
            return None
        head, rest = items[0], items[1:]
        if isinstance(head, _Resume):
            return self.flow(rest, head.after_break, resolve, budget)
        kind = head.kind
        if kind == K.RETURN_STMT:
            inner = _children(head)
            return self.expression(inner[0], resolve, 1) if inner else None
        if kind == K.COMPOUND_STMT:
            return self.flow([*_children(head), *rest], after_break, resolve, budget)
        if kind in (K.CASE_STMT, K.DEFAULT_STMT):
            # Falling into the next case of a switch.
            return self.flow([_children(head)[-1], *rest], after_break, resolve, budget)
        if kind == K.BREAK_STMT:
            return None if after_break is None else self.flow(after_break, None, resolve, budget)
        if kind == K.IF_STMT:
            return self.flow_if(head, rest, after_break, resolve, budget)
        if kind == K.SWITCH_STMT:
            return self.flow_switch(head, rest, after_break, resolve, budget)
        if self.transfers(head):
            return None
        if self.exits(head):
            return _NEVER
        return self.flow(rest, after_break, resolve, budget)

    def flow_if(
        self, statement: ci.Cursor, rest: list[Any], after_break: list[Any] | None, resolve: _Resolve, budget: list[int]
    ) -> Any:
        parts = _children(statement)
        if len(parts) < 2:
            return None
        condition, branches = parts[0], parts[1:3]
        decided = self.evaluate(condition, truth=True)
        if decided is not None:
            branch = branches[0] if decided else (branches[1] if len(branches) > 1 else None)
            return self.flow([branch, *rest] if branch is not None else rest, after_break, resolve, budget)
        if not any(self.transfers(branch) or self.exits(branch) for branch in branches):
            return self.flow(rest, after_break, resolve, budget)
        taken = self.flow([branches[0], *rest], after_break, resolve, budget)
        skipped = self.flow([*branches[1:], *rest], after_break, resolve, budget)
        return _choose(self.expression(condition, resolve, 1, truth=True), taken, skipped)

    def flow_switch(
        self, statement: ci.Cursor, rest: list[Any], after_break: list[Any] | None, resolve: _Resolve, budget: list[int]
    ) -> Any:
        parts = _children(statement)
        selector, body = parts[0], parts[-1]
        if len(parts) < 2 or body.kind != K.COMPOUND_STMT or not _switch_labels_top_level(body):
            return None
        statements = _children(body)
        # Past the switch, and where a break in it continues.
        resume = [_Resume(after_break), *rest]
        typed = _integer_type(selector.type) or {}
        labels = [_case_values(statement) for statement in statements]
        cases = [
            None
            if values is None
            else {None if value is None else expressions.normalized(value, typed) for value in values}
            for values in labels
        ]
        default = next((index for index, values in enumerate(cases) if values is not None and None in values), None)

        def entered(index: int | None) -> Any:
            return self.flow([*statements[index:], *resume] if index is not None else resume, resume, resolve, budget)

        selected = self.evaluate(selector)
        if selected is not None:
            return entered(
                next((index for index, values in enumerate(cases) if values and selected in values), default)
            )
        test = self.expression(selector, resolve, 1)
        result = entered(default)
        for index in reversed(range(len(statements))):
            values = cases[index]
            if values is None or None in values:
                continue
            condition = {
                "op": "in",
                "a": [test],
                "set": sorted(value for value in values if value is not None),
                "w": 32,
                "s": True,
            }
            result = _choose(condition, entered(index), result)
        return result

    def transfers(self, statement: ci.Cursor) -> bool:
        """A return, break or continue inside: control may leave the statement for elsewhere."""
        key = statement.hash
        if key not in self.transferring:
            self.transferring[key] = any(node.kind in _TRANSFERS for node in statement.walk_preorder())
        return self.transferring[key]

    def resolver(self, positions: dict[int, int]) -> _Resolve:
        """How expressions state a function's variables: a parameter ``positions`` names as itself
        (``{"p": i}``) or as any value stored in it besides, a local as the values stored in it (see
        ``choice``); another parameter is unknown."""
        holding: set[int] = set()

        def resolve(variable: ci.Cursor, reference: ci.Cursor, depth: int) -> dict[str, Any] | None:
            key = variable.hash
            function = variable.semantic_parent
            if key in holding or depth > _EXPRESSION_DEPTH or function is None or function.kind != K.FUNCTION_DECL:
                return None
            first: dict[str, Any] | None = None
            if variable.kind == K.PARM_DECL:
                if key not in positions:
                    return None
                first = {"p": positions[key]}
            elif variable.storage_class in (ci.StorageClass.STATIC, ci.StorageClass.EXTERN):
                return None
            holding.add(key)
            try:
                return self.choice(function, variable, first, resolve, depth)
            finally:
                holding.discard(key)

        return resolve

    def choice(
        self, function: ci.Cursor, variable: ci.Cursor, first: dict[str, Any] | None, resolve: _Resolve, depth: int
    ) -> dict[str, Any] | None:
        """The values a variable of ``function`` may hold, as a choice (``?:`` on no condition): ``first``
        (a parameter's argument) or a local's initializer, each value a plain assignment stores, and
        each one a ``|=``, ``&=`` or ``^=`` makes of those (dequeue_entity's ``action |= DO_DETACH``).
        None where the function changes it otherwise or takes its address."""
        writes = self.stores_in(function)
        key = variable.hash
        combined = writes.combined.get(key, [])
        typed = _integer_type(variable.type)
        if key in writes.opaque or len(combined) > _COMBINED_LIMIT or (combined and typed is None):
            return None
        sources: list[dict[str, Any] | None] = [first] if first is not None else []
        initializer = libclang.var_initializer(variable) if variable.kind == K.VAR_DECL else None
        if initializer is not None:
            sources.append(self.expression(initializer, resolve, depth + 1))
        sources += [self.expression(value, resolve, depth + 1) for value in writes.stores.get(key, [])]
        if not sources or any(source is None for source in sources):
            return None
        held = sources[0]
        for source in sources[1:]:
            held = {"op": "?:", "a": [None, held, source]}
        for operator, value in combined:
            operand = self.expression(value, resolve, depth + 1)
            if operand is None:
                return None
            held = {"op": "?:", "a": [None, held, {"op": operator, "a": [held, operand], **(typed or {})}]}
        return held if _size(held) <= _TEMPLATE_NODES else None


def _any_way(ways: list[list[dict[str, Any]]]) -> dict[str, Any] | None:
    """The condition that holds where every guard of at least one of ``ways`` holds; None where a way
    has a guard that is no condition (``u``: compiled where its expression is not decided)."""
    if any("v" not in guard for way in ways for guard in way):
        return None
    typed = {"w": 32, "s": True}
    terms = []
    for way in ways:
        factors = [guard["e"] if guard["v"] else {"op": "!", "a": [guard["e"]], **typed} for guard in way]
        term = factors[0]
        for factor in factors[1:]:
            term = {"op": "&&", "a": [term, factor], **typed}
        terms.append(term)
    either = terms[0]
    for term in terms[1:]:
        either = {"op": "||", "a": [either, term], **typed}
    return either


class _Tagged(list[Any]):
    """An event list that puts the walk's current guards on each event appended (``pair``: the event
    is the second item of an appended tuple)."""

    def __init__(self, walk: _FunctionWalk, pair: bool = False) -> None:
        super().__init__()
        self.walk = walk
        self.pair = pair

    def append(self, item: Any) -> None:
        if self.walk.guard_stack:
            event = item[1] if self.pair else item
            event["guards"] = list(self.walk.guard_stack)
        super().append(item)


class _FunctionWalk:
    """The facts of one function body."""

    def __init__(
        self,
        extractor: _Extractor,
        cursor: ci.Cursor,
        live_labels: frozenset[str] = frozenset(),
        dead_variables: frozenset[int] = frozenset(),
        decided_variables: frozenset[int] = frozenset(),
        overwritten: frozenset[int] = frozenset(),
        returns_discarded: bool = False,
    ) -> None:
        self.x = extractor
        self.cursor = cursor
        self.parameters = {parameter.hash: index for index, parameter in enumerate(cursor.get_arguments() or [])}
        self.events: dict[str, list[Any]] = {
            kind: _Tagged(self)
            for kind in (
                "calls", "accesses", "unevaluated", "list_calls", "indirect", "function_pointers", "field_accesses",
                "parameter_calls",
            )
        }  # fmt: skip
        self.conditional = 0  # depth of __builtin_constant_p branches being walked
        self.callees: set[int] = set()  # the hashes of the references that name a direct call's callee
        # Liveness: the labels a goto that is code targets reach code, and a store to a variable
        # nothing reads is not code. The walk counts the reads of each variable in code, and the
        # labels the gotos in code target, for the next walk (``_Extractor.analysis``).
        self.live_labels = live_labels
        self.dead_variables = dead_variables
        self.decided_variables = decided_variables
        self.decided_reads: collections.Counter[int] = collections.Counter()  # reads the image decides
        self.goto_targets: set[str] = set()
        self.reads: collections.Counter[int] = collections.Counter()
        self.stores: set[int] = set()  # the references that are the target of a store, not a read
        # Per variable, the references counted as reads and the stores of a whole value as statements
        # (an initial value, a plain assignment), and the variables whose address is taken. The stores
        # every read sees stored again first (``overwritten_stores``) are not code in the next walk.
        self.read_sites: dict[int, list[ci.Cursor]] = collections.defaultdict(list)
        # Per variable, the guards each read in compiled code is under (None where the image decides).
        self.read_guards: dict[int, list[tuple[int, ...] | None]] = collections.defaultdict(list)
        self.store_sites: dict[int, list[ci.Cursor]] = collections.defaultdict(list)
        self.address_taken: set[int] = set()
        self.overwritten = overwritten
        self.same_outcomes: set[int] = set()  # the ifs whose two outcomes give the same, without effect
        # What compiled code does besides computing values: effects (a call of a function that has
        # one, an asm the compiler keeps, a volatile access, a store outside the function's locals, a
        # loop that may not end) and loops. A call of the function may go when it has neither.
        self.effects = 0
        self.compiled_loops = 0
        self.statement_values: set[int] = set()  # the expression statements outside blocks, discarded
        # The walk of a call whose value nothing uses: each return's value is discarded. The calls
        # this walk discards, which have an effect: such a call of a helper is folded the same way.
        self.returns_discarded = returns_discarded
        self.unused_calls: set[int] = set()
        # Member addresses a call's argument passes to a helper that takes them back with
        # container_of (the member's hash, the helper), and the member addresses the function returns.
        self.cancelled_members: dict[int, str] = {}
        self.returned_members: set[int] = set()
        # Field addresses: each member chain's topmost member gets the chain's address; the members
        # below it (and the ones a local's definition folds away) are addressed only through it.
        self.chained: set[int] = set()
        self.field_address: dict[int, tuple[_Address, bool]] = {}
        self.inner: set[int] = set()
        self.addressed: set[int] = set()
        self.fields: list[tuple[int, dict[str, Any]]] = _Tagged(self, pair=True)
        # The member addresses a local holds (``remove = &vp->remove``): the local, the field access and
        # the access event of the member, which ``finish`` puts under the guards of the local's reads.
        self.held_addresses: list[tuple[ci.Cursor, list[dict[str, Any]]]] = []
        self.derived_cache: dict[tuple[int, tuple[int, ...], bool], _Address | None] = {}
        self.uses: dict[str, collections.Counter[int]] | None = None
        # Each local's and parameter's plain assignments: the value and where the assignment is.
        self.definitions: dict[int, list[tuple[ci.Cursor, _Place | None]]] = collections.defaultdict(list)
        self.parents: dict[int, ci.Cursor] = {}
        self.label_holders: set[int] = set()  # the nodes that hold a label or a case
        self.loops: list[_Place] = []
        self.backward = False  # the function jumps back (a goto to an earlier label, a computed goto)
        # Guards: the conditions on the parameters under which the code being walked is compiled.
        self.guard_table: list[dict[str, Any]] = []
        self.guard_ids: dict[str, int] = {}
        self.guard_stack: list[int] = []
        self.case_guards: dict[int, tuple[dict[str, Any], bool]] = {}
        self.holding: set[int] = set()  # the variables whose values are being stated (a cycle is unknown)
        self.positions: dict[str, Any] | None = None
        # Per label, the guards each goto in code reached it under (None: under a choice the image
        # makes, or a label whose address is taken), in the order the walk met them.
        self.goto_guards: dict[str, list[list[dict[str, Any]] | None]] = collections.defaultdict(list)
        self.jumps: dict[str, Any] | None = None
        self.labelled: dict[int, bool] = {}

    def finish(self) -> None:
        """The field accesses the machine code addresses on their own. A member address a local holds
        is formed only where a read of the local is compiled: it is under the guards of those reads."""
        for variable, items in self.held_addresses:
            guards = self.read_guard(variable)
            for item in items if guards else []:
                kept = item.get("guards", [])
                item["guards"] = [*kept, *(index for index in guards if index not in kept)]
        self.events = {kind: list(items) for kind, items in self.events.items()}
        self.events["field_accesses"] = [item for member, item in self.fields if member not in self.inner]

    def read_guard(self, variable: ci.Cursor) -> list[int]:
        """The guards under which some read of the variable in compiled code is: those every read is
        under, and a guard that one of the reads' other guards all hold; none where a read is under no
        guard or where the image decides, or the variable may be read another way (its address, a
        cleanup)."""
        found = self.read_guards.get(variable.hash, [])
        reads = [read for read in found if read]
        if (
            not reads
            or len(reads) != len(found)
            or variable.hash in self.address_taken
            or any(child.kind == K.UNEXPOSED_ATTR for child in variable.get_children())
        ):
            return []
        shared = [index for index in reads[0] if all(index in read for read in reads)]
        ways = {tuple(index for index in read if index not in shared) for read in reads}
        if () in ways:
            return shared
        either = _any_way([[self.guard_table[index] for index in way] for way in sorted(ways)])
        return shared if either is None else [*shared, self.guard_id(either, True)]

    def walk_held(self, variable: ci.Cursor, value: ci.Cursor, parent: ci.Cursor, index: int, generic: bool) -> None:
        """Walk the value a local is given, as code. Where it is a member's address (``remove =
        &vp->remove``, or an array member that decays to its address), the local holds that address."""
        start = len(self.fields)
        self.walk(value, parent, index, None, generic)
        member = _strip(value)
        if member.kind == K.UNARY_OPERATOR and libclang.is_address_of(member) and _children(member):
            member = _strip(_children(member)[0])
        if member.kind != K.MEMBER_REF_EXPR or variable.type.get_canonical().kind != TK.POINTER:
            return
        for where, access in self.fields[start:]:
            if where == member.hash and access.get("address"):
                events = [item for item in self.events["accesses"] if item["provenance"] is access["provenance"]]
                self.held_addresses.append((variable, [access, *events]))

    def never_read(self) -> frozenset[int]:
        """The parameters and locals of the function that compiled code never reads: every
        read is in code no control reaches, in an unevaluated operand, in a discarded value, or an
        argument a callee ignores. A variable whose address is taken counts as read, and so does one
        with a cleanup attribute, which its cleanup receives. A parameter may have any type: one of
        array type is a pointer (free_cpumask_var's cpumask_var_t), and a struct passed by value
        that is never read is not copied (is_mergeable_vm_userfaultfd_ctx)."""
        return frozenset(
            variable.hash
            for variable in self.tracked_variables()
            if self.reads[variable.hash] == 0 and self.decided_reads[variable.hash] == 0
        )

    def read_only_where_the_image_decides(self) -> frozenset[int]:
        """The variables whose every read in compiled code is where the image decides: under a
        __builtin_constant_p choice, or an argument a callee that is not inline ignores."""
        return frozenset(
            variable.hash
            for variable in self.tracked_variables()
            if self.reads[variable.hash] == 0 and self.decided_reads[variable.hash] > 0
        )

    def tracked_variables(self) -> list[ci.Cursor]:
        """The parameters, and the locals without a cleanup, that are not volatile. A struct or an
        array local counts too: one nothing reads is not laid out (rcu_start_exp_gp_kworker's
        ``struct sched_param param``, whose address only a call under IS_ENABLED takes)."""
        parameters = list(self.cursor.get_arguments() or [])
        locals_ = [
            node
            for node in self.cursor.walk_preorder()
            if node.kind == K.VAR_DECL
            and _is_local(node)
            and not any(child.kind == K.UNEXPOSED_ATTR for child in node.get_children())
        ]
        return [variable for variable in [*parameters, *locals_] if not variable.type.is_volatile_qualified()]

    def overwritten_stores(self) -> frozenset[int]:
        """The stores of a whole value to a local (its initial value, or a plain assignment as a
        statement) that no read sees: for every read the walk counted, a block around it without a
        label or a case assigns the local again in an earlier statement, and the store is not between
        that assignment and the read. Control enters such a block only at its start, so every path
        from the store to the read passes the assignment. ipi_mux_send_mask's ``icpu =
        this_cpu_ptr(ipi_mux_pcpu)`` is stored again at the top of the loop before any read, and
        wait_task_inactive's ``rq = task_rq(p)``, read only by task_on_cpu, which ignores it, again by
        ``rq = task_rq_lock(p, &rf)`` before every other read. A local whose address is taken may be
        read through it, and a struct or an array may be stored in part: neither counts."""
        found: set[int] = set()
        for variable, stores in self.store_sites.items():
            kills = [store for store in stores if store.kind == K.BINARY_OPERATOR]
            reads = self.read_sites.get(variable)
            if len(stores) < 2 or not kills or not reads or variable in self.address_taken:
                continue
            target = _strip_parens(_children(kills[0])[0]).referenced
            if target is None or target.type.get_canonical().kind in (TK.RECORD, *_ARRAY_TYPES):
                continue
            self.local_uses()
            for store in stores:
                others = {kill.hash for kill in kills if kill.hash != store.hash}
                if others and all(self.stored_again_before(read, store, others) for read in reads):
                    found.add(store.hash)
        return frozenset(found)

    def stored_again_before(self, read: ci.Cursor, store: ci.Cursor, kills: set[int]) -> bool:
        """A block around ``read`` without a label or a case has one of ``kills`` (assignments of the
        variable as statements of that block) before the statement that holds the read, and
        ``store`` is not in a statement from the one after that assignment through that one."""
        positions = self.block_positions(store)
        child, parent = read, self.parents.get(read.hash)
        while parent is not None:
            if parent.kind == K.COMPOUND_STMT and parent.hash not in self.label_holders:
                statements = _children(parent)
                at = next(index for index, statement in enumerate(statements) if statement.hash == child.hash)
                earlier = [index for index in range(at) if statements[index].hash in kills]
                if earlier:
                    stored = positions.get(parent.hash)
                    if stored is None or not earlier[-1] < stored <= at:
                        return True
            if parent.hash == self.cursor.hash:
                return False
            child, parent = parent, self.parents.get(parent.hash)
        return False

    def block_positions(self, node: ci.Cursor) -> dict[int, int]:
        """For each block around ``node``, the index of its statement that holds ``node``."""
        positions: dict[int, int] = {}
        child, parent = node, self.parents.get(node.hash)
        while parent is not None:
            if parent.kind == K.COMPOUND_STMT:
                statements = _children(parent)
                positions[parent.hash] = next(
                    index for index, statement in enumerate(statements) if statement.hash == child.hash
                )
            child, parent = parent, self.parents.get(parent.hash)
        return positions

    def statement_without_effect(self, statement: ci.Cursor, block: ci.Cursor, index: int) -> bool:
        """A statement of a block, not an expression, that is not code beyond what its parts do
        besides computing values: an if whose branches have no effect, a loop without effect, or an
        if whose two outcomes agree."""
        if self.x.branches_without_effect(statement, self.dead_variables):
            return True
        if statement.kind in _LOOPS:
            return self.x.loop_without_effect(statement, self.cursor, self.dead_variables)
        if self.same_outcome(statement, block, index):
            self.same_outcomes.add(statement.hash)
            return True
        return False

    def same_outcome(self, statement: ci.Cursor, block: ci.Cursor, index: int) -> bool:
        """``if (c) return v;`` whose return and the rest of its block give the same, neither with an
        effect, so ``c`` decides nothing: rcutree_online_cpu's ``if (rcu_scheduler_active ==
        RCU_SCHEDULER_INACTIVE) return 0;`` before ``tick_dep_clear(TICK_DEP_BIT_RCU); return 0;``
        (tick_dep_clear is empty without NO_HZ_FULL), and clk_pm_runtime_put's ``if
        (!core->rpm_enabled) return;`` before ``pm_runtime_put_sync(core->dev);``, a stub."""
        children = _children(statement)
        if statement.kind != K.IF_STMT or len(children) != 2 or _mentions(children[0], _IMAGE_DECIDES):
            return False
        if self.x.evaluator.evaluate(children[0], truth=True) is not None:
            return False
        taken = _children(children[1]) if children[1].kind == K.COMPOUND_STMT else [children[1]]
        if not taken or taken[-1].kind != K.RETURN_STMT:
            return False
        body = _function_body(self.cursor)
        falls_off = body is not None and block.hash == body.hash and self.cursor.result_type.kind == TK.VOID
        then = self.x.outcome(taken, self.dead_variables, falls_off=False)
        return then is not None and then == self.x.outcome(
            _children(block)[index + 1 :], self.dead_variables, falls_off
        )

    # Guards ---------------------------------------------------------------------------------

    def held(self, variable: ci.Cursor, reference: ci.Cursor, depth: int) -> dict[str, Any] | None:
        """The values a parameter or local may hold at ``reference``, for link to decide at a fold:
        a parameter nothing writes is the caller's argument; one whose address escapes only after the
        reference, which no loop or label puts before the escape, too. A variable that plain
        assignments write may hold its first value (the argument, or the initializer; a local read
        before every store reads an indeterminate value) or any value assigned, and one that ``|=``,
        ``&=`` or ``^=`` change also what each makes of those (dequeue_entity's ``action |=
        DO_DETACH``): a choice, decided only where every choice agrees. A pointer set once whose
        value cannot be stated is named instead (``symbol``)."""
        key = variable.hash
        if key in self.holding or depth > _EXPRESSION_DEPTH:
            return None
        uses = self.local_uses()
        index = self.parameters.get(key) if variable.kind == K.PARM_DECL else None
        combined = self.combined(variable, uses["written"][key])
        if combined is None:
            if index is None or uses["assigned"][key] or not self.escapes_after(variable, reference):
                return None
            return {"p": index}
        evaluator = self.x.evaluator
        self.holding.add(key)
        try:
            values: list[dict[str, Any] | None] = [{"p": index}] if index is not None else []
            initializer = libclang.var_initializer(variable) if index is None else None
            if initializer is not None:
                values.append(evaluator.expression(initializer, self.held, depth + 1))
            values += [
                evaluator.expression(definition, self.held, depth + 1)
                for definition, _ in self.definitions.get(key, [])
            ]
            operands = [(operator, evaluator.expression(value, self.held, depth + 1)) for operator, value in combined]
        finally:
            self.holding.discard(key)
        if not values or any(value is None for value in values) or any(operand is None for _, operand in operands):
            return self.symbol(variable)
        choice = values[0]
        for value in values[1:]:
            choice = {"op": "?:", "a": [None, choice, value]}
        typed = _integer_type(variable.type) or {}
        for operator, operand in operands:
            choice = {"op": "?:", "a": [None, choice, {"op": operator, "a": [choice, operand], **typed}]}
        return choice

    def combined(self, variable: ci.Cursor, written: int) -> list[tuple[str, ci.Cursor]] | None:
        """The ``|=``, ``&=`` and ``^=`` the function makes to a variable, where those are its
        ``written`` writes besides plain assignments (none for a variable no other write changes), or
        None where another write changes it or takes its address, or there are too many."""
        if not written:
            return []
        writes = self.x.evaluator.stores_in(self.cursor)
        found = writes.combined.get(variable.hash, [])
        if variable.hash in writes.opaque or len(found) != written or len(found) > _COMBINED_LIMIT:
            return None
        return found

    def symbol(self, variable: ci.Cursor) -> dict[str, Any] | None:
        """A pointer local the function sets once, by its initializer or one plain assignment that no
        loop or label repeats, and changes in no other way: every read after that store reads one
        value, which the facts name (``{"v": name}``) though they cannot state it, so that two reads
        are one value (switch_hrtimer_base's new_cpu_base, set from this_cpu_base or as it)."""
        key = variable.hash
        typed = _integer_type(variable.type)
        if variable.kind != K.VAR_DECL or variable.storage_class in (ci.StorageClass.STATIC, ci.StorageClass.EXTERN):
            return None
        if typed is None or variable.type.get_canonical().kind != TK.POINTER or self.local_uses()["written"][key]:
            return None
        initializer = libclang.var_initializer(variable)
        definitions = [definition for definition, _ in self.definitions.get(key, [])]
        if initializer is not None:
            definitions.append(variable)
        if len(definitions) != 1:
            return None
        (definition,) = definitions
        layout = self.layout()
        start = definition.extent.start
        if definition.hash in layout["loops"] or start.file is None or self.cursor.extent.start.file is None:
            return None
        if start.file.name != self.cursor.extent.start.file.name or any(
            label < start.offset for label in layout["labels"]
        ):
            return None
        return {"v": self.x.object_name(variable), **typed}

    def layout(self) -> dict[str, Any]:
        """The function's nodes inside a loop and its labels' offsets, and per variable the offsets of
        ``&variable`` and whether it is changed otherwise (``op=``, ``++``)."""
        if self.positions is None:
            loops: set[int] = set()
            labels: list[int] = []
            escapes: dict[int, list[int]] = collections.defaultdict(list)
            other: set[int] = set()
            for node in self.cursor.walk_preorder():
                if node.kind in (K.WHILE_STMT, K.FOR_STMT, K.DO_STMT):
                    loops.update(child.hash for child in node.walk_preorder())
                elif node.kind == K.LABEL_STMT:
                    labels.append(node.extent.start.offset)
                operands = _children(node)
                target = _strip(operands[0]) if operands else None
                if target is None or target.kind != K.DECL_REF_EXPR or target.referenced is None:
                    continue
                if node.kind == K.UNARY_OPERATOR and libclang.is_address_of(node):
                    escapes[target.referenced.hash].append(node.extent.start.offset)
                elif node.kind == K.COMPOUND_ASSIGNMENT_OPERATOR or (
                    node.kind == K.UNARY_OPERATOR and libclang.is_increment(node)
                ):
                    other.add(target.referenced.hash)
            self.positions = {"loops": loops, "labels": labels, "escapes": escapes, "other": other}
        return self.positions

    def escapes_after(self, parameter: ci.Cursor, reference: ci.Cursor) -> bool:
        """Every write to the parameter is ``&parameter`` after the reference in the function's text, the
        reference is in no loop, and no label is before it: control reaches it before any escape."""
        positions = self.layout()
        key = parameter.hash
        at = reference.extent.end.offset
        if key in positions["other"] or reference.hash in positions["loops"]:
            return False
        if reference.extent.start.file is None or self.cursor.extent.start.file is None:
            return False
        if reference.extent.start.file.name != self.cursor.extent.start.file.name:
            return False
        if any(label < at for label in positions["labels"]):
            return False
        return bool(positions["escapes"][key]) and all(offset > at for offset in positions["escapes"][key])

    def guard_expression(self, condition: ci.Cursor, truth: bool = True) -> dict[str, Any] | None:
        """The condition as an expression on the parameters, when it names one."""
        expression = self.x.evaluator.expression(condition, self.held, truth=truth)
        return expression if expressions.mentions_parameter(expression) else None

    def object_operand(self, operand: ci.Cursor) -> bool:
        """The operand is the address of an object the program defines (``&init_mm``), never 0."""
        return expressions.nonzero(self.x.evaluator.expression(operand, self.held))

    def label_guard(self, label: ci.Cursor, fallthrough: list[dict[str, Any]] | None) -> dict[str, Any] | None:
        """The guard on a label's code: reached by the gotos met before it and, unless ``fallthrough`` is
        None, by control falling into it under those guards; so compiled where any of them is. None
        when a goto after the label (a loop) or a computed goto may reach it, or one way is unguarded."""
        if self.jumps is None:
            later: set[str] = set()
            places: dict[str, int] = {}
            gotos: list[tuple[str, int]] = []
            for node in self.cursor.walk_preorder():
                if node.kind == K.LABEL_STMT:
                    places[node.spelling] = node.extent.start.offset
                elif node.kind in (K.GOTO_STMT, K.ADDR_LABEL_EXPR, K.INDIRECT_GOTO_STMT):
                    targets = [child.spelling for child in _children(node) if child.kind == K.LABEL_REF]
                    if node.kind != K.GOTO_STMT or not targets:
                        later.update(places)  # a computed goto may reach any label
                        later.add("*")
                    gotos += [(target, node.extent.start.offset) for target in targets]
            later |= {target for target, offset in gotos if target in places and offset > places[target]}
            self.jumps = {"later": later}
        name = label.spelling
        if name in self.jumps["later"] or "*" in self.jumps["later"]:
            return None
        ways = list(self.goto_guards.get(name, []))
        if fallthrough is not None:
            ways.append(fallthrough)
        if not ways or any(not way for way in ways):
            return None
        return _any_way([way for way in ways if way is not None])

    def has_label(self, node: ci.Cursor) -> bool:
        """A label inside that control may enter from outside, past any condition around it: a goto
        label, or a case of an enclosing switch (one no switch inside the node owns)."""
        key = node.hash
        if key not in self.labelled:
            found = False
            # The node's own case labels are those the guard describes.
            root = node
            while root.kind in (K.CASE_STMT, K.DEFAULT_STMT) and _children(root):
                root = _children(root)[-1]
            stack = [root]
            while stack and not found:
                current = stack.pop()
                if current.kind in _ENTRIES:
                    found = True
                elif current.kind != K.SWITCH_STMT:
                    stack.extend(_children(current))
                else:
                    # A nested switch's cases are entered only from it; a goto label inside it is not.
                    found = any(child.kind == K.LABEL_STMT for child in current.walk_preorder())
            self.labelled[key] = found
        return self.labelled[key]

    def guard_id(self, expression: dict[str, Any], value: bool | None) -> int:
        """The guard's index in the function's table, added once. A guard with no ``value`` holds where
        its expression is not decided (``u``): what only computes an operand of a comparison the caller's
        constants fold."""
        guard = {"e": expression, "v": value} if value is not None else {"e": expression, "u": True}
        key = jsonio.canonical(guard)
        if key not in self.guard_ids:
            self.guard_ids[key] = len(self.guard_table)
            self.guard_table.append(guard)
        return self.guard_ids[key]

    def push_guard(self, expression: dict[str, Any], value: bool | None) -> None:
        self.guard_stack.append(self.guard_id(expression, value))

    def walk_guarded(
        self,
        guard: tuple[dict[str, Any] | None, bool | None] | None,
        node: ci.Cursor,
        parent: ci.Cursor | None,
        index: int,
        reason: str | None,
        generic: bool,
    ) -> None:
        """Walk a branch under a guard, unless a label inside lets a goto enter it without one."""
        pushed = guard is not None and guard[0] is not None and not self.has_label(node)
        if pushed:
            assert guard is not None and guard[0] is not None
            self.push_guard(guard[0], guard[1])
        try:
            self.walk(node, parent, index, reason, generic)
        finally:
            if pushed:
                self.guard_stack.pop()

    def discard(
        self, node: ci.Cursor, parent: ci.Cursor | None, index: int, generic: bool, reason: str = "discarded"
    ) -> None:
        """Walk an expression whose value nothing uses. Without effects it is not computed at all;
        otherwise only its effects are, with what they need: a call with an effect, a volatile
        access or a store is walked as code, and the operators around them only pass values on
        (psi_sched_switch(..., !task_on_rq_queued(prev) || prev->se.sched_delayed): the READ_ONCE
        in task_on_rq_queued stays, and the read of the right operand of || goes). In ({ ... }) the
        statements before the value are code and the value is discarded."""
        if self.x.effect_free(node, self.dead_variables):
            self.walk(node, parent, index, reason, generic)
            return
        kind = node.kind
        children = _children(node)
        if kind == K.StmtExpr and children and children[0].kind == K.COMPOUND_STMT:
            statements = _children(children[0])
            for statement_index, statement in enumerate(statements):
                if statement_index == len(statements) - 1 and statement.kind.is_expression():
                    self.discard(statement, children[0], statement_index, generic, reason)
                else:
                    self.walk(statement, children[0], statement_index, None, generic)
            return
        if kind == K.IF_STMT and (
            node.hash in self.same_outcomes or self.x.branches_without_effect(node, self.dead_variables)
        ):
            # An if whose branches have no effect, or whose two outcomes agree: only what its
            # condition does is code.
            self.discard(children[0], node, 0, generic, reason)
            for child_index, child in enumerate(children[1:], 1):
                self.walk(child, node, child_index, reason, generic)
            return
        if kind in _LOOPS and self.x.loop_without_effect(node, self.cursor, self.dead_variables):
            # A loop without effect (flush_all_rcu_sheaves' list walk with CONFIG_SLUB_TINY): none
            # of it is code.
            self.walk(node, parent, index, reason, generic)
            return
        if kind == K.BINARY_OPERATOR and len(children) == 2:
            operator = libclang.binary_kind(node)
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR, _COMMA) and self.x.effect_free(
                children[1], self.dead_variables
            ):
                # The right operand only gives the unused value.
                self.discard(children[0], node, 0, generic, reason)
                self.walk(children[1], node, 1, reason, generic)
                return
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
                # The left operand decides whether the right one's effects happen.
                self.walk(node, parent, index, None, generic)
                return
        if (
            kind in (K.CALL_EXPR, K.CONDITIONAL_OPERATOR, K.GENERIC_SELECTION_EXPR)
            or not kind.is_expression()
            or node.type.is_volatile_qualified()
            or _store_target(node) is not None
        ):
            if kind == K.CALL_EXPR:
                # A call with an effect whose value nothing uses: of a helper the image inlines, only
                # what its effects need is code (``value_unused``).
                self.unused_calls.add(node.hash)
            self.walk(node, parent, index, None, generic)
            return
        # An operator, a cast or a member access: its own value is unused, and its operands give only
        # that value, so each is discarded in turn.
        if kind in (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR):
            self.reference(node, parent, index, reason, generic)
        for child_index, child in enumerate(children):
            if kind in (K.CSTYLE_CAST_EXPR, K.COMPOUND_LITERAL_EXPR) and child_index < len(children) - 1:
                self.walk(child, node, child_index, "type", generic)
            else:
                self.discard(child, node, child_index, generic, reason)

    def exit_guard(self, statement: ci.Cursor) -> tuple[dict[str, Any], bool] | None:
        """The guard on the statements after ``if (c) return;`` (or a branch that exits otherwise):
        they are compiled only where the exiting branch is not taken."""
        if statement.kind != K.IF_STMT:
            return None
        parts = _children(statement)
        if len(parts) < 2 or _mentions(parts[0], _IMAGE_DECIDES):
            return None
        if self.x.evaluator.evaluate(parts[0], truth=True) is not None:
            return None
        evaluator = self.x.evaluator
        then_exits = evaluator.exits(parts[1])
        else_exits = len(parts) > 2 and evaluator.exits(parts[2])
        if then_exits == else_exits:
            return None
        expression = self.guard_expression(parts[0])
        return None if expression is None else (expression, not then_exits)

    def switch_guards(self, selector: ci.Cursor, body: ci.Cursor) -> None:
        """The guard of each statement of a switch body on the selector: the cases control reaches it
        from, falling through, up to a statement that exits."""
        expression = self.guard_expression(selector, truth=False)
        if expression is None or body.kind != K.COMPOUND_STMT or not _switch_labels_top_level(body):
            return
        statements = _children(body)
        labels = [_case_values(statement) for statement in statements]
        every = sorted({value for values in labels if values is not None for value in values if value is not None})
        reaching: set[int] = set()
        default = False
        for statement, values in zip(statements, labels, strict=True):
            if values is not None:
                reaching |= {value for value in values if value is not None}
                default = default or None in values
            if reaching or default:
                selected = sorted(set(every) - reaching) if default else sorted(reaching)
                condition = {"op": "in", "a": [expression], "set": selected, "w": 32, "s": True}
                self.case_guards[statement.hash] = (condition, not default)
            if self.x.evaluator.exits(statement):
                reaching, default = set(), False

    # Addresses ------------------------------------------------------------------------------

    def lvalue(self, node: ci.Cursor, bits: int, members: list[ci.Cursor], depth: int) -> _Address:
        """The address of an lvalue: ``.`` and ``->`` members, subscripts and ``*p`` down to an object
        the function names or a pointer value. ``members`` collects the member expressions passed."""
        node = _strip(node)
        if depth > 12:
            return _Address(0, "unknown")
        children = _children(node)
        if node.kind == K.MEMBER_REF_EXPR and children and node.referenced is not None:
            if node.referenced.kind != K.FIELD_DECL:
                return _Address(0, "unknown")
            members.append(node)
            base = _unnamed_base(children[0])
            typ = base.type.get_canonical()
            through = typ.kind == TK.POINTER
            record = typ.get_pointee().get_canonical() if through else typ
            offset = record.get_offset(node.referenced.spelling) if record.kind == TK.RECORD else -1
            if offset < 0:
                return _Address(0, "unknown")
            if through:
                return self.pointer(base, bits + offset, members, depth + 1)
            return self.lvalue(base, bits + offset, members, depth + 1)
        if node.kind == K.ARRAY_SUBSCRIPT_EXPR and len(children) == 2:
            array, index = children
            size = node.type.get_canonical().get_size()
            value = self.x.evaluator.evaluate(index)
            indexed = self.index_of(index) if value is None and size > 0 else None
            # A variable index is added in a register: the constant part of the address stays in the
            # displacement of an access the chain makes, but not past a value that holds the address.
            steps = value if value is not None else indexed[1] if indexed is not None else 0
            element = steps * size * 8 if size > 0 else 0
            if _strip(array).type.get_canonical().kind in _ARRAY_TYPES:
                found = self.lvalue(array, bits + element, members, depth + 1)
            else:
                found = self.pointer(array, bits + element, members, depth + 1)
            if value is not None:
                return found
            # The nearest variable index decides what a register that holds the element adds.
            return _indexed(replace(found, element=bits), indexed, size * 8)
        if node.kind == K.UNARY_OPERATOR and children and libclang.unary_kind(node) == _DEREF:
            return self.pointer(children[0], bits, members, depth + 1)
        if node.kind == K.DECL_REF_EXPR and node.referenced is not None:
            if _is_global_var(node.referenced):
                return _Address(bits, "global")
            if node.referenced.kind in (K.PARM_DECL, K.VAR_DECL):
                variable = node.referenced
                size = variable.type.get_canonical().get_size()
                return _Address(bits, "local", object=(self.x.object_name(variable), max(size, 0)))
        return _Address(bits, "other")

    def pointer(self, node: ci.Cursor, bits: int, members: list[ci.Cursor], depth: int) -> _Address:
        """Where a pointer value points, as far as the compiler folds it: through ``&``, container_of,
        a local whose every definition points the same way, and an inline helper that returns an
        address derived from one of its parameters."""
        node = _strip(node)
        if depth > 12:
            return _Address(0, "unknown")
        children = _children(node)
        if node.kind == K.UNARY_OPERATOR and children and libclang.is_address_of(node):
            return self.lvalue(children[0], bits, members, depth + 1)
        container = _container(node)
        if container is not None:
            contained = self.pointer(container[0], bits - container[1] * 8, members, depth + 1)
            return contained if contained.base in _OBJECT_BASES else replace(contained, base="container_of")
        if node.kind == K.StmtExpr and children and children[0].kind == K.COMPOUND_STMT:
            # ({ ...; value; }): to_swnode's ({ ... is_software_node(__f) ? container_of(...) : NULL; }).
            statements = _children(children[0])
            if statements and statements[-1].kind.is_expression():
                return self.pointer(statements[-1], bits, members, depth + 1)
        if node.type.get_canonical().kind in _ARRAY_TYPES:
            # An array that decays to its address (sb->s_writers.rw_sem): the array's own address.
            return self.lvalue(node, bits, members, depth + 1)
        arithmetic = self.arithmetic(node)
        if arithmetic is not None:
            pointer, right, step, sign = arithmetic
            value = self.x.evaluator.evaluate(right)
            if value is not None:
                return self.pointer(pointer, bits + sign * value * step, members, depth + 1)
            # A variable index (rw_sem + level) is added in a register, as for o->slots[k]; a
            # parameter's (or a parameter less a constant's) steps are a term a caller can fix.
            indexed = self.index_of(right)
            steps = sign * indexed[1] if indexed is not None else 0
            stepped = self.pointer(pointer, bits + steps * step, members, depth + 1)
            if stepped.base == "unknown":
                return stepped
            return _indexed(replace(stepped, element=bits), indexed, sign * step)
        if node.kind == K.CONDITIONAL_OPERATOR and len(children) == 3:
            # to_software_node(fwnode): is_software_node(fwnode) ? container_of(fwnode, ...) : NULL.
            # A null arm is never dereferenced; arms that point the same way give that address.
            arms = [arm for arm in children[1:] if not _is_null(arm)]
            agreed = _agree({self.pointer(arm, bits, members, depth + 1) for arm in arms})
            if agreed is not None and len(arms) < 2 and agreed.base not in _OBJECT_BASES:
                # Where nothing tests the result for null, the compiler keeps the selected value (the
                # container, or NULL) in a register and adds the access's whole offset to it.
                agreed = replace(agreed, inner=bits)
            return agreed if agreed is not None else _Address(bits, "pointer")
        if node.kind == K.GENERIC_SELECTION_EXPR and children:
            # container_of_const(ptr, type, member): the association the control type selects.
            selected = self.selection(node)
            if selected is not None and len(selected) == 1:
                (index,) = selected
                if 0 < index < len(children):
                    return self.pointer(children[index], bits, members, depth + 1)
        if node.kind == K.DECL_REF_EXPR and node.referenced is not None:
            referenced = node.referenced
            if referenced.kind == K.PARM_DECL and not self.written(referenced):
                return _Address(bits, "pointer", self.parameters.get(referenced.hash))
            if referenced.kind in (K.PARM_DECL, K.VAR_DECL) and not _is_global_var(referenced):
                derived = self.derived(referenced, node, depth)
                if derived is not None:
                    return derived.shifted(bits)
            # A parameter the function has set before this use (o = o->next in a walk), or a local
            # whose definitions disagree, holds another pointer.
            return _Address(bits, "pointer")
        if node.kind == K.CALL_EXPR and node.referenced is not None and node.referenced.kind == K.FUNCTION_DECL:
            returned = self.x.returned_address(node.referenced.spelling)
            arguments = list(node.get_arguments() or [])
            found: _Address | None = None
            if returned is not None:
                # The helper's parameter indices, as the call's arguments give them.
                shift, terms = 0, []
                for position, scale, constant in returned.index:
                    value = self.x.evaluator.evaluate(arguments[position]) if position < len(arguments) else None
                    indexed = (
                        self.index_of(arguments[position]) if position < len(arguments) and value is None else None
                    )
                    if value is not None:
                        shift += value * scale
                    elif indexed is not None:
                        shift += indexed[1] * scale
                        terms.append((indexed[0], scale, constant + indexed[1]))
                returned = replace(returned, bits=returned.bits + shift, index=tuple(terms))
            if returned is not None and returned.param is None:
                found = returned.shifted(bits)
            elif returned is not None and returned.param is not None and returned.param < len(arguments):
                found = self.pointer(arguments[returned.param], bits + returned.bits, members, depth + 1)
                if returned.element is not None:
                    # The helper's variable index is nearer the access than any in the argument.
                    found = replace(found, element=returned.element + bits)
                if found.base not in _OBJECT_BASES and returned.base == "container_of":
                    found = replace(found, base="container_of")
                if found.base != "unknown":
                    found = replace(found, index=(*found.index, *returned.index))
            if found is not None:
                definition = self.x.evaluator.definitions.get(node.referenced.spelling)
                nullable = node.referenced.spelling in self.x.nullable_helpers
                if found.base != "unknown" and (nullable or (definition is not None and not _is_inline(definition))):
                    # A helper not declared inline may stay a call, and one that returns NULL or an
                    # address becomes a select (-fno-delete-null-pointer-checks keeps the test): then
                    # its result is a register, and the access adds only its own offset to it.
                    found = replace(found, inner=bits)
                return found
        return _Address(bits, "pointer")

    def arithmetic(self, node: ci.Cursor) -> tuple[ci.Cursor, ci.Cursor, int, int] | None:
        """Pointer arithmetic ``p + i``, ``i + p`` or ``p - i``: the pointer, the integer, the bits one
        step moves and the sign of a step."""
        if node.kind != K.BINARY_OPERATOR or libclang.binary_kind(node) not in (libclang.ADD, libclang.SUBTRACT):
            return None
        operands = _children(node)
        if len(operands) != 2:
            return None
        left, right = operands
        add = libclang.binary_kind(node) == libclang.ADD
        if add and right.type.get_canonical().kind == TK.POINTER:
            left, right = right, left
        if left.type.get_canonical().kind != TK.POINTER or right.type.get_canonical().kind not in _SCALAR_KINDS:
            return None
        return left, right, _element_bytes(left) * 8, 1 if add else -1

    def index_of(self, expression: ci.Cursor) -> tuple[int, int] | None:
        """An index that is a parameter the function never writes, plus a constant (``level``,
        ``k - 1``): the parameter's position and the constant."""
        node = _strip(expression)
        offset = 0
        if node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) in (libclang.ADD, libclang.SUBTRACT):
            operands = _children(node)
            if len(operands) != 2:
                return None
            left, right = operands
            value = self.x.evaluator.evaluate(right)
            if value is not None:
                offset, node = value if libclang.binary_kind(node) == libclang.ADD else -value, _strip(left)
            elif libclang.binary_kind(node) == libclang.ADD and self.x.evaluator.evaluate(left) is not None:
                offset, node = self.x.evaluator.evaluate(left) or 0, _strip(right)
            else:
                return None
        referenced = node.referenced if node.kind == K.DECL_REF_EXPR else None
        if referenced is None or referenced.kind != K.PARM_DECL or self.written(referenced):
            return None
        position = self.parameters.get(referenced.hash)
        return (position, offset) if position is not None else None

    def derived(self, variable: ci.Cursor, use: ci.Cursor, depth: int) -> _Address | None:
        """Where a local or parameter points at ``use`` when every value that can reach it points the
        same way (``d = &desc->irq_data``; list_for_each_entry's ``pos = container_of(..., member)``;
        a parameter is its own value until the function sets it) and nothing writes it otherwise or
        takes its address. When a parameter's own value and the values a walk assigns it disagree
        and those values are all one container_of (an rb_entry walk), the parameter stays the base
        and the machine code may keep the member's address instead. The members of the definitions
        are folded away when every other use of the local is the base of a member access."""
        uses = self.local_uses()
        if uses["written"][variable.hash]:
            return None
        definitions, entry = self.reaching(variable, use)
        key = (variable.hash, tuple(sorted(definition.hash for definition in definitions)), entry)
        if key in self.derived_cache:
            return self.derived_cache[key]
        self.derived_cache[key] = None
        members: list[ci.Cursor] = []
        assigned = {self.pointer(definition, 0, members, depth + 1) for definition in definitions}
        own = _Address(0, "pointer", self.parameters.get(variable.hash)) if entry else None
        address = _agree(assigned | ({own} if own is not None else set()))
        if address is None and own is not None:
            walked = _agree(assigned)
            if walked is not None and walked.base == "container_of":
                address = replace(own, inner=walked.bits)
        if address is None or address.base == "unknown":
            return None
        if uses["references"][variable.hash] - uses["assigned"][variable.hash] == uses["bases"][variable.hash]:
            self.inner.update(member.hash for member in members)
        self.derived_cache[key] = address
        return address

    def reaching(self, variable: ci.Cursor, use: ci.Cursor) -> tuple[list[ci.Cursor], bool]:
        """The values a local or parameter can hold at ``use``, without the null pointers, and whether
        a parameter's own value can: its initializer and the assignments that can reach the use (each
        one before it, or after it in a loop around both, or anywhere when the function jumps back),
        less what an assignment every path to the use passes overwrites."""
        self.local_uses()
        here = _place(use)
        values = [
            (value, place) for value, place in self.definitions.get(variable.hash, []) if self.reaches(place, here)
        ]
        initializer = libclang.var_initializer(variable) if variable.kind == K.VAR_DECL else None
        entry = variable.kind == K.PARM_DECL
        dominating = self.dominating(variable, use)
        if dominating is not None:
            value, keep = dominating
            # Only the assignments that can still come after that one on the way to the use.
            values = [(value, None)] + [
                (other, place) for other, place in values if other.hash != value.hash and keep(other, place)
            ]
            initializer, entry = None, False
        found = [value for value, _ in values] + ([initializer] if initializer is not None else [])
        return [value for value in found if not _is_null(value) and not self.x.returns_null(value)], entry

    def reaches(self, definition: _Place | None, use: _Place | None) -> bool:
        if definition is None or use is None or definition[0] != use[0] or definition[1] <= use[2] or self.backward:
            return True
        return any(loop[0] == use[0] and loop[1] <= use[1] and definition[2] <= loop[2] for loop in self.loops)

    def dominating(
        self, variable: ci.Cursor, use: ci.Cursor
    ) -> tuple[ci.Cursor, Callable[[ci.Cursor, _Place | None], bool]] | None:
        """The last plain assignment to the variable that every path to ``use`` passes, and which of the
        other assignments can still come after it: a statement of a block that holds the use, before
        the statement the use is in, with no label or case from there to the use (then the assignments
        between the two); or a ``for`` header's (``loop_header``)."""
        node = use
        while node.hash != self.cursor.hash:
            parent = self.parents.get(node.hash)
            if parent is None or node.hash in self.label_holders:
                return None
            if parent.kind == K.FOR_STMT:
                found = self.loop_header(variable, parent, node)
                if found is not None:
                    return found
            if parent.kind == K.COMPOUND_STMT:
                siblings = _children(parent)
                index = next((at for at, sibling in enumerate(siblings) if sibling.hash == node.hash), None)
                for sibling in reversed(siblings[:index] if index is not None else []):
                    if sibling.hash in self.label_holders:
                        return None
                    if self.assigns(sibling, variable):
                        after, until = _place(sibling), _place(node)
                        if after is None or until is None or after[0] != until[0]:
                            return None
                        start: _Place = after
                        end: _Place = until

                        def between(
                            _: ci.Cursor, place: _Place | None, start: _Place = start, end: _Place = end
                        ) -> bool:
                            return place is not None and place[0] == start[0] and start[2] < place[1] <= end[2]

                        return _children(sibling)[1], between
            node = parent
        return None

    def assigns(self, node: ci.Cursor, variable: ci.Cursor) -> bool:
        """``node`` is a plain assignment to the variable."""
        operands = _children(node)
        if node.kind != K.BINARY_OPERATOR or libclang.binary_kind(node) != 22 or len(operands) != 2:
            return False
        target = _strip(operands[0])
        return (
            target.kind == K.DECL_REF_EXPR and target.referenced is not None and target.referenced.hash == variable.hash
        )

    def loop_header(
        self, variable: ci.Cursor, loop: ci.Cursor, node: ci.Cursor
    ) -> tuple[ci.Cursor, Callable[[ci.Cursor, _Place | None], bool]] | None:
        """An assignment of a ``for`` header that every path to ``node`` (the body, or a part of the
        header) passes. In the body: one the condition makes whenever it holds (for_each_process's
        ``(p = next_task(p)) != &init_task``, hlist_bl's ``pos && ({ tpos = ...; 1; })``), after which
        only the body's assignments come. In the body, the condition or the step: the init's
        (list_for_each_entry's ``pos = list_first_entry(...)``), after which only the loop's come. None
        when a label inside the loop lets control enter it past the init. libclang leaves absent parts
        of the header out, so a part is told by its shape when not all three are there."""
        if loop.hash in self.label_holders:
            return None
        parts = _children(loop)
        body, header = parts[-1], parts[:-1]
        roles = self.header_roles(header)
        if roles is None:
            return None
        role = {part.hash: name for part, name in zip(header, roles, strict=True)}
        where = "body" if node.hash == body.hash else role.get(node.hash)
        condition = next((part for part in header if role[part.hash] == "condition"), None)
        initial = next((part for part in header if role[part.hash] == "init"), None)
        if where == "body" and condition is not None:
            found = self.assigned_when_true(condition, variable)
            if found:
                return _children(found[-1])[1], lambda other, _: self.within(other, body)
        if where in ("body", "condition", "step") and initial is not None:
            found = self.assigned_at_top(initial, variable)
            if found:
                return _children(found[-1])[1], lambda other, _: self.within(other, loop)
        return None

    @staticmethod
    def header_roles(header: list[ci.Cursor]) -> list[str] | None:
        if len(header) == 3:
            return ["init", "condition", "step"]
        shapes = [
            _bare(part).kind == K.DECL_STMT
            or (_bare(part).kind == K.BINARY_OPERATOR and libclang.binary_kind(_bare(part)) in (22, _COMMA))
            for part in header
        ]
        if len(header) == 2:
            return {
                (True, False): ["init", "condition"],
                (False, True): ["condition", "step"],
                (True, True): ["init", "step"],
            }.get((shapes[0], shapes[1]))
        if len(header) == 1 and not shapes[0]:
            return ["condition"]
        return None

    def assigned_at_top(self, expression: ci.Cursor, variable: ci.Cursor) -> list[ci.Cursor]:
        """The assignments to the variable an expression always makes: itself, or a comma's operands."""
        node = _bare(expression)
        if node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == _COMMA:
            left, right = _children(node)
            return self.assigned_at_top(left, variable) + self.assigned_at_top(right, variable)
        return [node] if self.assigns(node, variable) else []

    def assigned_when_true(self, condition: ci.Cursor, variable: ci.Cursor) -> list[ci.Cursor]:
        """The assignments to the variable a condition makes whenever it holds: at its top, in either
        side of a comparison or ``&&``, the left of ``||``, or a statement of ``({ ... })``."""
        found = self.assigned_at_top(condition, variable)
        if found:
            return found
        node = _bare(condition)
        operands = _children(node)
        if node.kind == K.BINARY_OPERATOR and len(operands) == 2:
            operator = libclang.binary_kind(node)
            # CXBinaryOperatorKind LT 11 ... NE 16 (Cmp, <=>, is 10).
            if operator == libclang.LOGICAL_AND or 11 <= operator <= 16:
                return self.assigned_when_true(operands[0], variable) + self.assigned_when_true(operands[1], variable)
            if operator == libclang.LOGICAL_OR:
                return self.assigned_when_true(operands[0], variable)
        if node.kind == K.StmtExpr and operands and operands[0].kind == K.COMPOUND_STMT:
            return [
                assignment
                for statement in _children(operands[0])
                if statement.kind.is_expression()
                for assignment in self.assigned_when_true(statement, variable)
            ]
        return []

    def within(self, node: ci.Cursor, scope: ci.Cursor) -> bool:
        """``node`` is inside ``scope`` (by the walk's parents)."""
        current: ci.Cursor | None = node
        while current is not None:
            if current.hash == scope.hash:
                return True
            current = self.parents.get(current.hash)
        return False

    def local_uses(self) -> dict[str, collections.Counter[int]]:
        """Per local or parameter, by its cursor's hash: references, plain assignments to it (each kept
        with where it is), writes the derivation cannot follow (``+=``, ``++``, ``&local`` unless an
        inline callee never reads it), and uses as a member access's base; also each node's parent,
        the nodes that hold a label or a case, the function's loops, and whether it jumps back."""
        if self.uses is not None:
            return self.uses
        uses: dict[str, collections.Counter[int]] = {
            name: collections.Counter() for name in ("references", "assigned", "written", "bases")
        }

        def local(node: ci.Cursor | None) -> int | None:
            node = _strip(node) if node is not None else None
            if node is None or node.kind != K.DECL_REF_EXPR or node.referenced is None:
                return None
            return node.referenced.hash if node.referenced.kind in (K.VAR_DECL, K.PARM_DECL) else None

        stack: list[tuple[ci.Cursor, ci.Cursor | None]] = [(self.cursor, None)]
        while stack:
            node, parent = stack.pop()
            operands = _children(node)
            stack.extend((child, node) for child in reversed(operands))
            if parent is not None:
                self.parents[node.hash] = parent
            kind = node.kind
            if kind == K.DECL_REF_EXPR and node.referenced is not None:
                uses["references"][node.referenced.hash] += 1
                if self.tested(node):
                    uses["bases"][node.referenced.hash] += 1
                continue
            if kind in (K.LABEL_STMT, K.CASE_STMT, K.DEFAULT_STMT):
                above: ci.Cursor | None = node
                while above is not None and above.hash not in self.label_holders:
                    self.label_holders.add(above.hash)
                    above = self.parents.get(above.hash)
            if kind in _LOOPS:
                loop = _place(node)
                if loop is not None:
                    self.loops.append(loop)
            elif kind == K.INDIRECT_GOTO_STMT:
                self.backward = True
            elif kind == K.GOTO_STMT:
                label = next((child.referenced for child in operands if child.kind == K.LABEL_REF), None)
                at, to = _place(node), _place(label) if label is not None else None
                self.backward = self.backward or at is None or to is None or at[0] != to[0] or to[1] <= at[1]
            target = local(operands[0]) if operands else None
            if target is None:
                continue
            if kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == 22 and len(operands) == 2:
                uses["assigned"][target] += 1
                self.definitions[target].append((operands[1], _place(node)))
            elif kind == K.COMPOUND_ASSIGNMENT_OPERATOR or (kind == K.UNARY_OPERATOR and libclang.is_increment(node)):
                uses["written"][target] += 1
            elif kind == K.UNARY_OPERATOR and libclang.is_address_of(node):
                if not self.ignored_address(node):
                    uses["written"][target] += 1
            elif kind == K.MEMBER_REF_EXPR and operands[0].type.get_canonical().kind == TK.POINTER:
                uses["bases"][target] += 1
        self.uses = uses
        return uses

    def variable_cancelled(self, variable: ci.Cursor, bits: int, depth: int = 0) -> bool:
        """Every use of a local or parameter (``_is_local``) takes ``bits`` back off its value with
        container_of (``cancelled``); a store into it is no use. container_of's type check reads its
        pointer only in typeof, which is not computed."""
        if depth > 8 or not _is_local(variable):
            return False
        self.local_uses()
        uses = [
            node
            for node in _effect_nodes(
                self.cursor, lambda node, children: [] if node.kind == K.STATIC_ASSERT else children
            )
            if node.kind == K.DECL_REF_EXPR
            and node.referenced is not None
            and node.referenced.hash == variable.hash
            and not self.stored_into(node)
        ]
        return bool(uses) and all(self.cancelled(node, bits, depth + 1) for node in uses)

    def stored_into(self, reference: ci.Cursor) -> bool:
        """``reference`` is what a plain assignment stores into."""
        parent = self.parents.get(reference.hash)
        operands = _children(parent) if parent is not None else []
        return parent is not None and _is_plain_assignment(parent) and operands[0].hash == reference.hash

    def cancelled(self, node: ci.Cursor, bits: int, depth: int = 0) -> bool:
        """The value of ``node`` is used only to take ``bits`` back off it with container_of
        (``(type *)(__mptr - offsetof(type, member))``): directly, through the locals it initializes,
        or as an argument at a parameter a helper of the unit uses only that way (``cancels``). Then
        the member address of those bits it holds is never formed."""
        self.local_uses()
        child, parent = node, self.parents.get(node.hash)
        while parent is not None and (
            (parent.kind in (K.PAREN_EXPR, K.UNEXPOSED_EXPR) and len(_children(parent)) == 1)
            or (parent.kind == K.CSTYLE_CAST_EXPR and _children(parent)[-1].hash == child.hash)
        ):
            child, parent = parent, self.parents.get(parent.hash)
        if parent is None:
            return False
        operands = _children(parent)
        if parent.kind == K.VAR_DECL:
            initializer = libclang.var_initializer(parent)
            return (
                initializer is not None
                and initializer.hash == child.hash
                and self.variable_cancelled(parent, bits, depth)
            )
        if (
            parent.kind == K.BINARY_OPERATOR
            and libclang.binary_kind(parent) == libclang.SUBTRACT
            and len(operands) == 2
            and operands[0].hash == child.hash
            and child.type.get_canonical().kind == TK.POINTER
        ):
            value = libclang.constant_int(operands[1])
            return value is not None and value * _element_bytes(child) * 8 == bits
        if parent.kind == K.CALL_EXPR and parent.referenced is not None and parent.referenced.kind == K.FUNCTION_DECL:
            arguments = list(parent.get_arguments() or [])
            offset = len(operands) - len(arguments)
            position = (
                next((index for index, operand in enumerate(operands) if operand.hash == child.hash), -1) - offset
            )
            return position >= 0 and self.x.cancels(parent.referenced.spelling, position, bits)
        return False

    def passed_through(self, node: ci.Cursor) -> ci.Cursor:
        """An expression past the helper calls that return their argument unchanged
        (spin_lock_irqsave's spinlock_check(lock)): spin_lock_irqsave(sst->lock_irqsave, ...) names
        the lock by the pointer field it reads, as kfree(p->buf) does."""
        for _ in range(8):
            if node.kind != K.CALL_EXPR or node.referenced is None or node.referenced.kind != K.FUNCTION_DECL:
                return node
            returned = self.x.returned_address(node.referenced.spelling)
            arguments = list(node.get_arguments() or [])
            if returned is None or returned.param is None or returned.bits or returned.param >= len(arguments):
                return node
            if returned.base != "pointer":
                return node
            node = _strip(arguments[returned.param])
        return node

    def tested(self, reference: ci.Cursor) -> bool:
        """A pointer only tested for null (``for (; se; se = NULL)``, ``if (!p)``, ``p && ...``): the
        machine code needs no address for it when it folds the definition into the accesses."""
        if reference.type.get_canonical().kind != TK.POINTER:
            return False
        operand = reference
        parent = self.parents.get(operand.hash)
        while parent is not None and parent.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR):
            operand, parent = parent, self.parents.get(parent.hash)
        if parent is None:
            return False
        children = _children(parent)
        if parent.kind in (K.IF_STMT, K.WHILE_STMT, K.CONDITIONAL_OPERATOR):
            return bool(children) and children[0].hash == operand.hash
        if parent.kind == K.DO_STMT:
            return bool(children) and children[-1].hash == operand.hash
        if parent.kind == K.FOR_STMT:
            # A bare pointer anywhere but the body of a for is its condition.
            return bool(children) and children[-1].hash != operand.hash
        if parent.kind == K.UNARY_OPERATOR:
            return libclang.unary_kind(parent) == libclang.LOGICAL_NOT
        return parent.kind == K.BINARY_OPERATOR and libclang.binary_kind(parent) in (
            libclang.LOGICAL_AND,
            libclang.LOGICAL_OR,
        )

    def ignored_address(self, address: ci.Cursor) -> bool:
        """``&local`` passed straight to an inline callee that never reads that parameter
        (find_matching_se(&se, &pse), empty without group scheduling): the local stays unwritten."""
        argument = address
        parent = self.parents.get(argument.hash)
        while parent is not None and parent.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR, K.CSTYLE_CAST_EXPR):
            argument, parent = parent, self.parents.get(parent.hash)
        if parent is None or parent.kind != K.CALL_EXPR:
            return False
        arguments = list(parent.get_arguments() or [])
        position = next((at for at, item in enumerate(arguments) if item.hash == argument.hash), None)
        return position is not None and position in self.x.ignored_parameters(parent.referenced)

    def written(self, variable: ci.Cursor) -> bool:
        """The function assigns the variable, changes it in place or takes its address."""
        uses = self.local_uses()
        return bool(uses["assigned"][variable.hash] or uses["written"][variable.hash])

    def argument(self, expression: ci.Cursor) -> dict[str, Any]:
        """What a call argument names: its state unit, the parameter it derives from, and, for an
        address the facts can follow (wb_exit(&bdi->wb), prepare_alloc_pages(..., &ac, ...),
        clk_core_free_parent_map(container_of(ref, ...))), what that address adds to its base and
        what the base is, so a folded callee's field offsets can be rebased onto the caller's. A
        function the argument names (kref_put's release_bdi) is ``function``; for a global, the
        ``lines`` the argument spans, where its own accesses of the global are (link moves them into a
        callee the compiler specialized to that global)."""
        item: dict[str, Any] = {
            "unit": self.held_global(expression) or self.x.unit_from_ref(expression) or self.global_storage(expression),
            "param": self.parameter_root(expression),
        }
        if item["unit"] is not None and item["unit"]["kind"] == "global":
            item["lines"] = [expression.extent.start.line, expression.extent.end.line]
        named = _strip(expression)
        if named.kind == K.UNARY_OPERATOR and libclang.is_address_of(named) and _children(named):
            named = _strip(_children(named)[0])
        if named.kind == K.DECL_REF_EXPR and named.referenced is not None and named.referenced.kind == K.FUNCTION_DECL:
            item["function"] = named.referenced.spelling
        if expression.type.get_canonical().kind == TK.POINTER:
            members: list[ci.Cursor] = []
            address = self.pointer(expression, 0, members, 0)
            if address.base in ("pointer", "container_of") and (members or address.param is not None or address.bits):
                # The parameter itself or a local equal to it gives 0; a member address its offset. A
                # pointer loaded through a parameter (path->dentry) is a new base and gives none.
                item["displacement"] = address.bits // 8
                item["param"] = address.param
                item["base"] = address.base
            elif address.base in ("local", "other", "global"):
                # The address of an object the caller names: its own local (on the stack or in
                # registers once the callee is inlined), or a global's storage.
                item["displacement"] = address.bits // 8
                item["param"] = None
                item["base"] = address.base
                item.update(_object_keys(address))
            if "displacement" in item and address.element is not None:
                # Past a variable index (&cpu_base->clock_base[idx]): the bits from the indexed element.
                item["element"] = address.element // 8
            if "displacement" in item and address.inner is not None:
                # A register the machine code may hold instead (devres_remove's result, a walk's
                # rb_entry): the bytes from it.
                item["inner"] = address.inner // 8
            if "displacement" in item and address.index:
                # sb->s_writers.rw_sem + level - 1: the steps of parameters a caller may pass constants.
                item["index"] = _index_terms(address)
        # The value the argument passes, where it is a constant or depends on the caller's parameters:
        # the guards of a folded callee are decided by it (link._bind).
        value = self.x.evaluator.expression(expression, self.held)
        if expressions.stated(value):
            item["expr"] = value
        return item

    def held_global(self, expression: ci.Cursor, depth: int = 0) -> Unit | None:
        """The global whose storage an address holds: ``&g``, ``&g.a``, ``&g[i]`` or a global array's
        decay; a call of an inline helper that returns one (NODE_DATA(nid), &contig_page_data
        without NUMA); or a local whose every definition that can reach it holds the same and that
        nothing else writes (free_area_init_node: pgdat = NODE_DATA(nid); change_clocksource:
        tk = &tk_core.shadow_timekeeper)."""
        node = _strip(expression)
        if depth > 8:
            return None
        found = self.global_storage(node)
        if found is not None:
            return found
        variable = node.referenced if node.kind == K.DECL_REF_EXPR else None
        if variable is None or variable.kind != K.VAR_DECL or _is_global_var(variable):
            return None
        if self.local_uses()["written"][variable.hash]:
            return None
        values, _ = self.reaching(variable, node)
        held = [self.held_global(value, depth + 1) for value in values]
        if not held or any(unit is None for unit in held):
            return None
        units = {jsonio.canonical(unit): unit for unit in held if unit is not None}
        return next(iter(units.values())) if len(units) == 1 else None

    def global_storage(self, expression: ci.Cursor, depth: int = 0) -> Unit | None:
        """The global whose storage an address is in: ``&g``, ``&g.a``, ``&g[i]``, a global array's
        decay, or a call of an inline helper every return of which gives one such global
        (NODE_DATA(nid) without NUMA returns &contig_page_data)."""
        node = _strip(expression)
        if node.kind == K.UNARY_OPERATOR and libclang.is_address_of(node) and _children(node):
            return self.x.unit_from_ref(node) if self.x.global_from_ref(_children(node)[0]) is not None else None
        if node.kind == K.DECL_REF_EXPR:
            if _is_global_var(node.referenced) and node.type.get_canonical().kind in _ARRAY_TYPES:
                return self.x.unit_from_ref(node)
            return None
        referenced = node.referenced if node.kind == K.CALL_EXPR else None
        if referenced is None or referenced.kind != K.FUNCTION_DECL or depth > 3:
            return None
        definition = self.x.evaluator.definitions.get(referenced.spelling)
        if definition is None or self.x.evaluator.weak(definition):
            return None
        returned = [
            _children(statement)[0]
            for statement in definition.walk_preorder()
            if statement.kind == K.RETURN_STMT and _children(statement)
        ]
        units = [self.global_storage(value, depth + 1) for value in returned]
        if not units or any(unit is None for unit in units):
            return None
        distinct = {jsonio.canonical(unit): unit for unit in units}
        return next(iter(distinct.values())) if len(distinct) == 1 else None

    def parameter_root(self, expression: ci.Cursor) -> int | None:
        for child in expression.walk_preorder():
            if child.kind == K.DECL_REF_EXPR and child.referenced is not None:
                referenced = child.referenced
                if referenced.kind == K.PARM_DECL:
                    return self.parameters.get(referenced.hash)
                if referenced.kind == K.VAR_DECL:
                    return None
        return None

    def add_access(
        self, unit: Unit, action: str, where: ci.Cursor, reason: str | None, named: ci.Cursor | None = None
    ) -> None:
        """``where`` locates the access; ``named`` is the expression that names the state, when it
        is not ``where`` itself (the argument of a call that frees or locks it)."""
        provenance = [self.x.location(where)]
        where = named if named is not None else where
        generic = bool(unit.pop("generic", False))
        if reason is not None:
            self.events["unevaluated"].append({"unit": unit, "reason": reason, "provenance": provenance})
            return
        item: dict[str, Any] = {"unit": unit, "access": action, "field": unit.get("field"), "provenance": provenance}
        if generic:
            # An association of a _Generic whose selection could not be read: the image decides.
            item["generic"] = True
        if self.conditional and unit["kind"] == "global":
            item["conditional"] = True
        self.events["accesses"].append(item)
        if unit["kind"] == "struct" and unit.get("field") is not None:
            known = self.field_address.get(where.hash)
            member = self.passed_through(_strip(where))
            if known is None:
                # An action's argument (spin_lock(&p->lock), kfree(p->buf)) names its member directly.
                addressed = where.hash in self.addressed
                if member.kind == K.UNARY_OPERATOR and libclang.is_address_of(member) and _children(member):
                    member, addressed = _strip(_children(member)[0]), True
                if member.kind == K.MEMBER_REF_EXPR:
                    address = self.lvalue(member, 0, [], 0)
                    addressed = addressed or (
                        member.hash != where.hash and member.type.get_canonical().kind in _ARRAY_TYPES
                    )
                else:
                    # The lock helpers name a member through an inline call (spin_lock_irqsave's
                    # spinlock_check(&rsp->rss_lock), which returns &lock->rlock): the address, when
                    # it passes the unit's member.
                    passed: list[ci.Cursor] = []
                    address = (
                        self.pointer(member, 0, passed, 0)
                        if member.type.get_canonical().kind == TK.POINTER
                        else _Address(0, "unknown")
                    )
                    if any(item.referenced.spelling == unit["field"] for item in passed):
                        addressed = True
                    else:
                        address = _Address(0, "unknown")
                known = (address, addressed)
            address, addressed = known
            field = member.referenced if member.kind == K.MEMBER_REF_EXPR else None
            size = field.type.get_size() if field is not None else -1
            layout_error = _layout_error(size)
            access = {
                "struct": unit["name"],
                "def": unit["def"],
                "field": unit["field"],
                "offset": unit.get("offset", -1),
                # What the machine code adds to the register the base is in: the offsets along the
                # member chain (c->box.size, s->per_node[node].node, (&p->a)->b), through the locals
                # and inline helpers that derive the base (d = &desc->irq_data), less the member
                # offset a container_of subtracts. The base is a pointer value, a container_of, a
                # global, or an object on the stack (local) or in registers (other).
                "displacement": address.bits // 8 if address.base != "unknown" else -1,
                "base": address.base,
                "size": size,
                "bitfield": bool(field is not None and field.is_bitfield()),
                "provenance": provenance,
            }
            if address.param is not None and address.base in ("pointer", "container_of"):
                # The parameter the base pointer is: a folded callee's offsets rebase onto its argument.
                access["param"] = address.param
            if address.element is not None and address.base != "unknown":
                # Past a variable index the machine code may fold the constant part into the access
                # (o->slots[k].b: lw 0x6c) or keep the element's address in a register and add only
                # this (__next_base's &cpu_base->clock_base[idx], then base->offset).
                access["element_displacement"] = address.element // 8
            if address.inner is not None and address.base != "unknown":
                # The machine code may keep the value of a helper call or of a walk's container_of in
                # a register and add only this.
                access["inner_displacement"] = address.inner // 8
            if address.index and address.base != "unknown":
                # The parameters among the variable indices, which a caller that passes a constant fixes.
                access["index"] = _index_terms(address)
            if address.base == "local":
                # The caller's or the function's own local object, which the frame may hold.
                access.update(_object_keys(address))
            if addressed:
                # &p->m, or an array member decaying to its address: no load or store.
                access["address"] = True
                canonical = field.type.get_canonical() if field is not None else None
                if canonical is not None and canonical.kind in _ARRAY_TYPES:
                    # What the code reads through the decayed address is the first element's bytes.
                    element = canonical.get_array_element_type().get_size()
                    if element > 0:
                        access["element_size"] = element
                if where.hash in self.cancelled_members:
                    # An argument a helper takes back with container_of (is_mounted(&mnt->mnt)): not
                    # formed where the image inlines that helper (link drops it there).
                    access["cancelled_by"] = self.cancelled_members[where.hash]
                if where.hash in self.returned_members:
                    # The member address the function returns (dl_rq_of_se's &rq->dl): not formed
                    # where a caller takes it back with container_of (a call's ``result_cancelled``).
                    access["returned"] = True
            if self.conditional:
                # Under a __builtin_constant_p choice, or an argument a callee the image may inline
                # ignores: compiled only where the image has it.
                access["conditional"] = True
            if layout_error is not None:
                access["layout_error"] = layout_error
            self.fields.append((where.hash, access))

    def mode(self, node: ci.Cursor, parent: ci.Cursor | None, index: int) -> str:
        if parent is None:
            return "read"
        if parent.kind in (K.BINARY_OPERATOR, K.COMPOUND_ASSIGNMENT_OPERATOR) and index == 0:
            return "write" if libclang.is_assignment(parent) else "read"
        if parent.kind == K.UNARY_OPERATOR and libclang.is_increment(parent):
            return "write"
        return "read"

    def call(self, node: ci.Cursor, parent: ci.Cursor | None, reason: str | None, generic: bool = False) -> None:
        referenced = node.referenced
        arguments = list(node.get_arguments() or [])
        head = next(iter(node.get_children()), None)
        if head is None:
            return
        callee_expression = _strip(head)
        if referenced is not None and referenced.kind == K.FUNCTION_DECL:
            # The callee's name is a call, not a use of the function's address.
            self.callees.add(callee_expression.hash)
            self.direct_call(node, parent, referenced.spelling, arguments, reason, generic)
            return
        if callee_expression.kind == K.GENERIC_SELECTION_EXPR:
            # _Generic(x, a: f, b: g)(y) calls the function its selection names (seqlock.h's
            # __seqprop); libclang names no callee for it.
            selected = self.selection(callee_expression) if reason is None else set()
            for index, association in enumerate(_children(callee_expression)[1:], 1):
                name = _strip(association)
                if name.kind != K.DECL_REF_EXPR or name.referenced is None or name.referenced.kind != K.FUNCTION_DECL:
                    continue
                self.callees.add(name.hash)
                if selected is None or index in selected:
                    self.direct_call(
                        node, parent, name.referenced.spelling, arguments, reason, generic or selected is None
                    )
            return
        if reason is not None:
            return
        through = callee_expression.referenced if callee_expression.kind == K.DECL_REF_EXPR else None
        position = self.parameters.get(through.hash) if through is not None else None
        if through is not None and position is not None and not self.written(through):
            # kref_put's release(kref): the function its caller passes is called with these arguments.
            self.events["parameter_calls"].append(
                {
                    "param": position,
                    "arguments": [self.argument(argument) for argument in arguments],
                    "provenance": [self.x.location(node)],
                }
            )
            return
        callee_expression = head
        for child in callee_expression.walk_preorder():
            referenced = child.referenced
            if (
                child.kind == K.MEMBER_REF_EXPR
                and referenced is not None
                and referenced.kind == K.FIELD_DECL
                and _is_function_pointer(referenced.type)
            ):
                owner = _named_struct(referenced.semantic_parent)
                if owner is not None:
                    self.events["indirect"].append(
                        {
                            "ops_struct": owner.spelling,
                            "field": referenced.spelling,
                            "provenance": [self.x.location(node)],
                        }
                    )
                return

    def direct_call(
        self,
        node: ci.Cursor,
        parent: ci.Cursor | None,
        callee: str,
        arguments: list[ci.Cursor],
        reason: str | None,
        generic: bool,
    ) -> None:
        described = [self.argument(argument) for argument in arguments]
        if reason is None and callee.startswith("__builtin_"):
            # A compiler builtin: folded or emitted inline, not a call to a function of the program.
            reason = "builtin"
        if reason is not None:
            self.events["unevaluated"].append({"call": callee, "reason": reason, "provenance": [self.x.location(node)]})
            return
        event: dict[str, Any] = {"callee": callee, "arguments": described, "provenance": [self.x.location(node)]}
        initialized = self.initialized_object(node)
        if initialized is not None:
            # The call's value initializes a struct local (guard = class_X_constructor(p)): a helper
            # that returns its own local by value returns that object.
            event["result_object"] = initialized
        if generic:
            # In a _Generic association whose selection could not be read: the image decides.
            event["generic"] = True
        if self.conditional:
            # Under a __builtin_constant_p choice: the image decides.
            event["conditional"] = True
        if node.hash in self.unused_calls and node.type.get_canonical().kind != TK.VOID:
            # Nothing uses the call's value: a folded helper keeps only what its effects need, the
            # events of its walk with its returns discarded (the function's ``value_unused``).
            event["value_unused"] = True
        returned = self.x.returned_member(callee)
        if returned is not None and self.cancelled(node, returned):
            # The caller takes the member address the helper returns back with container_of
            # (start_dl_timer's rq_of_dl_rq(dl_rq_of_se(dl_se))): folded, it is never formed.
            event["result_cancelled"] = True
        self.events["calls"].append(event)
        membership = self.x.vocabulary.membership.get(callee)
        if membership is not None:
            helpers = [
                next(
                    (
                        child.referenced.spelling
                        for child in argument.walk_preorder()
                        if child.kind == K.CALL_EXPR
                        and child.referenced is not None
                        and child.referenced.kind == K.FUNCTION_DECL
                    ),
                    None,
                )
                for argument in arguments
            ]
            self.events["list_calls"].append(
                {
                    "operation": callee,
                    "membership": membership,
                    "arguments": [item["unit"] for item in described],
                    "argument_helpers": helpers,
                    "provenance": [self.x.location(node)],
                }
            )
        action = self.x.vocabulary.calls.action(callee)
        if action is not None:
            self.action(node, parent, callee, action, arguments)

    def selection(self, node: ci.Cursor) -> set[int] | None:
        """The association a ``_Generic`` selects (see ``selected_associations``), read once per node."""
        key = node.hash
        if key not in self.x.selections:
            self.x.selections[key] = self.x.selected_associations(node, self.cursor)
        return self.x.selections[key]

    def action(
        self, node: ci.Cursor, parent: ci.Cursor | None, callee: str, action: str, arguments: list[ci.Cursor]
    ) -> None:
        rules = self.x.vocabulary.calls
        if action == "allocate" and parent is not None and parent.kind == K.UNEXPOSED_EXPR:
            # An allocation creates an object of the type its result is converted to.
            record, pointer, _ = _canonical_record(parent.type)
            unit = self.x.record_unit(record, None) if record is not None and pointer else None
            if unit is not None:
                self.add_access(unit, action, node, None)
                return
        candidates = arguments
        if (
            action == "free"
            and len(arguments) > 1
            and rules.free_object_after_first is not None
            and rules.free_object_after_first.search(callee)
        ):
            candidates = arguments[1:]
        for argument in candidates:
            unit = (
                self.x.lock_unit_from_ref(argument) or self.x.unit_from_ref(argument)
                if action in ("lock", "unlock")
                else self.x.unit_from_ref(argument)
            )
            if unit is not None:
                self.add_access(unit, action, node, None, argument)
                return
        for argument in candidates:
            parameter = self.parameter_root(argument)
            if parameter is not None:
                self.events["accesses"].append(
                    {
                        "unit": None,
                        "param": parameter,
                        "access": action,
                        "field": None,
                        "provenance": [self.x.location(node)],
                    }
                )
                return

    def reference(
        self, node: ci.Cursor, parent: ci.Cursor | None, index: int, reason: str | None, generic: bool
    ) -> None:
        referenced = node.referenced
        if referenced is None:
            return
        if node.kind == K.DECL_REF_EXPR and referenced.kind == K.FUNCTION_DECL:
            if reason is None and node.hash not in self.callees:
                # A function used as a value: passed, stored, compared or named in an initializer.
                self.events["function_pointers"].append(
                    {"target": referenced.spelling, "provenance": [self.x.location(node)]}
                )
            return
        if node.kind == K.DECL_REF_EXPR and _is_global_var(referenced):
            if parent is not None and parent.kind == K.MEMBER_REF_EXPR:
                return
            unit: Unit | None = self.x.unit_from_ref(node)
            if unit is not None and _is_constant_scalar(referenced):
                # A const scalar with an initializer is a value the compiler folds, not state.
                reason = reason or "constant"
        elif node.kind == K.MEMBER_REF_EXPR and referenced.kind == K.FIELD_DECL and not _is_unnamed_member(referenced):
            unit = self.x.unit_from_ref(node)
        else:
            return
        if unit is None:
            return
        if generic:
            unit["generic"] = True
        self.add_access(unit, self.mode(node, parent, index), node, reason)

    def cleanup(self, node: ci.Cursor, initializer: ci.Cursor | None) -> None:
        """A local with a cleanup calls a destructor when its scope ends.

        A local of a cleanup class (``guard(...)``, ``CLASS(fd_prepare, fdf)``, a local with the
        class's ``cleanup`` attribute; the constructor's own ``class_X_t _t`` is none) calls the class's
        destructor with its address. A class of a named struct is that struct; a lock guard's unnamed
        struct holds what the constructor took, so the destructor's argument stands for the
        constructor's first argument too (the lock it releases). Any other local with a ``cleanup``
        attribute (``__free(x)``, ``LOCK_MOUNT(...)``) calls the function the attribute names with the
        local's address: a struct local is that object, any other holds what its initializer gave.
        """
        pattern = self.x.vocabulary.cleanup_class
        attributed = any(child.kind == K.UNEXPOSED_ATTR for child in node.get_children())
        match = pattern.fullmatch(node.type.spelling) if pattern is not None and attributed else None
        record, pointer, _ = _canonical_record(node.type)
        own = (
            {
                "unit": self.x.record_unit(record, None),
                "param": None,
                "displacement": 0,
                "base": "local",
                "object": self.x.object_name(node),
                "object_size": max(node.type.get_canonical().get_size(), 0),
            }
            if record is not None and not pointer
            else None
        )
        if match is not None:
            destructor = self.x.vocabulary.cleanup_destructor.format(name=match.group(1))
            constructor = initializer
            while constructor is not None and constructor.kind == K.UNEXPOSED_EXPR and len(_children(constructor)) == 1:
                constructor = _children(constructor)[0]
            arguments = (
                [self.argument(argument) for argument in constructor.get_arguments() or []]
                if constructor is not None and constructor.kind == K.CALL_EXPR
                else []
            )
            if own is not None and own["unit"] is not None:
                # A class of a named struct: the destructor reads the local.
                arguments = [own]
            elif own is not None and arguments:
                # A lock guard: its struct, and the lock the constructor took (guard(task_rq_lock)(p)
                # unlocks p's rq through &guard.rf, a field of the local).
                value = {key: item for key, item in arguments[0].items() if key not in _ADDRESS_KEYS}
                arguments = [{**value, **{key: item for key, item in own.items() if key not in ("unit", "param")}}]
        elif attributed:
            function = self.x.cleanup_function(node, self.cursor)
            if function is None:
                return
            destructor = function
            held = self.argument(initializer) if initializer is not None else {"unit": None, "param": None}
            if held["unit"] is None:
                held = {"unit": self.x.record_unit(record, None) if record is not None else None, "param": None}
            arguments = [own if own is not None else held]
        else:
            return
        self.events["calls"].append(
            {
                "callee": destructor,
                "arguments": arguments or [{"unit": None, "param": None}],
                "implied": "cleanup",
                "provenance": [self.x.location(node)],
            }
        )
        if not self.x.function_effect_free(destructor):
            self.effects += 1

    def count_effect(self, node: ci.Cursor, kind: ci.CursorKind) -> None:
        """Count what a node of compiled code does besides computing a value."""
        if kind == K.CALL_EXPR:
            callee = node.referenced
            if callee is None or callee.kind != K.FUNCTION_DECL:
                self.effects += 1
            elif _is_overflow_builtin(callee.spelling):
                # __builtin_mul_overflow(a, b, &r): a store into the third argument.
                arguments = list(node.get_arguments() or [])
                if len(arguments) != 3 or not _address_of_local(arguments[2], None):
                    self.effects += 1
            elif callee.spelling not in _VALUE_BUILTINS and not self.x.function_effect_free(callee.spelling):
                self.effects += 1
        elif kind == K.ASM_STMT:
            if not self.x.asm_removable(node):
                self.effects += 1
        elif kind in _LOOPS:
            self.compiled_loops += 1
            if self.x.loop_may_not_end(node):
                self.effects += 1
        elif kind.is_expression() and node.type.is_volatile_qualified():
            self.effects += 1
        else:
            target = _store_target(node)
            if target is not None:
                variable = _stored_variable(target)
                if variable is None or not _is_local(variable):
                    self.effects += 1

    def walk(
        self, node: ci.Cursor, parent: ci.Cursor | None, index: int, reason: str | None, generic: bool = False
    ) -> None:
        kind = node.kind
        if (
            reason is None
            and parent is not None
            and parent.kind not in _STATEMENT_PARENTS
            and kind.is_expression()
            and node.hash not in self.statement_values
            and _statement_position(parent, index)
        ):
            # An expression statement that is a loop's body or an if's branch (scoped_guard(mutex,
            # &lock) val = ...): its value is discarded, so only its effects are code. A block's
            # statements are discarded where the block is walked.
            self.statement_values.add(node.hash)
            self.discard(node, parent, index, generic)
            return
        children = _children(node)
        if kind in (K.MEMBER_REF_EXPR, K.ARRAY_SUBSCRIPT_EXPR) and reason is None and node.hash not in self.chained:
            # The top of a member chain: its topmost member gets the chain's address.
            members: list[ci.Cursor] = []
            address = self.lvalue(node, 0, members, 0)
            if members:
                self.chained.update(member.hash for member in members)
                self.field_address[members[0].hash] = (address, node.hash in self.addressed)
                self.inner.update(member.hash for member in members[1:])
        elif kind == K.UNARY_OPERATOR and children and libclang.is_address_of(node):
            operand = _strip_parens(children[0])
            self.addressed.add(operand.hash)
            if operand.kind == K.DECL_REF_EXPR and operand.referenced is not None:
                self.address_taken.add(operand.referenced.hash)
        elif kind == K.UNEXPOSED_EXPR and len(children) == 1 and node.type.get_canonical().kind == TK.POINTER:
            operand = _strip_parens(children[0])
            if operand.type.get_canonical().kind in _ARRAY_TYPES:
                # An array member that decays to its address.
                self.addressed.add(operand.hash)
        if reason is None:
            self.count_effect(node, kind)
            if kind in (K.GOTO_STMT, K.ADDR_LABEL_EXPR):
                # A goto in code, or a label whose address is taken (&&label): the label is code.
                self.goto_targets.update(child.spelling for child in children if child.kind == K.LABEL_REF)
                way = (
                    None if self.conditional or kind != K.GOTO_STMT else [self.guard_table[i] for i in self.guard_stack]
                )
                for child in children:
                    if child.kind == K.LABEL_REF:
                        self.goto_guards[child.spelling].append(way)
            elif kind == K.DECL_REF_EXPR and _is_local(node.referenced) and node.hash not in self.stores:
                if self.conditional:
                    self.decided_reads[node.referenced.hash] += 1
                else:
                    self.reads[node.referenced.hash] += 1
                self.read_sites[node.referenced.hash].append(node)
                self.read_guards[node.referenced.hash].append(None if self.conditional else tuple(self.guard_stack))
        stored = _store_target(node)
        if stored is not None and _strip_parens(stored).kind == K.DECL_REF_EXPR:
            # v = e stores into v without reading it. v op= e and v++ read v only to store into it,
            # unless their own value is used (pcpu_dump_alloc_info's while (v /= 10)).
            value_unused = parent is not None and _statement_position(parent, index)
            if value_unused or (kind == K.BINARY_OPERATOR and libclang.is_assignment(node)):
                self.stores.add(_strip_parens(stored).hash)
            variable = _strip_parens(stored).referenced
            statement = parent is not None and _statement_position(parent, index)
            held = False
            if reason is None and statement and variable is not None and _is_plain_assignment(node):
                self.store_sites[variable.hash].append(node)
                held = len(children) == 2
            if (
                reason is None
                and len(children) == 2
                and statement
                and variable is not None
                and (variable.hash in self.dead_variables | self.decided_variables or node.hash in self.overwritten)
            ):
                # A store, as a statement, to a variable nothing reads (local_lock's __tl), or one
                # every read sees stored again first (wait_task_inactive's rq = task_rq(p)): only
                # what the stored value's computation does besides computing it is code. One read
                # only where the image decides is stored only where the image has it.
                self.walk(children[0], node, 0, reason, generic)
                if variable.hash in self.dead_variables or node.hash in self.overwritten:
                    self.discard(children[1], node, 1, generic)
                else:
                    self.conditional += 1
                    self.walk(children[1], node, 1, reason, generic)
                    self.conditional -= 1
                return
            if held and variable is not None:
                # p = &q->m: the address is the local's, formed where a read of it is compiled.
                self.walk(children[0], node, 0, reason, generic)
                self.walk_held(variable, children[1], node, 1, generic)
                return
        if kind == K.CALL_EXPR:
            self.call(node, parent, reason, generic)
        elif kind in (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR):
            self.reference(node, parent, index, reason, generic)
        if kind == K.CALL_EXPR and node.spelling in _IMAGE_DECIDES:
            # __builtin_constant_p(x) and __builtin_object_size(p, type) never evaluate their operands.
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "builtin", generic)
            return
        if kind == K.RETURN_STMT and children and reason is None and self.returns_discarded:
            # The walk of a call whose value nothing uses: the returned value is discarded, so only
            # what computing it does besides is code (rq_clock_task's rq->clock_task goes, its
            # WARN_ON_ONCE stays).
            self.discard(children[0], node, 0, generic)
            return
        if kind == K.RETURN_STMT and children and reason is None:
            returned = _strip(children[0])
            if returned.kind == K.UNARY_OPERATOR and libclang.is_address_of(returned) and _children(returned):
                # The member address the function returns (``returned``).
                self.returned_members.add(_strip(_children(returned)[0]).hash)
        if kind == K.STATIC_ASSERT:
            # container_of's type check, ((type *)0)->member: compiled only for its type.
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "type", generic)
            return
        if kind == K.CSTYLE_CAST_EXPR and children and reason is None and node.type.get_canonical().kind == TK.VOID:
            # (void)x (lockdep_assert_held(__rq_lockp(rq)) without lockdep): only what computing x
            # does besides computing it is code.
            self.discard(children[-1], node, len(children) - 1, generic)
            return
        if kind == K.CXX_UNARY_EXPR:
            tokens = self.x.written_tokens(node, self.cursor)
            unevaluated = "alignof" if any(token in {"_Alignof", "__alignof__"} for token in tokens) else "sizeof"
            # sizeof says more than a discarded value around it: (void)sizeof(x).
            inner_reason = unevaluated if reason in (None, "discarded") else reason
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, inner_reason, generic)
            return
        if kind == K.GENERIC_SELECTION_EXPR and children:
            selected = self.selection(node) if reason is None else set()
            self.walk(children[0], node, 0, reason or "generic_control", generic)
            for child_index, child in enumerate(children[1:], 1):
                if selected is None:
                    self.walk(child, node, child_index, reason, True)
                elif child_index in selected:
                    self.walk(child, node, child_index, reason, generic)
                else:
                    self.walk(child, node, child_index, reason or "generic_unselected", generic)
            return
        if kind == K.VAR_DECL:
            initializer = libclang.var_initializer(node)
            if initializer is not None and _is_static_local(node):
                # string_get_size's static units_str = { units_10, units_2 }: the compiler lays the
                # initial value out as data, an edge of the static local (_global_initializers), not
                # code of the function.
                self.x.static_locals[node.hash] = node
                for child_index, child in enumerate(children):
                    self.walk(child, node, child_index, reason or "static_initializer", generic)
                return
            if reason is None:
                self.cleanup(node, initializer)
            if reason is None and initializer is not None:
                self.store_sites[node.hash].append(node)
            # The initial value of a variable nothing reads is not computed (ns = current_user_ns()
            # passed only to make_kgid, which ignores it), nor one every read sees stored again first.
            dead_store = (
                reason is None
                and initializer is not None
                and (node.hash in self.dead_variables or node.hash in self.overwritten)
            )
            decided = reason is None and initializer is not None and node.hash in self.decided_variables
            for child_index, child in enumerate(children):
                if initializer is not None and child == initializer:
                    if dead_store:
                        self.discard(child, node, child_index, generic)
                    elif decided:
                        # free_area_init_core's nid, read only by zone_init_internals, which ignores it.
                        self.conditional += 1
                        self.walk(child, node, child_index, reason, generic)
                        self.conditional -= 1
                    elif reason is None:
                        self.walk_held(node, child, node, child_index, generic)
                    else:
                        self.walk(child, node, child_index, reason, generic)
                elif child.kind.is_expression():
                    self.walk(child, node, child_index, reason or "type", generic)
            return
        if kind in (K.CSTYLE_CAST_EXPR, K.COMPOUND_LITERAL_EXPR) and children:
            for child_index, child in enumerate(children[:-1]):
                if child.kind.is_expression():
                    self.walk(child, node, child_index, reason or "type", generic)
            self.walk(children[-1], node, len(children) - 1, reason, generic)
            return
        if kind in (K.IF_STMT, K.CONDITIONAL_OPERATOR) and len(children) >= 2 and reason is None:
            condition = children[0]
            if _mentions(condition, _IMAGE_DECIDES):
                # __builtin_constant_p and __builtin_object_size choose a branch only at compile
                # time: both branches are walked, and the machine code decides which calls stay.
                self.walk(condition, node, 0, reason, generic)
                self.conditional += 1
                for child_index, child in enumerate(children[1:], 1):
                    self.walk(child, node, child_index, reason, generic)
                self.conditional -= 1
                return
            value = self.x.evaluator.evaluate(condition, truth=True)
            if value is not None:
                live = 1 if value else 2
                self.walk(condition, node, 0, reason, generic)
                for child_index, child in enumerate(children[1:], 1):
                    self.walk(child, node, child_index, None if child_index == live else "dead_branch", generic)
                return
            if kind == K.IF_STMT or len(children) == 3:
                guard = self.guard_expression(condition)
                if guard is not None:
                    # The branches are compiled where the caller's arguments let the condition be
                    # true, or false: each is under that guard.
                    self.walk(condition, node, 0, reason, generic)
                    for child_index, child in enumerate(children[1:3], 1):
                        self.walk_guarded((guard, child_index == 1), child, node, child_index, reason, generic)
                    return
        if kind == K.BINARY_OPERATOR and len(children) == 2 and reason is None:
            operator = libclang.binary_kind(node)
            if operator in _ZEROING:
                zero = next((i for i in (1, 0) if libclang.constant_int(children[i]) == 0), None)
                if zero is not None:
                    # IS_DAX(inode) without CONFIG_FS_DAX: inode->i_flags & 0. Anything and 0, or
                    # times 0, is 0: the other operand's value is not used.
                    self.walk(children[zero], node, zero, reason, generic)
                    self.discard(children[1 - zero], node, 1 - zero, generic)
                    return
                guarded = next(
                    (
                        (side, guard)
                        for side in (1, 0)
                        if self.x.effect_free(children[1 - side], self.dead_variables)
                        and (guard := self.guard_expression(children[side], truth=False)) is not None
                    ),
                    None,
                )
                if guarded is not None:
                    # __fget_files_rcu's file->f_mode & mask, where fdget_raw passes mask 0: the other
                    # operand, which has no effect, is read only where this one may be nonzero.
                    side, guard = guarded
                    self.walk(children[side], node, side, reason, generic)
                    self.walk_guarded((guard, True), children[1 - side], node, 1 - side, reason, generic)
                    return
            compared = self.guard_expression(node, truth=False) if operator in _EQUALITY else None
            addressed = [i for i in (1, 0) if compared is not None and self.object_operand(children[i])]
            if compared is not None and addressed:
                # pagetable_pte_ctor's mm != &init_mm: the compiler folds it where the caller passes
                # a constant (alloc_pte_late's NULL) or &init_mm itself, and forms &init_mm only where not.
                self.walk(children[1 - addressed[0]], node, 1 - addressed[0], reason, generic)
                self.walk_guarded((compared, None), children[addressed[0]], node, addressed[0], reason, generic)
                return
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
                right = self.x.evaluator.evaluate(children[1], truth=True)
                if (
                    right is not None
                    and bool(right) == (operator == libclang.LOGICAL_OR)
                    and self.x.effect_free(children[1], self.dead_variables)
                ):
                    # a || 1 is 1 and a && 0 is 0: a's value is unused (can_demote's !pgdat ||
                    # !numa_demotion_enabled without NUMA), so only a's effects are code. Where the
                    # right operand has an effect, a decides whether it happens (pagetable_pte_ctor's
                    # mm != &init_mm && !ptlock_init(ptdesc)).
                    self.discard(children[0], node, 0, generic)
                    self.walk(children[1], node, 1, reason, generic)
                    return
                self.walk(children[0], node, 0, reason, generic)
                if _mentions(children[0], _IMAGE_DECIDES):
                    # flush_workqueue's __builtin_constant_p(_wq == system_wq) && _wq == system_wq.
                    self.conditional += 1
                    self.walk(children[1], node, 1, reason, generic)
                    self.conditional -= 1
                    return
                left = self.x.evaluator.evaluate(children[0], truth=True)
                decided = left is not None and (bool(left) == (operator == libclang.LOGICAL_OR))
                if decided:
                    self.walk(children[1], node, 1, "dead_branch", generic)
                else:
                    # The right operand is evaluated only where the left one does not decide.
                    guard = self.guard_expression(children[0])
                    within = (guard, operator == libclang.LOGICAL_AND) if guard is not None else None
                    self.walk_guarded(within, children[1], node, 1, reason, generic)
                return
            if operator == _COMMA:
                # cpu_to_node(cpu) without NUMA: ((void)(cpu), 0). The left value is discarded.
                self.discard(children[0], node, 0, generic)
                self.walk(children[1], node, 1, reason, generic)
                return
        if kind == K.SWITCH_STMT and len(children) == 2 and reason is None:
            # A value clang folds (switch (sizeof(x)), switch (_NSIG_WORDS)). The evaluator's helper
            # results are truth values, which choose an if but not a case.
            value = None if _mentions(children[0], _IMAGE_DECIDES) else libclang.constant_int(children[0])
            body = children[1]
            if value is not None and body.kind == K.COMPOUND_STMT and _switch_labels_top_level(body):
                # switch (_NSIG_WORDS) on rv64: only the cases control reaches from the label the
                # value selects, up to the statement that leaves the switch, are compiled.
                self.walk(children[0], node, 0, reason, generic)
                statements = _children(body)
                labels = [_case_values(statement) for statement in statements]
                matched = any(value in values for values in labels if values is not None)
                live = False
                for child_index, statement in enumerate(statements):
                    values = labels[child_index]
                    if values is not None and (value in values or (not matched and None in values)):
                        live = True
                    self.walk(statement, body, child_index, None if live else "dead_branch", generic)
                    if live and self.x.evaluator.exits(statement):
                        live = False
                return
            if value is None and not _mentions(children[0], _IMAGE_DECIDES):
                # A selector on the parameters: each statement of the body is under the cases that reach it.
                self.switch_guards(children[0], body)
        if kind == K.COMPOUND_STMT and reason is None:
            # Statements after a return, a goto, a break, a continue or a noreturn call are reached
            # only through a label or a case. After an `if (__builtin_constant_p(...)) return ...;`
            # the machine code decides whether the rest was compiled.
            dead = False
            builtin_exits = 0
            exit_guards = 0  # guards on the rest of the block, from `if (C) return;` before it
            value_last = parent is not None and parent.kind == K.StmtExpr
            for child_index, child in enumerate(children):
                # The ways control falls in here: under this block's guards, or none after an exit.
                fallthrough = None if dead else [self.guard_table[i] for i in self.guard_stack]
                if child.kind == K.LABEL_STMT:
                    # A label is code when control falls into it or a goto in code targets it.
                    dead = dead and child.spelling not in self.live_labels
                elif child.kind in _ENTRIES:
                    dead = False
                if exit_guards and (child.kind in _ENTRIES or self.has_label(child)):
                    # Control enters here through a label, past the conditions before it.
                    del self.guard_stack[len(self.guard_stack) - exit_guards :]
                    exit_guards = 0
                if child.kind == K.LABEL_STMT and not dead:
                    # Only forward gotos and falling in reach it: its code is under their guards, unless
                    # another label inside lets control in another way.
                    body = _children(child)
                    reach = None if body and self.has_label(body[-1]) else self.label_guard(child, fallthrough)
                    if reach is not None:
                        self.push_guard(reach, True)
                        exit_guards += 1
                case_guard = self.case_guards.get(child.hash)
                if dead:
                    self.walk(child, node, child_index, "after_exit", generic)
                elif (
                    child.kind.is_expression() and not (value_last and child_index == len(children) - 1)
                ) or self.statement_without_effect(child, node, child_index):
                    # An expression statement (get_user_ns(ns);, a call of a void function that only
                    # reads), or a statement without effect (statement_without_effect): its value is
                    # discarded, so only its effects are code. The last statement of ({ ... }) is
                    # that expression's value (per_cpu_ptr).
                    pushed = case_guard is not None and case_guard[0] is not None and not self.has_label(child)
                    if pushed:
                        assert case_guard is not None and case_guard[0] is not None
                        self.push_guard(case_guard[0], case_guard[1])
                    try:
                        self.discard(child, node, child_index, generic)
                    finally:
                        if pushed:
                            self.guard_stack.pop()
                elif case_guard is not None:
                    self.walk_guarded(case_guard, child, node, child_index, None, generic)
                else:
                    self.walk(child, node, child_index, None, generic)
                if not dead and self.x.evaluator.exits(child):
                    dead = True
                elif not dead and _exits_under_builtin(child, self.x.evaluator):
                    self.conditional += 1
                    builtin_exits += 1
                elif not dead and (after := self.exit_guard(child)) is not None:
                    self.push_guard(*after)
                    exit_guards += 1
            del self.guard_stack[len(self.guard_stack) - exit_guards :]
            self.conditional -= builtin_exits
            return
        if kind == K.CALL_EXPR and children and reason in (None, "discarded"):
            ignored = self.x.ignored_parameters(node.referenced)
            inline = node.referenced is not None and self.x.inline_function(node.referenced.spelling)
            arguments = list(node.get_arguments() or [])
            # Arguments past the named parameters of a variadic callee that never reads them
            # (sysfs_emit without CONFIG_SYSFS) are ignored too.
            named = len(list(node.referenced.get_arguments() or [])) if node.referenced is not None else 0
            variadic = self.x.ignores_variadic(node.referenced)
            # The children are the callee, then the arguments in order: libclang's cursors for one
            # argument from get_arguments and from the children do not compare equal.
            offset = len(children) - len(arguments)
            for child_index, child in enumerate(children):
                position = child_index - offset if child_index >= offset else None
                if position is not None and reason is None and node.referenced is not None:
                    self.mark_cancelled(child, node.referenced.spelling, position)
                if position is None or not (position in ignored or (variadic and position >= named)):
                    self.walk(child, node, child_index, reason, generic)
                elif inline:
                    # A value only a parameter the inline callee never reads receives: only its
                    # effects are computed.
                    self.discard(child, node, child_index, generic, "ignored_argument")
                elif reason is None and self.x.effect_free(child):
                    # A callee that is not inline drops the value where the compiler inlines it, or
                    # removes the parameter when it is static (device_remove_file(dev,
                    # &dev_attr_dev), mm_init(mm, current, current_user_ns())): the image decides.
                    self.conditional += 1
                    self.walk(child, node, child_index, reason, generic)
                    self.conditional -= 1
                else:
                    self.walk(child, node, child_index, reason, generic)
            return
        for child_index, child in enumerate(children):
            self.walk(child, node, child_index, reason, generic)

    def mark_cancelled(self, argument: ci.Cursor, callee: str, position: int) -> None:
        """An argument ``&p->m`` at a parameter the callee takes back with container_of
        (``cancels``): the member's record names the callee (``cancelled_by``)."""
        node = _strip(argument)
        if node.kind != K.UNARY_OPERATOR or not libclang.is_address_of(node) or not _children(node):
            return
        member = _strip(_children(node)[0])
        bits = _member_bits(member)
        if bits is not None and self.x.cancels(callee, position, bits):
            self.cancelled_members[member.hash] = callee

    def returned_object(self) -> str | None:
        """The struct local every return of the function returns by value (``return _t;``)."""
        names = set()
        for child in self.cursor.walk_preorder():
            if child.kind == K.RETURN_STMT:
                values = _children(child)
                value = _strip(values[0]) if values else None
                variable = value.referenced if value is not None and value.kind == K.DECL_REF_EXPR else None
                if variable is None or variable.kind != K.VAR_DECL or _is_global_var(variable):
                    return None
                if variable.type.get_canonical().kind != TK.RECORD:
                    return None
                names.add(self.x.object_name(variable))
        return names.pop() if len(names) == 1 else None

    def initialized_object(self, call: ci.Cursor) -> str | None:
        """The struct local a call's value initializes or is assigned to, if any."""
        self.local_uses()
        parent = self.parents.get(call.hash)
        while parent is not None and parent.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR, K.CSTYLE_CAST_EXPR):
            parent = self.parents.get(parent.hash)
        target = None
        if parent is not None and parent.kind == K.VAR_DECL:
            target = parent
        elif parent is not None and parent.kind == K.BINARY_OPERATOR and libclang.binary_kind(parent) == 22:
            left = _strip(_children(parent)[0])
            target = left.referenced if left.kind == K.DECL_REF_EXPR else None
        if target is None or target.kind != K.VAR_DECL or _is_global_var(target):
            return None
        return self.x.object_name(target) if target.type.get_canonical().kind == TK.RECORD else None

    def returns(self) -> Unit | None:
        """The state a helper returns an address inside, such as one bucket of a hash table."""
        for child in self.cursor.walk_preorder():
            if child.kind == K.RETURN_STMT:
                unit = self.x.unit_from_ref(child)
                if unit is not None:
                    return unit
        return None


def _unused_callees(walk: _FunctionWalk, definitions: dict[str, ci.Cursor]) -> list[str]:
    """The functions of the unit a walk calls where nothing uses the call's value."""
    return sorted(
        {call["callee"] for call in walk.events["calls"] if call.get("value_unused") and call["callee"] in definitions}
    )


def _definitions(tu: ci.TranslationUnit) -> dict[str, ci.Cursor]:
    found: dict[str, ci.Cursor] = {}
    for cursor in tu.cursor.get_children():
        if cursor.kind == K.FUNCTION_DECL and cursor.is_definition() and cursor.spelling:
            found.setdefault(cursor.spelling, cursor)
    return found


def _main_file(cursor: ci.Cursor, main: Path) -> bool:
    location = cursor.location
    return location.file is not None and Path(location.file.name).resolve() == main


def _owned(extractor: _Extractor, cursor: ci.Cursor, main: Path) -> bool:
    """A definition the unit compiles as its own function: one of its file or of a C file it includes,
    or one with external linkage that an included header gives. Such a header is the unit's own
    fragment (kernel/rcu/tree.c includes tree_stall.h): an external definition two units included
    would be defined twice. An inline definition is every includer's, and emits no definition."""
    if _main_file(cursor, main) or extractor.relative(cursor.location.file.name).endswith(".c"):
        return True
    return bool(cursor.linkage == ci.LinkageKind.EXTERNAL and not _is_inline(cursor))


def _initializer_functions(tu: ci.TranslationUnit, definitions: dict[str, ci.Cursor]) -> set[str]:
    """The defined functions a file-scope initializer names: a table of operations, an initcall."""
    found: set[str] = set()
    for cursor in tu.cursor.get_children():
        if cursor.kind != K.VAR_DECL:
            continue
        initializer = libclang.var_initializer(cursor)
        if initializer is None:
            continue
        for child in initializer.walk_preorder():
            referenced = child.referenced
            if (
                child.kind == K.DECL_REF_EXPR
                and referenced is not None
                and referenced.kind == K.FUNCTION_DECL
                and referenced.spelling in definitions
            ):
                found.add(referenced.spelling)
    return found


def _designation(child: ci.Cursor) -> tuple[list[ci.Cursor], ci.Cursor]:
    """The designators of one element of an initializer list (``.a.b``, ``[2]``) and its value.
    libclang gives a designated element as an expression of type void whose last child is the value."""
    children = _children(child)
    if child.kind == K.UNEXPOSED_EXPR and child.type.kind == TK.VOID and len(children) >= 2:
        return children[:-1], children[-1]
    return [], child


def _initializer_values(
    node: ci.Cursor, typ: ci.Type, bits: int, top: str | None
) -> list[tuple[str | None, int, ci.Cursor]]:
    """Each scalar value of an initializer, with the outermost field it initializes and its offset
    in bits from the start of the object."""
    if node.kind != K.INIT_LIST_EXPR:
        return [(top, bits, node)]
    canonical = typ.get_canonical()
    record = canonical.kind == TK.RECORD
    fields = [field for field in canonical.get_fields() if field.spelling or not field.is_bitfield()] if record else []
    element = canonical.get_array_element_type().get_canonical() if canonical.kind in _ARRAY_TYPES else None
    values: list[tuple[str | None, int, ci.Cursor]] = []
    position = 0
    for child in _children(node):
        designators, value = _designation(child)
        current, offset, name = canonical, bits, top
        if designators:
            for index, designator in enumerate(designators):
                if designator.kind == K.MEMBER_REF and designator.referenced is not None:
                    field_offset = current.get_offset(designator.spelling)
                    if field_offset < 0:
                        return values
                    if index == 0 and record:
                        # The next positional element follows this field (or none, for an indirect one).
                        found = next((i for i, f in enumerate(fields) if f == designator.referenced), len(fields))
                        position = found + 1
                    offset, name = offset + field_offset, name or designator.spelling
                    current = designator.referenced.type.get_canonical()
                else:
                    step = libclang.constant_int(designator)
                    inner = current.get_array_element_type().get_canonical() if current.kind in _ARRAY_TYPES else None
                    if step is None or inner is None or inner.get_size() <= 0:
                        return values
                    if index == 0:
                        position = step + 1
                    offset, current = offset + step * inner.get_size() * 8, inner
        elif record:
            if position >= len(fields) or (position > 0 and canonical.get_declaration().kind == K.UNION_DECL):
                return values
            field = fields[position]
            position += 1
            field_offset = field.get_field_offsetof()
            if field_offset < 0:
                return values
            offset, name, current = bits + field_offset, top or field.spelling or None, field.type.get_canonical()
        elif element is not None and element.get_size() > 0:
            offset, current = bits + position * element.get_size() * 8, element
            position += 1
        else:
            return values
        if value.kind != K.INIT_LIST_EXPR and current.kind in (TK.RECORD, *_ARRAY_TYPES):
            if _strip(value).type.get_canonical() != current:
                # Braces elided around an aggregate: the positions no longer follow the fields.
                return values
            continue
        values.extend(_initializer_values(value, current, offset, name))
    return values


def _global_initializers(extractor: _Extractor, tu: ci.TranslationUnit) -> list[dict[str, Any]]:
    """The addresses of globals a global's initializer stores (arch/riscv/kernel/vdso.c:
    ``vdso_info.vdso_code_start = vdso_start``), for file-scope globals and for the static locals of
    the functions walked (string_get_size's ``units_str = { units_10, units_2 }``). Where the program
    never writes the holder after boot, the compiler and the linker put those addresses into the
    functions that read it."""
    items = []
    for cursor in [*tu.cursor.get_children(), *extractor.static_locals.values()]:
        if cursor.kind != K.VAR_DECL or not cursor.is_definition() or not _is_global_var(cursor):
            continue
        initializer = libclang.var_initializer(cursor)
        if initializer is None:
            continue
        holder = extractor.global_unit(cursor)
        for field, bits, value in _initializer_values(initializer, cursor.type, 0, None):
            found = _static_address(value)
            if found is None:
                continue
            target, target_bits = found
            items.append(
                {
                    "global": holder,
                    "field": field,
                    "offset": bits // 8,
                    "target": extractor.global_unit(target),
                    "target_offset": target_bits // 8,
                    "provenance": [extractor.location(value)],
                }
            )
    return sorted({jsonio.canonical(item): item for item in items}.values(), key=jsonio.canonical)


def _tables(
    extractor: _Extractor, tu: ci.TranslationUnit, main: Path, exclusions: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    tables = []
    for cursor in tu.cursor.get_children():
        if cursor.kind != K.VAR_DECL or not cursor.spelling or not _main_file(cursor, main):
            continue
        initializer = libclang.var_initializer(cursor)
        record, pointer, _ = _canonical_record(cursor.type)
        if initializer is None or record is None or pointer:
            continue
        functions = {
            child.referenced.spelling
            for child in initializer.walk_preorder()
            if child.kind == K.DECL_REF_EXPR
            and child.referenced is not None
            and child.referenced.kind == K.FUNCTION_DECL
        }
        if not functions:
            continue
        try:
            text = " ".join(token.spelling for token in tu.get_tokens(extent=cursor.extent))
        except RuntimeError:
            exclusions.append(
                {
                    "kind": "callback_table",
                    "reason": "tokens_unavailable",
                    "function": None,
                    "target": cursor.spelling,
                    "detail": {"table": cursor.spelling, "ops_struct": record.spelling},
                    "provenance": [extractor.location(cursor)],
                }
            )
            continue
        entries = sorted({(name, target) for name, target in _TABLE_ENTRY.findall(text) if target in functions})
        if entries:
            tables.append(
                {
                    "name": cursor.spelling,
                    "ops_struct": record.spelling,
                    "entries": [{"field": name, "target": target} for name, target in entries],
                    "provenance": [extractor.location(cursor)],
                }
            )
    return sorted(tables, key=lambda table: (table["name"], table["provenance"][0]["line"]))


def _witnesses(source: str, main: Path, vocabulary: Vocabulary) -> list[dict[str, Any]]:
    if not vocabulary.membership_witness_macros:
        return []
    witnesses = []
    for number, line in enumerate(main.read_text(errors="replace").splitlines(), 1):
        macro = next((name for name in vocabulary.membership_witness_macros if name in line), None)
        if macro is not None:
            witnesses.append({"method": macro, "provenance": [{"source": "c", "file": source, "line": number}]})
    return witnesses


# The names a weak attribute is spelled with: __attribute__((weak)) and __attribute__((__weak__)).
_WEAK_ATTRIBUTES = frozenset({"weak", "__weak__"})
_IDENTIFIER_BYTES = re.compile(rb"[A-Za-z_][A-Za-z0-9_]*")


def _parameter_count(cursor: ci.Cursor) -> int:
    """The argument registers a call to the function fills: its parameters, at most eight; all eight
    for a variadic function or one declared without a prototype."""
    typ = cursor.type
    if typ.kind != TK.FUNCTIONPROTO or typ.is_function_variadic():
        return 8
    return min(8, len(list(cursor.get_arguments() or [])))


def _result_registers(cursor: ci.Cursor) -> tuple[int, str | None]:
    """The registers the function returns in, plus a layout error when its result size is unknown."""
    result = cursor.result_type.get_canonical()
    if result.kind == TK.VOID:
        return 0, None
    size = result.get_size()
    layout_error = _layout_error(size)
    return (2 if size < 0 or size > 8 else 1), layout_error


def _is_weak(extractor: _Extractor, cursor: ci.Cursor) -> bool:
    """A definition declared weak, read from the attributes libclang gives the declaration and the
    identifier each is written with. The text of a definition can begin inside a macro of another
    file (kernel/sched/clock.c: ``notrace unsigned long long __weak sched_clock(void)``) or be
    written by a macro (``COND_SYSCALL(x)``); its attributes are the same either way."""
    for child in cursor.get_children():
        if child.kind == K.UNEXPOSED_ATTR:
            name = extractor.attribute_name(child)
            if name is not None and extractor.spells_weak(name):
                return True
    return False


def extract(source: str, c_tree: Path, arguments: list[str], vocabulary: Vocabulary) -> Facts:
    """Parse one C file of the tree and return its facts."""
    # Each analysis nested inside another (up to _EFFECT_DEPTH) recurses through its function's
    # syntax tree: room for that many walks.
    sys.setrecursionlimit(max(sys.getrecursionlimit(), _RECURSION_LIMIT))
    main = (c_tree / source).resolve()
    index = ci.Index.create()
    # The preprocessing record keeps the unit's macro definitions, which _Generic selection and
    # cleanup attributes are read from.
    tu = index.parse(str(main), args=arguments, options=ci.TranslationUnit.PARSE_DETAILED_PROCESSING_RECORD)
    extractor = _Extractor(tu, source, c_tree, vocabulary)
    errors = sorted(
        {
            f"{extractor.relative(diagnostic.location.file.name if diagnostic.location.file else None)}:"
            f"{diagnostic.location.line}: {diagnostic.spelling}"
            for diagnostic in tu.diagnostics
            if diagnostic.severity >= ci.Diagnostic.Error
        }
    )
    includes = sorted({extractor.relative(include.include.name) for include in tu.get_includes()})
    include_probes = _include_probes(tu, main, arguments, extractor)
    definitions = _definitions(tu)
    # The unit's own functions, then every function they or a file-scope initializer reach.
    owned = {name for name, cursor in definitions.items() if _owned(extractor, cursor, main)}
    pending = sorted(owned | _initializer_functions(tu, definitions))
    functions: dict[str, Any] = {}
    exclusions: list[dict[str, Any]] = []
    # The helpers a call whose value nothing uses calls, each given its walk with the returns
    # discarded too (``value_unused``), once every function reached is walked.
    unused: list[str] = []
    unused_walked: set[str] = set()
    while pending or unused:
        if not pending:
            name = unused.pop()
            if name in unused_walked:
                continue
            unused_walked.add(name)
            walk = extractor.analysis(definitions[name], returns_discarded=True)
            for event in walk.events["calls"]:
                event["visible"] = event["callee"] in definitions
            function = functions[name]
            if walk.events != function["events"] or walk.guard_table != function.get("guards", []):
                function["value_unused"] = {
                    "events": walk.events,
                    **({"guards": walk.guard_table} if walk.guard_table else {}),
                }
            reached = {call["callee"] for call in walk.events["calls"]}
            reached |= {pointer["target"] for pointer in walk.events["function_pointers"]}
            pending.extend(sorted(callee for callee in reached if callee in definitions and callee not in functions))
            unused.extend(_unused_callees(walk, definitions))
            continue
        name = pending.pop()
        if name in functions:
            continue
        cursor = definitions[name]
        walk = extractor.analysis(cursor)
        reached = {call["callee"] for call in walk.events["calls"]}
        reached |= {pointer["target"] for pointer in walk.events["function_pointers"]}
        for event in walk.events["calls"]:
            event["visible"] = event["callee"] in definitions
        result_registers, layout_error = _result_registers(cursor)
        if layout_error is not None:
            exclusions.append(
                {
                    "kind": "function",
                    "reason": "result_registers_unknown",
                    "function": name,
                    "target": None,
                    "detail": {"layout_error": layout_error},
                    "provenance": [extractor.location(cursor)],
                }
            )
        functions[name] = {
            "name": name,
            "file": extractor.relative(cursor.location.file.name),
            "line": cursor.location.line,
            "static": cursor.linkage == ci.LinkageKind.INTERNAL,
            "weak": _is_weak(extractor, cursor),
            # Registers a0..a7 carry the first eight integer arguments; a variadic one, or one declared
            # without a prototype, may take all of them.
            "parameters": _parameter_count(cursor),
            "result_registers": result_registers,
            "owned": name in owned,
            "returns": walk.returns(),
            **({"returns_object": walk.returned_object()} if walk.returned_object() is not None else {}),
            "events": walk.events,
            # The conditions on the parameters that events' ``guards`` index (link._bind decides them).
            **({"guards": walk.guard_table} if walk.guard_table else {}),
        }
        pending.extend(sorted(callee for callee in reached if callee in definitions and callee not in functions))
        unused.extend(_unused_callees(walk, definitions))
    records = {}
    pending_records = sorted(extractor.records)
    while pending_records:
        key = pending_records.pop()
        if key in records:
            continue
        records[key] = extractor.record_facts(extractor.records[key])
    tables = _tables(extractor, tu, main, exclusions)
    return {
        "format": FORMAT,
        "source": source,
        "includes": includes,
        "include_probes": include_probes,
        "exclusions": exclusions,
        "errors": errors,
        "functions": dict(sorted(functions.items())),
        "records": dict(sorted(records.items())),
        "tables": tables,
        "global_initializers": _global_initializers(extractor, tu),
        "witnesses": _witnesses(source, main, vocabulary),
        "generic_selections": dict(extractor.generic),
    }
