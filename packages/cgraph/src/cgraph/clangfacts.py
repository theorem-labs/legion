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
from typing import Any

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
        self.texts: dict[Path, bytes] = {}
        self.relative_cache: dict[str, str] = {}
        self.generic = {"selected": 0, "ambiguous": 0}
        self.plain: dict[str, bool] = {}

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

    # Unevaluated parts ---------------------------------------------------------------------

    def text(self, path: Path) -> bytes:
        if path not in self.texts:
            try:
                self.texts[path] = path.read_bytes()
            except OSError:
                self.texts[path] = b""
        return self.texts[path]

    def association_type(self, expression: ci.Cursor) -> str | None:
        """The type name written before ``:`` in front of a ``_Generic`` association."""
        path, offset = libclang.spelling_location(expression.extent.start)
        if path is None:
            return None
        text = self.text(path)
        index = offset - 1
        while index >= 0 and chr(text[index]) in " \t\r\n\\":
            index -= 1
        if index < 0 or text[index : index + 1] != b":":
            return None
        end = index
        depth = 0
        index -= 1
        while index >= 0:
            character = chr(text[index])
            if character in ")]":
                depth += 1
            elif character in "([":
                if depth == 0:
                    break
                depth -= 1
            elif character == "," and depth == 0:
                break
            index -= 1
        return _normal_type(text[index + 1 : end].decode(errors="replace").replace("\\", " "))

    def selected_associations(self, node: ci.Cursor) -> set[int] | None:
        """The index of the association ``_Generic`` selects, or None when it cannot be told."""
        children = _children(node)
        control = children[0]
        candidates = {_normal_type(control.type.spelling), _normal_type(control.type.get_canonical().spelling)}
        names = [self.association_type(arm) for arm in children[1:]]
        if any(name is None for name in names):
            self.generic["ambiguous"] += 1
            return None
        matches = {index for index, name in enumerate(names, 1) if name in candidates}
        if len(matches) == 1:
            self.generic["selected"] += 1
            return matches
        if not matches and "default" in names:
            self.generic["selected"] += 1
            return {names.index("default") + 1}
        self.generic["ambiguous"] += 1
        return None


def _normal_type(text: str) -> str:
    text = re.sub(r"\s+", " ", text).strip()
    return re.sub(r"\s*\*\s*", "*", text)


def _constant_condition(condition: ci.Cursor) -> int | None:
    for child in condition.walk_preorder():
        if child.kind == K.CALL_EXPR and child.spelling == "__builtin_constant_p":
            return None
    return libclang.constant_int(condition)


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
        self.events["accesses"].append(item)
        if unit["kind"] == "struct" and unit.get("field") is not None:
            self.events["field_accesses"].append(
                {
                    "struct": unit["name"],
                    "def": unit["def"],
                    "field": unit["field"],
                    "offset": unit.get("offset", -1),
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

    def call(self, node: ci.Cursor, parent: ci.Cursor | None, reason: str | None) -> None:
        referenced = node.referenced
        arguments = list(node.get_arguments() or [])
        if referenced is not None and referenced.kind == K.FUNCTION_DECL:
            callee = referenced.spelling
            described = [self.argument(argument) for argument in arguments]
            if reason is not None:
                self.events["unevaluated"].append(
                    {"call": callee, "reason": reason, "provenance": [self.x.location(node)]}
                )
                return
            self.events["calls"].append(
                {"callee": callee, "arguments": described, "provenance": [self.x.location(node)]}
            )
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
        """A local of a cleanup class calls the class's destructor when its scope ends.

        The destructor gets the arguments the constructor got: the local holds what the
        constructor took, such as the lock a guard holds.
        """
        pattern = self.x.vocabulary.cleanup_class
        if pattern is None:
            return
        match = pattern.fullmatch(node.type.spelling)
        if match is None:
            return
        destructor = self.x.vocabulary.cleanup_destructor.format(name=match.group(1))
        constructor = initializer
        while constructor is not None and constructor.kind == K.UNEXPOSED_EXPR and len(_children(constructor)) == 1:
            constructor = _children(constructor)[0]
        arguments = (
            [self.argument(argument) for argument in constructor.get_arguments() or []]
            if constructor is not None and constructor.kind == K.CALL_EXPR
            else []
        )
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
            self.call(node, parent, reason)
        elif kind in (K.DECL_REF_EXPR, K.MEMBER_REF_EXPR):
            self.reference(node, parent, index, reason, generic)
        children = _children(node)
        if kind == K.CXX_UNARY_EXPR:
            for child_index, child in enumerate(children):
                self.walk(child, node, child_index, reason or "sizeof", generic)
            return
        if kind == K.GENERIC_SELECTION_EXPR and children:
            selected = self.x.selected_associations(node) if reason is None else set()
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
            value = _constant_condition(children[0])
            if value is not None:
                live = 1 if value else 2
                self.walk(children[0], node, 0, reason, generic)
                for child_index, child in enumerate(children[1:], 1):
                    self.walk(child, node, child_index, None if child_index == live else "dead_branch", generic)
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


def extract(source: str, c_tree: Path, arguments: list[str], vocabulary: Vocabulary) -> Facts:
    """Parse one C file of the tree and return its facts."""
    main = (c_tree / source).resolve()
    index = ci.Index.create()
    tu = index.parse(str(main), args=arguments)
    extractor = _Extractor(tu, source, c_tree, vocabulary)
    errors = sorted(
        {
            f"{diagnostic.location.file}:{diagnostic.location.line}: {diagnostic.spelling}"
            for diagnostic in tu.diagnostics
            if diagnostic.severity >= ci.Diagnostic.Error
        }
    )
    includes = sorted({extractor.relative(include.include.name) for include in tu.get_includes()})
    definitions = _definitions(tu)
    pending = sorted(name for name, cursor in definitions.items() if _main_file(cursor, main))
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
