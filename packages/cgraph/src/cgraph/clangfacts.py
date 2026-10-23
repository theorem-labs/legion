"""One translation unit to facts: plain JSON that depends on that unit's text and nothing else.

A unit's facts hold every function the unit defines in its own file, and every function of an
included header that those functions reach (by a call, an implied cleanup call, or an address). For
each function: its calls, its accesses to state, its list and hash insertions, its calls through a
function-pointer field, the function addresses it passes, and the byte offsets of the fields it
names. The unit also gives the layout of each record those accesses name, its callback tables
(designated initializers of function-pointer fields), and the lines where a membership macro
appears.

What the compiler does not evaluate is not an access: the operand of ``sizeof`` and ``_Alignof``,
the controlling expression and the unselected associations of ``_Generic``, the type expressions
of a declaration or a cast (``typeof``), and the branch of an ``if`` or ``?:`` whose condition clang
folds to a constant. Those references are kept as ``unevaluated`` with the reason.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any, ClassVar

import clang.cindex as ci
from clang.cindex import CursorKind as K
from clang.cindex import TypeKind as TK

from . import libclang
from .config import Vocabulary

FORMAT = "cgraph/tu-facts/v1"
Facts = dict[str, Any]
Unit = dict[str, Any]

_RECORD_KINDS = (K.STRUCT_DECL, K.UNION_DECL)
_TABLE_ENTRY = re.compile(r"\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*&?\s*([A-Za-z_][A-Za-z0-9_]*)")


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


def _container_offset(base: ci.Cursor) -> int | None:
    """The member offset a ``container_of`` subtracts from the pointer it is given, in bytes."""
    for node in base.walk_preorder():
        if node.kind == K.BINARY_OPERATOR and libclang.binary_kind(node) == libclang.SUBTRACT:
            operands = _children(node)
            if len(operands) == 2 and operands[0].type.get_canonical().kind == TK.POINTER:
                value = libclang.constant_int(operands[1])
                if value is not None:
                    return value
    return None


def _displacement(member: ci.Cursor) -> tuple[int, str]:
    """Bytes from the base of a member expression to the field it names, along ``.`` links, and
    what the base is: ``pointer``; ``container_of`` (the displacement already less the member
    offset); ``global`` (the address of a global, folded into a pc-relative address); ``local`` (an
    object on the stack or in registers)."""
    total = 0
    node = member
    while node.kind == K.MEMBER_REF_EXPR:
        field = node.referenced
        record = _named_struct(field.semantic_parent) if field is not None else None
        offset = record.type.get_offset(field.spelling) if record is not None else -1
        if offset < 0:
            return -1, "unknown"
        total += offset
        children = _children(node)
        if not children:
            return total // 8, "pointer"
        base = _strip(children[0])
        through_pointer = children[0].type.get_canonical().kind == TK.POINTER
        if through_pointer:
            if base.kind == K.UNARY_OPERATOR and libclang.is_address_of(base) and _children(base):
                target = _strip(_children(base)[0])
                if target.kind == K.DECL_REF_EXPR and _is_global_var(target.referenced):
                    return total // 8, "global"
            container = _container_offset(base)
            if container is not None:
                return total // 8 - container, "container_of"
            return total // 8, "pointer"
        if base.kind == K.MEMBER_REF_EXPR:
            node = base
            continue
        if base.kind == K.DECL_REF_EXPR and base.referenced is not None:
            referenced = base.referenced
            if referenced.kind == K.PARM_DECL or (referenced.kind == K.VAR_DECL and not _is_global_var(referenced)):
                return total // 8, "local"
            if _is_global_var(referenced):
                return total // 8, "global"
        return total // 8, "other"
    return total // 8, "pointer"


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
        self.evaluator = _Evaluator(
            {cursor.spelling: cursor for cursor in tu.cursor.get_children()
             if cursor.kind == K.FUNCTION_DECL and cursor.is_definition() and cursor.spelling}
        )  # fmt: skip
        self.ignored_cache: dict[str, frozenset[int]] = {}

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
        ``&``; a ``->`` or a ``*`` leaves the global for the object it points to."""
        node = cursor
        while True:
            children = _children(node)
            if node.kind == K.DECL_REF_EXPR:
                return self.global_unit(node.referenced) if _is_global_var(node.referenced) else None
            if node.kind in (K.MEMBER_REF_EXPR, K.ARRAY_SUBSCRIPT_EXPR) and children:
                if children[0].type.get_canonical().kind == TK.POINTER:
                    return None
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
        try:
            size = record.type.get_size()
        except Exception:
            size = -1
        fields = []
        for field in self.record_fields(record):
            child, pointer, array = _canonical_record(field.type)
            # The offset in this record, through any anonymous member on the way.
            offset = record.type.get_offset(field.spelling)
            offset = offset // 8 if offset >= 0 else -1
            try:
                field_size = field.type.get_size()
            except Exception:
                field_size = -1
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
            if child is not None and child.spelling:
                definition = child.get_definition() or child
                entry["record"] = child.spelling
                entry["record_def"] = self.where(definition)
                entry["record_plain"] = self.is_plain(definition) if child.get_definition() is not None else False
            fields.append(entry)
        return {
            "name": record.spelling,
            "def": self.where(record),
            "size": size,
            "plain": self.is_plain(record),
            "fields": fields,
        }

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
        except Exception:
            return []

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
        name what the AST's associations name gives each association its type."""
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
            self.generic["ambiguous"] += 1
            return None
        names = list(next(iter(tables)))
        matches = {index for index, name in enumerate(names, 1) if name in candidates}
        if len(matches) == 1:
            self.generic["selected"] += 1
            return matches
        if not matches and "default" in names:
            self.generic["selected"] += 1
            return {names.index("default") + 1}
        self.generic["ambiguous"] += 1
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
                    return argument
                if token in templates:
                    return templates[token].format(arg=argument)
        return None


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


# Builtins whose answer the compiler gives only while it optimizes: the image decides the branch.
_IMAGE_DECIDES = frozenset({"__builtin_constant_p", "__builtin_object_size", "__builtin_dynamic_object_size"})
_UNREACHABLE = frozenset({"__builtin_unreachable", "__builtin_trap"})
_ENTRIES = (K.LABEL_STMT, K.CASE_STMT, K.DEFAULT_STMT)
_EXITS = (K.RETURN_STMT, K.GOTO_STMT, K.BREAK_STMT, K.CONTINUE_STMT)
_BRANCHES = (K.IF_STMT, K.CONDITIONAL_OPERATOR, K.SWITCH_STMT, K.WHILE_STMT, K.FOR_STMT, K.DO_STMT, K.GOTO_STMT)


def _mentions(cursor: ci.Cursor, names: frozenset[str]) -> bool:
    return any(
        node.kind in (K.CALL_EXPR, K.DECL_REF_EXPR) and node.spelling in names for node in cursor.walk_preorder()
    )


def _is_inline(definition: ci.Cursor) -> bool:
    """A definition written ``inline`` (``static inline``, ``__always_inline``) before its name."""
    try:
        tokens = [token.spelling for token in definition.translation_unit.get_tokens(extent=definition.extent)]
    except Exception:
        return False
    head = tokens[: tokens.index(definition.spelling)] if definition.spelling in tokens else tokens
    return any(token in ("inline", "__inline", "__inline__", "__always_inline") for token in head)


def _exits_under_builtin(statement: ci.Cursor, evaluator: _Evaluator) -> bool:
    """An ``if`` whose condition only the compiler decides (``__builtin_constant_p``) and whose
    branch leaves the block: what follows was compiled only when the condition was false."""
    if statement.kind != K.IF_STMT:
        return False
    parts = _children(statement)
    return len(parts) >= 2 and _mentions(parts[0], _IMAGE_DECIDES) and evaluator.exits(parts[1])


def _pure(expression: ci.Cursor, definitions: dict[str, ci.Cursor], depth: int = 0) -> bool:
    """No assignment, no asm and no call except to an inline helper that is itself pure (get_current()
    returns the thread pointer): dropping the expression changes nothing but its value."""
    for node in expression.walk_preorder():
        if node.kind == K.CALL_EXPR:
            callee = definitions.get(node.spelling)
            if callee is None or depth > 2 or not _is_inline(callee) or not _pure(callee, definitions, depth + 1):
                return False
        elif (
            node.kind in (K.COMPOUND_ASSIGNMENT_OPERATOR, K.ASM_STMT)
            or (node.kind == K.BINARY_OPERATOR and libclang.is_assignment(node))
            or (node.kind == K.UNARY_OPERATOR and libclang.is_increment(node))
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

    def __init__(self, definitions: dict[str, ci.Cursor]) -> None:
        self.definitions = definitions
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
        if definition is None or depth > 3:
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

    def exits(self, statement: ci.Cursor) -> bool:
        """Control does not fall through: a return, goto, break or continue, a block that ends in
        one, an if whose constant-true branch exits, or a noreturn builtin with no branch around it."""
        kind = statement.kind
        if kind in _EXITS:
            return True
        inner = _children(statement)
        if kind == K.COMPOUND_STMT:
            return bool(inner) and self.exits(inner[-1])
        if kind == K.IF_STMT and len(inner) >= 2:
            value = self.evaluate(inner[0])
            if value is not None:
                branch = inner[1] if value else (inner[2] if len(inner) > 2 else None)
                return branch is not None and self.exits(branch)
            return False
        if kind in _BRANCHES:
            return False
        found = False
        for node in statement.walk_preorder():
            if node.kind in _BRANCHES:
                return False
            if node.kind == K.CALL_EXPR and node.spelling in _UNREACHABLE:
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

    def argument(self, expression: ci.Cursor) -> dict[str, Any]:
        return {"unit": self.x.unit_from_ref(expression), "param": self.parameter_root(expression)}

    def parameter_root(self, expression: ci.Cursor) -> int | None:
        for child in expression.walk_preorder():
            if child.kind == K.DECL_REF_EXPR and child.referenced is not None:
                referenced = child.referenced
                if referenced.kind == K.PARM_DECL:
                    return self.parameters.get(referenced.hash)
                if referenced.kind == K.VAR_DECL:
                    return None
        return None

    def add_access(self, unit: Unit, action: str, where: ci.Cursor, reason: str | None) -> None:
        provenance = [self.x.location(where)]
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
            displacement, base = _displacement(where) if where.kind == K.MEMBER_REF_EXPR else (-1, "unknown")
            field = where.referenced if where.kind == K.MEMBER_REF_EXPR else None
            try:
                size = field.type.get_size() if field is not None else -1
            except Exception:
                size = -1
            self.events["field_accesses"].append(
                {
                    "struct": unit["name"],
                    "def": unit["def"],
                    "field": unit["field"],
                    "offset": unit.get("offset", -1),
                    # What the machine code adds to its base register: the field's offset plus the
                    # offsets of the members a "." chain passes through (c->box.size), less the
                    # member offset a container_of subtracts.
                    "displacement": displacement,
                    "base": base,
                    "size": size,
                    "bitfield": bool(field is not None and field.is_bitfield()),
                    "provenance": provenance,
                }
            )

    def mode(self, node: ci.Cursor, parent: ci.Cursor | None, index: int) -> str:
        if parent is None:
            return "read"
        if parent.kind in (K.BINARY_OPERATOR, K.COMPOUND_ASSIGNMENT_OPERATOR) and index == 0:
            return "write" if libclang.is_assignment(parent) else "read"
        if parent.kind == K.UNARY_OPERATOR and libclang.is_increment(parent):
            return "write"
        return "read"

    def address_taken(self, expression: ci.Cursor) -> list[ci.Cursor]:
        """Functions an expression uses as values; the direct callees of nested calls are not."""
        found: list[ci.Cursor] = []

        def visit(node: ci.Cursor) -> None:
            if node.kind == K.CALL_EXPR:
                for argument in node.get_arguments() or []:
                    visit(argument)
                return
            referenced = node.referenced
            if node.kind == K.DECL_REF_EXPR and referenced is not None and referenced.kind == K.FUNCTION_DECL:
                found.append(node)
            for child in node.get_children():
                visit(child)

        visit(expression)
        return found

    def call(self, node: ci.Cursor, parent: ci.Cursor | None, reason: str | None, generic: bool = False) -> None:
        referenced = node.referenced
        arguments = list(node.get_arguments() or [])
        if referenced is not None and referenced.kind == K.FUNCTION_DECL:
            callee = referenced.spelling
            described = [self.argument(argument) for argument in arguments]
            if reason is None and callee.startswith("__builtin_"):
                # A compiler builtin: folded or emitted inline, not a call to a function of the program.
                reason = "builtin"
            if reason is not None:
                self.events["unevaluated"].append(
                    {"call": callee, "reason": reason, "provenance": [self.x.location(node)]}
                )
                return
            event: dict[str, Any] = {"callee": callee, "arguments": described, "provenance": [self.x.location(node)]}
            if generic:
                # In a _Generic association whose selection could not be read: the image decides.
                event["generic"] = True
            if self.conditional:
                # Under a __builtin_constant_p choice: the image decides.
                event["conditional"] = True
            self.events["calls"].append(event)
            for argument in arguments:
                for reference in self.address_taken(argument):
                    self.events["function_pointers"].append(
                        {"target": reference.referenced.spelling, "provenance": [self.x.location(reference)]}
                    )
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
            return
        if reason is not None:
            return
        callee_expression = next(iter(node.get_children()), None)
        if callee_expression is None:
            return
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

    def action(
        self, node: ci.Cursor, parent: ci.Cursor | None, callee: str, action: str, arguments: list[ci.Cursor]
    ) -> None:
        rules = self.x.vocabulary.calls
        if action == "alloc" and parent is not None and parent.kind == K.UNEXPOSED_EXPR:
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
                self.add_access(unit, action, node, None)
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
        if kind == K.CALL_EXPR:
            self.call(node, parent, reason, generic)
        elif kind in (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR):
            self.reference(node, parent, index, reason, generic)
        children = _children(node)
        if kind == K.CXX_UNARY_EXPR:
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "sizeof", generic)
            return
        if kind == K.GENERIC_SELECTION_EXPR and children:
            selected = self.x.selected_associations(node, self.cursor) if reason is None else set()
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
                left = self.x.evaluator.evaluate(children[0])
                decided = left is not None and (bool(left) == (operator == libclang.LOGICAL_OR))
                self.walk(children[0], node, 0, reason, generic)
                self.walk(children[1], node, 1, "dead_branch" if decided else reason, generic)
                return
        if kind == K.COMPOUND_STMT and reason is None:
            # Statements after a return, a goto, a break, a continue or a noreturn call are reached
            # only through a label or a case. After an `if (__builtin_constant_p(...)) return ...;`
            # the machine code decides whether the rest was compiled.
            dead = False
            builtin_exits = 0
            for child_index, child in enumerate(children):
                if child.kind in _ENTRIES:
                    dead = False
                self.walk(child, node, child_index, "after_exit" if dead else None, generic)
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


def _tables(extractor: _Extractor, tu: ci.TranslationUnit, main: Path) -> list[dict[str, Any]]:
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
        except Exception:
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


_WEAK = frozenset({"__weak", "weak", "__weak__"})


def _parameter_count(cursor: ci.Cursor) -> int:
    """The argument registers a call to the function fills: its parameters, at most eight; all eight
    for a variadic function or one declared without a prototype."""
    typ = cursor.type
    if typ.kind != TK.FUNCTIONPROTO or typ.is_function_variadic():
        return 8
    return min(8, len(list(cursor.get_arguments() or [])))


def _is_weak(extractor: _Extractor, cursor: ci.Cursor) -> bool:
    """A definition marked weak: ``__weak`` or ``__attribute__((weak))`` before its name, written in
    the source or, for a definition a macro generates (``COND_SYSCALL(x)``), in the macros it expands."""
    try:
        tokens = [token.spelling for token in extractor.tu.get_tokens(extent=cursor.extent)]
    except Exception:
        return False
    if cursor.spelling in tokens:
        return any(token in _WEAK for token in tokens[: tokens.index(cursor.spelling)])
    return any(token in _WEAK for _, body in extractor.expansion_bodies(cursor, cursor, limit=16) for token in body)


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
    definitions = _definitions(tu)
    # The unit's own functions: those of its file and of the C files it includes.
    pending = sorted(
        name
        for name, cursor in definitions.items()
        if _main_file(cursor, main) or extractor.relative(cursor.location.file.name).endswith(".c")
    )
    functions: dict[str, Any] = {}
    while pending:
        name = pending.pop()
        if name in functions:
            continue
        cursor = definitions[name]
        walk = _FunctionWalk(extractor, cursor)
        walk.walk(cursor, None, 0, None)
        reached = {call["callee"] for call in walk.events["calls"]}
        reached |= {pointer["target"] for pointer in walk.events["function_pointers"]}
        for event in walk.events["calls"]:
            event["visible"] = event["callee"] in definitions
        functions[name] = {
            "name": name,
            "file": extractor.relative(cursor.location.file.name),
            "line": cursor.location.line,
            "static": cursor.linkage == ci.LinkageKind.INTERNAL,
            "weak": _is_weak(extractor, cursor),
            # Registers a0..a7 carry the first eight integer arguments; a variadic one, or one declared
            # without a prototype, may take all of them.
            "parameters": _parameter_count(cursor),
            "main_file": _main_file(cursor, main),
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
    tables = _tables(extractor, tu, main)
    return {
        "format": FORMAT,
        "source": source,
        "includes": includes,
        "errors": errors,
        "functions": dict(sorted(functions.items())),
        "records": dict(sorted(records.items())),
        "tables": tables,
        "witnesses": _witnesses(source, main, vocabulary),
        "generic_selections": dict(extractor.generic),
    }
