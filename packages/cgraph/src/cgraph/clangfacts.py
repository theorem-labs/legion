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
folds to a constant. Those references are kept as ``unevaluated`` with the reason.
"""

from __future__ import annotations

import collections
import re
from collections.abc import Callable
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, ClassVar

import clang.cindex as ci
from clang.cindex import CursorKind as K
from clang.cindex import TypeKind as TK

from . import jsonio, libclang
from .config import Vocabulary
from .errors import ExtractionError

FORMAT = "cgraph/tu-facts/v1"
Facts = dict[str, Any]
Unit = dict[str, Any]

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


_ARRAY_TYPES = (TK.CONSTANTARRAY, TK.INCOMPLETEARRAY, TK.VARIABLEARRAY)
# CXUnaryOperatorKind Deref.
_DEREF = 6
# Bases that name an object the function itself names; a pointer value (given, loaded, or less a
# container_of offset) is "pointer" or "container_of".
_OBJECT_BASES = ("global", "local", "other", "unknown")


@dataclass(frozen=True)
class _Address:
    """Where an address points: ``bits`` from the value the base names, what that base is, the
    function's parameter it is when it is one, and, past a variable index, the bits from the start
    of that indexed element."""

    bits: int
    base: str
    param: int | None = None
    element: int | None = None

    def shifted(self, bits: int) -> _Address:
        element = self.element + bits if self.element is not None else None
        return replace(self, bits=self.bits + bits, element=element)


def _element_bytes(pointer: ci.Cursor) -> int:
    """The bytes one step of pointer arithmetic moves: 1 for ``void *`` and ``char *``."""
    pointee = pointer.type.get_canonical().get_pointee().get_canonical()
    size = pointee.get_size() if pointee.kind != TK.VOID else 1
    return size if size > 0 else 1


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
        self.ignored_cache: dict[str, frozenset[int]] = {}
        self.returned: dict[str, _Address | None] = {}
        self.texts: dict[Path, bytes] = {}

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
        """Where an inline helper of the unit returns a pointer to, from the parameter it derives it
        from when it has one (``return &desc->irq_data``, ``return container_of(mnt, ...)``), or None
        when its returns disagree. A call the compiler inlines adds those offsets to the argument."""
        if name in self.returned:
            return self.returned[name]
        self.returned[name] = None
        definition = self.evaluator.definitions.get(name)
        if definition is None or not _is_inline(definition) or self.is_weak(definition):
            return None
        walk = _FunctionWalk(self, definition)
        found: set[_Address] = set()
        for node in definition.walk_preorder():
            if node.kind == K.RETURN_STMT:
                values = _children(node)
                if not values:
                    return None
                found.add(walk.pointer(values[0], 0, [], 0))
        if len(found) != 1:
            return None
        (address,) = found
        if address.base in ("unknown", "local"):
            return None
        self.returned[name] = address
        return address

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
        """The global whose own storage an expression names, through ``.``, ``[]`` on an array and
        ``&``, and through ``(&g)->m`` (static_key_enable(&(x)->key) of a global key); any other
        ``->`` or ``*`` leaves the global for the object it points to."""
        node = cursor
        while True:
            children = _children(node)
            if node.kind == K.DECL_REF_EXPR:
                return self.global_unit(node.referenced) if _is_global_var(node.referenced) else None
            if node.kind in (K.MEMBER_REF_EXPR, K.ARRAY_SUBSCRIPT_EXPR) and children:
                if children[0].type.get_canonical().kind == TK.POINTER:
                    base = _strip(children[0])
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
        for child in cursor.walk_preorder():
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
        for child in cursor.walk_preorder():
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
        matches = {index for index, name in enumerate(names, 1) if name in candidates}
        if len(matches) == 1:
            return matches
        if not matches and "default" in names:
            return {names.index("default") + 1}
        return None

    def ignored_parameters(self, callee: ci.Cursor | None) -> frozenset[int]:
        """The parameters an inline function of the unit never reads, by position."""
        if callee is None or callee.kind != K.FUNCTION_DECL:
            return frozenset()
        name = callee.spelling
        if name in self.ignored_cache:
            return self.ignored_cache[name]
        definition = self.evaluator.definitions.get(name)
        result: frozenset[int] = frozenset()
        if definition is not None and definition.is_definition() and _is_inline(definition):
            parameters = list(definition.get_arguments() or [])
            used = {
                node.referenced.hash
                for node in definition.walk_preorder()
                if node.kind == K.DECL_REF_EXPR and node.referenced is not None and node.referenced.kind == K.PARM_DECL
            }
            result = frozenset(position for position, parameter in enumerate(parameters) if parameter.hash not in used)
        self.ignored_cache[name] = result
        return result

    def cleanup_function(self, node: ci.Cursor, bound: ci.Cursor) -> str | None:
        """The function a ``cleanup`` attribute on a local names, read from the declaration as
        written and from the macros it expands (``__free(x)``, ``LOCK_MOUNT(...)``)."""
        templates = self.vocabulary.cleanup_macros
        for parameters, body in self.expansion_bodies(node, bound, limit=32):
            for index, token in enumerate(body[:-3]):
                if body[index + 1] != "(" or body[index + 3] != ")" or not _IDENTIFIER.match(body[index + 2]):
                    continue
                argument = body[index + 2]
                if argument in parameters:
                    continue
                if token in ("cleanup", "__cleanup__"):
                    return self.macro_alias(argument)
                if token in templates:
                    return self.macro_alias(templates[token].format(arg=argument))
        return None

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


def _pure(expression: ci.Cursor, definitions: dict[str, ci.Cursor], depth: int = 0) -> bool:
    """No assignment, no asm, no volatile access and no call except to an inline helper that is
    itself pure (get_current() returns the thread pointer): dropping the expression changes nothing
    but its value."""
    for node in expression.walk_preorder():
        if node.kind == K.CALL_EXPR:
            callee = definitions.get(node.spelling)
            if callee is None or depth > 2 or not _is_inline(callee) or not _pure(callee, definitions, depth + 1):
                return False
        elif (
            node.kind in (K.COMPOUND_ASSIGNMENT_OPERATOR, K.ASM_STMT)
            or (node.kind == K.BINARY_OPERATOR and libclang.is_assignment(node))
            or (node.kind == K.UNARY_OPERATOR and libclang.is_increment(node))
            or (node.kind.is_expression() and node.type.is_volatile_qualified())
        ):
            return False
    return True


class _Evaluator:
    """What the compiler folds that clang's evaluator alone does not: inline helpers that return one
    truth value, locals that keep their initializer, ``x & 0`` and ``x * 0``, ``__builtin_expect``."""

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
        self.helpers: dict[str, int | None] = {}
        self.locals: dict[int, int | None] = {}

    def evaluate(self, cursor: ci.Cursor, depth: int = 0) -> int | None:
        if _mentions(cursor, _IMAGE_DECIDES):
            return None
        value = libclang.constant_int(cursor)
        if value is not None or depth > 6:
            return value
        kind, inner = cursor.kind, _children(cursor)
        if kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR, K.CSTYLE_CAST_EXPR) and inner:
            return self.evaluate(inner[-1], depth)
        if kind == K.UNARY_OPERATOR and inner and libclang.unary_kind(cursor) == libclang.LOGICAL_NOT:
            value = self.evaluate(inner[0], depth)
            return None if value is None else int(not value)
        if kind == K.BINARY_OPERATOR and len(inner) == 2:
            operator = libclang.binary_kind(cursor)
            left = self.evaluate(inner[0], depth)
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
                if left is not None and bool(left) == (operator == libclang.LOGICAL_OR):
                    return int(bool(left))
                right = self.evaluate(inner[1], depth)
                return None if left is None or right is None else int(bool(right))
            right = self.evaluate(inner[1], depth)
            if operator in (3, 17) and (left == 0 or right == 0):
                return 0
            operation = self._OPERATIONS.get(operator)
            return None if left is None or right is None or operation is None else operation(left, right)
        if kind == K.DECL_REF_EXPR and cursor.referenced is not None and cursor.referenced.kind == K.VAR_DECL:
            parent = cursor.referenced.semantic_parent
            if parent is not None and parent.kind == K.FUNCTION_DECL:
                return self.local(cursor.referenced, depth)
            return None
        if kind == K.CALL_EXPR and cursor.referenced is not None:
            if cursor.referenced.spelling == "__builtin_expect":
                arguments = list(cursor.get_arguments() or [])
                return self.evaluate(arguments[0], depth) if arguments else None
            if cursor.referenced.kind == K.FUNCTION_DECL:
                return self.helper(cursor.referenced.spelling, depth)
        return None

    def helper(self, name: str, depth: int) -> int | None:
        """The truth value an inline helper always returns, when every return gives the same one."""
        if name in self.helpers:
            return self.helpers[name]
        self.helpers[name] = None
        definition = self.definitions.get(name)
        if definition is None or depth > 3 or self.weak(definition):
            return None
        values = set()
        for node in definition.walk_preorder():
            if node.kind == K.RETURN_STMT:
                inner = _children(node)
                value = self.evaluate(inner[0], depth + 1) if inner else None
                if value is None:
                    return None
                values.add(bool(value))
        result = int(values.pop()) if len(values) == 1 else None
        self.helpers[name] = result
        return result

    def local(self, variable: ci.Cursor, depth: int) -> int | None:
        """A local's value when its initializer evaluates and nothing writes it or takes its address."""
        key = variable.hash
        if key in self.locals:
            return self.locals[key]
        self.locals[key] = None
        initializer = libclang.var_initializer(variable)
        function = variable.semantic_parent
        if initializer is None or function is None:
            return None
        for node in function.walk_preorder():
            if node.kind not in (K.BINARY_OPERATOR, K.COMPOUND_ASSIGNMENT_OPERATOR, K.UNARY_OPERATOR):
                continue
            operands = _children(node)
            target = operands[0] if operands else None
            while target is not None and target.kind in (K.UNEXPOSED_EXPR, K.PAREN_EXPR) and _children(target):
                target = _children(target)[0]
            if target is None or target.kind != K.DECL_REF_EXPR or target.referenced is None:
                continue
            if target.referenced.hash != key:
                continue
            if (
                node.kind == K.COMPOUND_ASSIGNMENT_OPERATOR
                or (node.kind == K.BINARY_OPERATOR and libclang.is_assignment(node))
                or (node.kind == K.UNARY_OPERATOR and (libclang.is_increment(node) or libclang.is_address_of(node)))
            ):
                return None
        value = self.evaluate(initializer, depth + 1)
        self.locals[key] = value
        return value

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
            value = self.evaluate(inner[0])
            if value is not None:
                branch = inner[1] if value else (inner[2] if len(inner) > 2 else None)
                return branch is not None and self.exits(branch, loop)
            return False
        if kind == K.DO_STMT and len(inner) == 2:
            # BUG(): do { __BUG_FLAGS(0); unreachable(); } while (0).
            return self.evaluate(inner[1]) == 0 and self.exits(inner[0], loop=True)
        if kind in _BRANCHES:
            return False
        found = False
        for node in statement.walk_preorder():
            if _branches_on(node):
                return False
            if node.kind == K.CALL_EXPR and (node.spelling in _UNREACHABLE or _is_noreturn(node.referenced)):
                found = True
        return found


class _FunctionWalk:
    """The facts of one function body."""

    def __init__(self, extractor: _Extractor, cursor: ci.Cursor) -> None:
        self.x = extractor
        self.cursor = cursor
        self.parameters = {parameter.hash: index for index, parameter in enumerate(cursor.get_arguments() or [])}
        self.events: dict[str, list[Any]] = {
            "calls": [],
            "accesses": [],
            "unevaluated": [],
            "list_calls": [],
            "indirect": [],
            "function_pointers": [],
            "field_accesses": [],
        }
        self.conditional = 0  # depth of __builtin_constant_p branches being walked
        self.callees: set[int] = set()  # the hashes of the references that name a direct call's callee
        self.selections: dict[int, set[int] | None] = {}  # each _Generic's selection, by node hash
        # Field addresses: each member chain's topmost member gets the chain's address; the members
        # below it (and the ones a local's definition folds away) are addressed only through it.
        self.chained: set[int] = set()
        self.field_address: dict[int, tuple[_Address, bool]] = {}
        self.inner: set[int] = set()
        self.addressed: set[int] = set()
        self.fields: list[tuple[int, dict[str, Any]]] = []
        self.derived_cache: dict[int, _Address | None] = {}
        self.uses: dict[str, collections.Counter[int]] | None = None
        self.definitions: dict[int, list[ci.Cursor]] = collections.defaultdict(list)

    def finish(self) -> None:
        """The field accesses the machine code addresses on their own."""
        self.events["field_accesses"] = [item for member, item in self.fields if member not in self.inner]

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
            # A variable index is added in a register: the constant part of the address stays in the
            # displacement of an access the chain makes, but not past a value that holds the address.
            element = value * size * 8 if value is not None and size > 0 else 0
            if _strip(array).type.get_canonical().kind in _ARRAY_TYPES:
                found = self.lvalue(array, bits + element, members, depth + 1)
            else:
                found = self.pointer(array, bits + element, members, depth + 1)
            # The nearest variable index decides what a register that holds the element adds.
            return found if value is not None else replace(found, element=bits)
        if node.kind == K.UNARY_OPERATOR and children and libclang.unary_kind(node) == _DEREF:
            return self.pointer(children[0], bits, members, depth + 1)
        if node.kind == K.DECL_REF_EXPR and node.referenced is not None:
            if _is_global_var(node.referenced):
                return _Address(bits, "global")
            if node.referenced.kind in (K.PARM_DECL, K.VAR_DECL):
                return _Address(bits, "local")
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
            found = self.pointer(container[0], bits - container[1] * 8, members, depth + 1)
            return found if found.base in _OBJECT_BASES else replace(found, base="container_of")
        if node.kind == K.DECL_REF_EXPR and node.referenced is not None:
            referenced = node.referenced
            array = referenced.type.get_canonical().kind in _ARRAY_TYPES
            if referenced.kind == K.PARM_DECL:
                return _Address(bits, "pointer", self.parameters.get(referenced.hash))
            if _is_global_var(referenced):
                return _Address(bits, "global" if array else "pointer")
            if referenced.kind == K.VAR_DECL:
                if array:
                    return _Address(bits, "local")
                derived = self.derived(referenced, depth)
                if derived is not None:
                    return derived.shifted(bits)
            return _Address(bits, "pointer")
        if node.kind == K.CALL_EXPR and node.referenced is not None and node.referenced.kind == K.FUNCTION_DECL:
            returned = self.x.returned_address(node.referenced.spelling)
            arguments = list(node.get_arguments() or [])
            if returned is not None and returned.param is None:
                return returned.shifted(bits)
            if returned is not None and returned.param is not None and returned.param < len(arguments):
                found = self.pointer(arguments[returned.param], bits + returned.bits, members, depth + 1)
                if returned.element is not None:
                    # The helper's variable index is nearer the access than any in the argument.
                    found = replace(found, element=returned.element + bits)
                if found.base in _OBJECT_BASES or returned.base != "container_of":
                    return found
                return replace(found, base="container_of")
        return _Address(bits, "pointer")

    def derived(self, variable: ci.Cursor, depth: int) -> _Address | None:
        """Where a local pointer points when every definition of it points the same way (``d =
        &desc->irq_data``; list_for_each_entry's ``pos = container_of(..., member)``) and nothing
        else writes it or takes its address. The members of the definitions are folded away when
        every other use of the local is the base of a member access."""
        key = variable.hash
        if key in self.derived_cache:
            return self.derived_cache[key]
        self.derived_cache[key] = None
        uses = self.local_uses()
        if uses["written"][key]:
            return None
        definitions = list(self.definitions.get(key, []))
        initializer = libclang.var_initializer(variable)
        if initializer is not None:
            definitions.append(initializer)
        members: list[ci.Cursor] = []
        addresses = {self.pointer(definition, 0, members, depth + 1) for definition in definitions}
        if len(addresses) != 1:
            return None
        (address,) = addresses
        if address.base == "unknown":
            return None
        if uses["references"][key] - uses["assigned"][key] == uses["bases"][key]:
            self.inner.update(member.hash for member in members)
        self.derived_cache[key] = address
        return address

    def local_uses(self) -> dict[str, collections.Counter[int]]:
        """Per local, by its cursor's hash: references, plain assignments to it, writes the
        derivation cannot follow (``+=``, ``++``, ``&local``), and uses as a member access's base."""
        if self.uses is not None:
            return self.uses
        uses: dict[str, collections.Counter[int]] = {
            name: collections.Counter() for name in ("references", "assigned", "written", "bases")
        }

        def local(node: ci.Cursor | None) -> int | None:
            node = _strip(node) if node is not None else None
            if node is None or node.kind != K.DECL_REF_EXPR or node.referenced is None:
                return None
            return node.referenced.hash if node.referenced.kind == K.VAR_DECL else None

        for node in self.cursor.walk_preorder():
            kind = node.kind
            if kind == K.DECL_REF_EXPR and node.referenced is not None:
                uses["references"][node.referenced.hash] += 1
                continue
            operands = _children(node)
            target = local(operands[0]) if operands else None
            if target is None:
                continue
            if kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == 22 and len(operands) == 2:
                uses["assigned"][target] += 1
                self.definitions[target].append(operands[1])
            elif kind == K.COMPOUND_ASSIGNMENT_OPERATOR or (
                kind == K.UNARY_OPERATOR and (libclang.is_increment(node) or libclang.is_address_of(node))
            ):
                uses["written"][target] += 1
            elif kind == K.MEMBER_REF_EXPR and operands[0].type.get_canonical().kind == TK.POINTER:
                uses["bases"][target] += 1
        self.uses = uses
        return uses

    def argument(self, expression: ci.Cursor) -> dict[str, Any]:
        """What a call argument names: its state unit, the parameter it derives from, and, for the
        address of a member reached from a pointer (wb_exit(&bdi->wb)), what that address adds to
        the pointer, so a folded callee's field offsets can be rebased onto the caller's."""
        item: dict[str, Any] = {
            "unit": self.held_global(expression) or self.x.unit_from_ref(expression),
            "param": self.parameter_root(expression),
        }
        if expression.type.get_canonical().kind == TK.POINTER:
            members: list[ci.Cursor] = []
            address = self.pointer(expression, 0, members, 0)
            if members and address.base in ("pointer", "container_of"):
                item["displacement"] = address.bits // 8
                item["param"] = address.param
        return item

    def held_global(self, expression: ci.Cursor) -> Unit | None:
        """The global a local pointer holds when every definition of it is the address of the same
        global's storage and nothing else writes it (free_area_init_node: pgdat = NODE_DATA(nid),
        which is &contig_page_data; change_clocksource: tk = &tk_core.shadow_timekeeper)."""
        node = _strip(expression)
        variable = node.referenced if node.kind == K.DECL_REF_EXPR else None
        if variable is None or variable.kind != K.VAR_DECL or _is_global_var(variable):
            return None
        if self.local_uses()["written"][variable.hash]:
            return None
        definitions = list(self.definitions.get(variable.hash, []))
        initializer = libclang.var_initializer(variable)
        if initializer is not None:
            definitions.append(initializer)
        if not definitions or not all(self.global_address(definition) for definition in definitions):
            return None
        units = {jsonio.canonical(unit): unit for unit in map(self.x.unit_from_ref, definitions)}
        return next(iter(units.values())) if len(units) == 1 else None

    def global_address(self, expression: ci.Cursor) -> bool:
        """``&`` of a global's storage (``&g``, ``&g.a``, ``&g[i]``), or a global array's decay."""
        node = _strip(expression)
        if node.kind == K.UNARY_OPERATOR and libclang.is_address_of(node) and _children(node):
            return self.x.global_from_ref(_children(node)[0]) is not None
        return (
            node.kind == K.DECL_REF_EXPR
            and _is_global_var(node.referenced)
            and node.type.get_canonical().kind in _ARRAY_TYPES
        )

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
            member = _strip(where)
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
            if addressed:
                # &p->m, or an array member decaying to its address: no load or store.
                access["address"] = True
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
        if generic:
            # In a _Generic association whose selection could not be read: the image decides.
            event["generic"] = True
        if self.conditional:
            # Under a __builtin_constant_p choice: the image decides.
            event["conditional"] = True
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
        if key not in self.selections:
            self.selections[key] = self.x.selected_associations(node, self.cursor)
        return self.selections[key]

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

        A local of a cleanup class (``guard(...)``) calls the class's destructor with the
        arguments the constructor got: the local holds what the constructor took, such as the
        lock a guard holds. Any other local with a ``cleanup`` attribute (``__free(x)``,
        ``LOCK_MOUNT(...)``) calls the function the attribute names with the local's address, which
        holds what its initializer gave, or an object of the local's type.
        """
        pattern = self.x.vocabulary.cleanup_class
        match = pattern.fullmatch(node.type.spelling) if pattern is not None else None
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
        elif any(child.kind == K.UNEXPOSED_ATTR for child in node.get_children()):
            function = self.x.cleanup_function(node, self.cursor)
            if function is None:
                return
            destructor = function
            held = self.argument(initializer) if initializer is not None else {"unit": None, "param": None}
            if held["unit"] is None:
                record, _, _ = _canonical_record(node.type)
                held = {"unit": self.x.record_unit(record, None) if record is not None else None, "param": None}
            arguments = [held]
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

    def walk(
        self, node: ci.Cursor, parent: ci.Cursor | None, index: int, reason: str | None, generic: bool = False
    ) -> None:
        kind = node.kind
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
            self.addressed.add(_strip_parens(children[0]).hash)
        elif kind == K.UNEXPOSED_EXPR and len(children) == 1 and node.type.get_canonical().kind == TK.POINTER:
            operand = _strip_parens(children[0])
            if operand.type.get_canonical().kind in _ARRAY_TYPES:
                # An array member that decays to its address.
                self.addressed.add(operand.hash)
        if kind == K.CALL_EXPR:
            self.call(node, parent, reason, generic)
        elif kind in (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR):
            self.reference(node, parent, index, reason, generic)
        if kind == K.CALL_EXPR and node.spelling in _IMAGE_DECIDES:
            # __builtin_constant_p(x) and __builtin_object_size(p, type) never evaluate their operands.
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "builtin", generic)
            return
        if kind == K.STATIC_ASSERT:
            # container_of's type check, ((type *)0)->member: compiled only for its type.
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "type", generic)
            return
        if (
            kind == K.CSTYLE_CAST_EXPR
            and children
            and reason is None
            and node.type.get_canonical().kind == TK.VOID
            and _pure(children[-1], {})
        ):
            # (void)x of a value without calls or effects (lockdep_assert_held without lockdep):
            # never read. A discarded call is the call rules' to judge.
            self.walk(children[-1], node, len(children) - 1, "discarded", generic)
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
            if reason is None:
                self.cleanup(node, initializer)
            for child_index, child in enumerate(children):
                if initializer is not None and child == initializer:
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
            value = self.x.evaluator.evaluate(condition)
            if value is not None:
                live = 1 if value else 2
                self.walk(condition, node, 0, reason, generic)
                for child_index, child in enumerate(children[1:], 1):
                    self.walk(child, node, child_index, None if child_index == live else "dead_branch", generic)
                return
        if kind == K.BINARY_OPERATOR and len(children) == 2 and reason is None:
            operator = libclang.binary_kind(node)
            if operator in (libclang.LOGICAL_AND, libclang.LOGICAL_OR):
                self.walk(children[0], node, 0, reason, generic)
                if _mentions(children[0], _IMAGE_DECIDES):
                    # flush_workqueue's __builtin_constant_p(_wq == system_wq) && _wq == system_wq.
                    self.conditional += 1
                    self.walk(children[1], node, 1, reason, generic)
                    self.conditional -= 1
                    return
                left = self.x.evaluator.evaluate(children[0])
                decided = left is not None and (bool(left) == (operator == libclang.LOGICAL_OR))
                self.walk(children[1], node, 1, "dead_branch" if decided else reason, generic)
                return
            if operator == _COMMA and _pure(children[0], {}):
                # cpu_to_node(cpu) without NUMA: ((void)(cpu), 0).
                self.walk(children[0], node, 0, "discarded", generic)
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
        if kind == K.COMPOUND_STMT and reason is None:
            # Statements after a return, a goto, a break, a continue or a noreturn call are reached
            # only through a label or a case. After an `if (__builtin_constant_p(...)) return ...;`
            # the machine code decides whether the rest was compiled.
            dead = False
            builtin_exits = 0
            value_last = parent is not None and parent.kind == K.StmtExpr
            for child_index, child in enumerate(children):
                if child.kind in _ENTRIES:
                    dead = False
                if dead:
                    self.walk(child, node, child_index, "after_exit", generic)
                elif (
                    child.kind.is_expression()
                    and not (value_last and child_index == len(children) - 1)
                    and _pure(child, {})
                ):
                    # An expression statement without calls or effects: its value is discarded. The
                    # last statement of ({ ... }) is that expression's value (per_cpu_ptr).
                    self.walk(child, node, child_index, "discarded", generic)
                else:
                    self.walk(child, node, child_index, None, generic)
                if not dead and self.x.evaluator.exits(child):
                    dead = True
                elif not dead and _exits_under_builtin(child, self.x.evaluator):
                    self.conditional += 1
                    builtin_exits += 1
            self.conditional -= builtin_exits
            return
        if kind == K.CALL_EXPR and children and reason is None:
            ignored = self.x.ignored_parameters(node.referenced)
            arguments = list(node.get_arguments() or [])
            for child_index, child in enumerate(children):
                position = next((place for place, argument in enumerate(arguments) if argument == child), None)
                if position is not None and position in ignored and _pure(child, self.x.evaluator.definitions):
                    # A value only a parameter the inline callee never reads receives: not computed.
                    self.walk(child, node, child_index, "ignored_argument", generic)
                else:
                    self.walk(child, node, child_index, reason, generic)
            return
        for child_index, child in enumerate(children):
            self.walk(child, node, child_index, reason, generic)

    def returns(self) -> Unit | None:
        """The state a helper returns an address inside, such as one bucket of a hash table."""
        for child in self.cursor.walk_preorder():
            if child.kind == K.RETURN_STMT:
                unit = self.x.unit_from_ref(child)
                if unit is not None:
                    return unit
        return None


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
    """The addresses of globals a file-scope global's initializer stores (arch/riscv/kernel/vdso.c:
    ``vdso_info.vdso_code_start = vdso_start``). Where the program never writes the holder after
    boot, the compiler and the linker put those addresses into the functions that read it."""
    items = []
    for cursor in tu.cursor.get_children():
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
    while pending:
        name = pending.pop()
        if name in functions:
            continue
        cursor = definitions[name]
        walk = _FunctionWalk(extractor, cursor)
        walk.walk(cursor, None, 0, None)
        walk.finish()
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
            "events": walk.events,
        }
        pending.extend(sorted(callee for callee in reached if callee in definitions and callee not in functions))
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
