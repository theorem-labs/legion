"""Guard expressions: the C integer expressions the facts state and link decides at a fold.

An expression is JSON: ``{"c": n}`` a constant, ``{"p": i}`` the function's parameter ``i``, ``{"v":
name}`` the one value a variable holds that the facts cannot state, ``{"g": name}`` the address of an
object the program defines (not weak, so not 0), ``None`` a value the facts cannot name, or ``{"op":
name, "a": [operands]}``: a C operator by its spelling (``"neg"`` and ``"~"`` and ``"!"`` unary,
``"cast"`` a conversion), ``"in"`` (the operand is one of ``"set"``), ``"."`` (the member ``"o"`` bits
into the struct its operand is), or ``"?:"``, whose None condition makes it a choice between its two
values. Each carries its C type: its width ``w`` in bits, its signedness ``s``, and ``b`` for _Bool. A
struct has no type keys: a parameter passed by value, a member that is itself a struct, or
``{"struct": [[bits, expression], ...]}``, a constant struct's scalar members (a member it does not
list is 0).
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

Expression = dict[str, Any]

# An expression that may take more values than this is unknown: a few choices stay exact.
_LIMIT = 16

_BINARY: dict[str, Callable[[int, int], int | None]] = {
    "+": lambda a, b: a + b, "-": lambda a, b: a - b, "*": lambda a, b: a * b,
    "/": lambda a, b: None if b == 0 else (abs(a) // abs(b)) * (1 if (a < 0) == (b < 0) else -1),
    "%": lambda a, b: None if b == 0 else a - b * ((abs(a) // abs(b)) * (1 if (a < 0) == (b < 0) else -1)),
    "<<": lambda a, b: a << b if 0 <= b < 128 else None, ">>": lambda a, b: a >> b if 0 <= b < 128 else None,
    "&": lambda a, b: a & b, "|": lambda a, b: a | b, "^": lambda a, b: a ^ b,
    "==": lambda a, b: int(a == b), "!=": lambda a, b: int(a != b), "<": lambda a, b: int(a < b),
    ">": lambda a, b: int(a > b), "<=": lambda a, b: int(a <= b), ">=": lambda a, b: int(a >= b),
}  # fmt: skip
_UNARY: dict[str, Callable[[int], int]] = {
    "!": lambda a: int(a == 0), "~": lambda a: ~a, "neg": lambda a: -a, "cast": lambda a: a,
}  # fmt: skip
_TYPE_KEYS = ("w", "s", "b")


def normalized(value: int, expression: Expression) -> int:
    """The value as the expression's C type holds it: ``w`` bits, signed when ``s``; a _Bool (``b``)
    holds 1 for any value that is not 0."""
    if expression.get("b"):
        return int(value != 0)
    width = expression.get("w")
    if not width:
        return value
    value &= (1 << width) - 1
    if expression.get("s") and value >= 1 << (width - 1):
        value -= 1 << width
    return value


def _bounded(found: set[int]) -> frozenset[int] | None:
    return frozenset(found) if len(found) <= _LIMIT else None


def nonzero(expression: Expression | None) -> bool:
    """An object's address, also converted to a type that keeps all its bits: never 0."""
    if expression is None:
        return False
    if "g" in expression:
        return True
    if expression.get("op") != "cast":
        return False
    operand = expression["a"][0]
    keeps = expression.get("b") or expression.get("w", 0) >= (operand or {}).get("w", 0)
    return bool(keeps) and nonzero(operand)


def _truths(expression: Expression | None) -> set[int] | None:
    """The truth values the expression may take (an object's address is true), or None."""
    if nonzero(expression):
        return {1}
    found = values(expression)
    return None if found is None else {int(value != 0) for value in found}


def member(struct: Expression | None, bits: int) -> Expression | None:
    """The member ``bits`` into a struct value: what a constant struct lists there (0 where it lists
    nothing), also through the members that are structs themselves; None where the struct is unknown."""
    if struct is None:
        return None
    if "struct" in struct:
        listed: dict[int, Expression | None] = {offset: value for offset, value in struct["struct"]}
        return listed.get(bits, {"c": 0})
    if struct.get("op") == "." and "w" not in struct:
        return member(struct["a"][0], struct["o"] + bits)
    return None


def values(expression: Expression | None) -> frozenset[int] | None:
    """Every value the expression may take, or None where one depends on something unknown (a
    parameter, a value the facts cannot name) or there are too many to keep. ``&&``, ``||`` and ``?:``
    take what the operand that decides them gives, as C evaluates them; ``x & 0`` and ``x * 0`` are 0
    whatever ``x`` is. A struct has no integer value."""
    if expression is None or any(key in expression for key in ("p", "v", "g", "struct")):
        return None
    if "c" in expression:
        return frozenset({normalized(int(expression["c"]), expression)})
    operator, operands = expression["op"], expression["a"]
    if operator == ".":
        held = values(member(operands[0], expression["o"])) if "w" in expression else None
        return None if held is None else _bounded({normalized(value, expression) for value in held})
    if operator in ("&&", "||"):
        decisive = int(operator == "||")
        truths = [_truths(operand) for operand in operands[:2]]
        if {decisive} in truths:
            return frozenset({decisive})
        if truths[0] is None or truths[1] is None:
            return None
        if operator == "&&":
            return frozenset({left & right for left in truths[0] for right in truths[1]})
        return frozenset({left | right for left in truths[0] for right in truths[1]})
    if operator == "?:":
        condition = _truths(operands[0])
        found: set[int] = set()
        for truth in condition if condition is not None else {0, 1}:
            arm = values(operands[1] if truth else operands[2])
            if arm is None:
                return None
            found |= {normalized(value, expression) for value in arm}
        return _bounded(found)
    if operator == "!" and nonzero(operands[0]):
        return frozenset({0})
    first = values(operands[0])
    if operator == "in":
        return None if first is None else frozenset({int(value in expression["set"]) for value in first})
    if operator in _UNARY:
        unary = _UNARY[operator]
        return None if first is None else _bounded({normalized(unary(value), expression) for value in first})
    second = values(operands[1])
    if operator in ("*", "&") and frozenset({0}) in (first, second):
        return frozenset({0})
    if operator in ("==", "!=") and (
        (nonzero(operands[0]) and second == frozenset({0})) or (nonzero(operands[1]) and first == frozenset({0}))
    ):
        # An object's address is not NULL: alloc_pte_late's pagetable_pte_ctor(NULL, …) tests mm != &init_mm.
        return frozenset({int(operator == "!=")})
    if (first is None or second is None) and operator in ("==", "!=") and same(operands[0], operands[1]):
        # Two reads of one value: switch_hrtimer_base's new_cpu_base, set from this_cpu_base.
        return frozenset({int(operator == "==")})
    if first is None or second is None:
        return None
    binary = _BINARY[operator]
    found = set()
    for left in first:
        for right in second:
            result = binary(left, right)
            if result is None:
                return None
            found.add(normalized(result, expression))
    return _bounded(found)


def evaluate(expression: Expression | None) -> int | None:
    """The one value the expression takes, or None."""
    found = values(expression)
    return next(iter(found)) if found is not None and len(found) == 1 else None


def truth(expression: Expression | None) -> int | None:
    """1 where every value the expression may take is true, 0 where every one is false, else None."""
    truths = _truths(expression) or set()
    return truths.pop() if len(truths) == 1 else None


def mentions_parameter(expression: Expression | None) -> bool:
    if expression is None:
        return False
    return "p" in expression or any(mentions_parameter(operand) for operand in expression.get("a", []))


def canonical(expression: Expression | None) -> Expression | None:
    """The expression as one value the facts name (a constant, a parameter, a variable's one value,
    an operation on those), without the casts that keep a type, and a choice whose sides are all that
    value as it; None where it may be one of several values or one the facts cannot name."""
    if expression is None or "op" not in expression:
        return expression
    raw = expression["a"]
    operands = [canonical(operand) for operand in raw]
    if expression["op"] == "?:" and raw[0] is None:
        return operands[1] if operands[1] is not None and operands[1] == operands[2] else None
    first = operands[0] if operands else None
    if first is None or any(operand is None for operand in operands):
        return None
    if expression["op"] == "cast" and all(first.get(key) == expression.get(key) for key in _TYPE_KEYS):
        return first
    return {**expression, "a": operands}


def same(first: Expression | None, second: Expression | None) -> bool:
    """The two expressions are one value: each names it, and they name it alike."""
    named = canonical(first)
    return named is not None and named == canonical(second)


def stated(expression: Expression | None) -> bool:
    """The expression says something of the value at a fold: the values it may take, a parameter of
    the function, or the one value it names."""
    return values(expression) is not None or mentions_parameter(expression) or canonical(expression) is not None


def substitute(expression: Expression | None, arguments: list[Expression | None]) -> Expression | None:
    """The expression in the caller's terms: each parameter becomes its argument, as the parameter's
    type holds it (a struct as it is), or unknown where no argument is stated."""
    if expression is None or ("op" not in expression and "p" not in expression):
        return expression
    if "p" in expression:
        index = expression["p"]
        value = arguments[index] if index < len(arguments) else None
        if value is None:
            return None
        typed = {key: expression[key] for key in _TYPE_KEYS if key in expression}
        return {"op": "cast", "a": [value], **typed} if typed else value
    return {**expression, "a": [substitute(operand, arguments) for operand in expression["a"]]}
