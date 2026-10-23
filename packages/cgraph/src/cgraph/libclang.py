"""The parts of libclang's C interface that its Python binding does not wrap."""

from __future__ import annotations

import ctypes
from functools import cache
from pathlib import Path
from typing import Any

import clang.cindex as ci

# CXBinaryOperatorKind: Assign (22) through OrAssign (32) store to their left operand.
_ASSIGNMENT_OPERATORS = range(22, 33)
# CXUnaryOperatorKind: PostInc, PostDec, PreInc, PreDec.
_INCREMENT_OPERATORS = range(1, 5)
_EVAL_INT = 1


@cache
def _lib() -> Any:
    lib = ci.conf.lib
    lib.clang_getCursorBinaryOperatorKind.argtypes = [ci.Cursor]
    lib.clang_getCursorBinaryOperatorKind.restype = ctypes.c_int
    lib.clang_getCursorUnaryOperatorKind.argtypes = [ci.Cursor]
    lib.clang_getCursorUnaryOperatorKind.restype = ctypes.c_int
    lib.clang_Cursor_Evaluate.argtypes = [ci.Cursor]
    lib.clang_Cursor_Evaluate.restype = ctypes.c_void_p
    lib.clang_EvalResult_getKind.argtypes = [ctypes.c_void_p]
    lib.clang_EvalResult_getKind.restype = ctypes.c_int
    lib.clang_EvalResult_getAsLongLong.argtypes = [ctypes.c_void_p]
    lib.clang_EvalResult_getAsLongLong.restype = ctypes.c_longlong
    lib.clang_EvalResult_dispose.argtypes = [ctypes.c_void_p]
    lib.clang_EvalResult_dispose.restype = None
    lib.clang_Cursor_getVarDeclInitializer.argtypes = [ci.Cursor]
    lib.clang_Cursor_getVarDeclInitializer.restype = ci.Cursor
    lib.clang_Cursor_getVarDeclInitializer.errcheck = ci.Cursor.from_result
    lib.clang_getSpellingLocation.argtypes = [
        ci.SourceLocation,
        ctypes.POINTER(ctypes.c_void_p),
        ctypes.POINTER(ctypes.c_uint),
        ctypes.POINTER(ctypes.c_uint),
        ctypes.POINTER(ctypes.c_uint),
    ]
    lib.clang_getSpellingLocation.restype = None
    lib.clang_getFileName.argtypes = [ctypes.c_void_p]
    lib.clang_getFileName.restype = ci._CXString
    lib.clang_getFileName.errcheck = ci._CXString.from_result
    return lib


def is_assignment(cursor: ci.Cursor) -> bool:
    return int(_lib().clang_getCursorBinaryOperatorKind(cursor)) in _ASSIGNMENT_OPERATORS


def is_increment(cursor: ci.Cursor) -> bool:
    return int(_lib().clang_getCursorUnaryOperatorKind(cursor)) in _INCREMENT_OPERATORS


def is_macro_function_like(cursor: ci.Cursor) -> bool:
    lib = _lib()
    function = lib.clang_Cursor_isMacroFunctionLike
    function.argtypes = [ci.Cursor]
    function.restype = ctypes.c_uint
    return bool(function(cursor))


def is_address_of(cursor: ci.Cursor) -> bool:
    """``&expr``: CXUnaryOperator_AddrOf is 5."""
    return int(_lib().clang_getCursorUnaryOperatorKind(cursor)) == 5


def constant_int(cursor: ci.Cursor) -> int | None:
    """The integer value clang folds an expression to, or None when it is not a constant."""
    result = _lib().clang_Cursor_Evaluate(cursor)
    if not result:
        return None
    try:
        if _lib().clang_EvalResult_getKind(result) != _EVAL_INT:
            return None
        return int(_lib().clang_EvalResult_getAsLongLong(result))
    finally:
        _lib().clang_EvalResult_dispose(result)


def var_initializer(cursor: ci.Cursor) -> ci.Cursor | None:
    initializer = _lib().clang_Cursor_getVarDeclInitializer(cursor)
    return None if initializer is None or initializer.kind.is_invalid() else initializer


def spelling_location(location: ci.SourceLocation) -> tuple[Path | None, int]:
    """The file and byte offset where the text at ``location`` is written (inside a macro body)."""
    file_pointer = ctypes.c_void_p()
    line, column, offset = ctypes.c_uint(), ctypes.c_uint(), ctypes.c_uint()
    _lib().clang_getSpellingLocation(
        location, ctypes.byref(file_pointer), ctypes.byref(line), ctypes.byref(column), ctypes.byref(offset)
    )
    if not file_pointer.value:
        return None, 0
    name = _lib().clang_getFileName(file_pointer)
    return (Path(name) if name else None), int(offset.value)
