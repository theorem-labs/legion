"""Guard expressions: the C integer expressions the facts state and link decides at a fold.

An expression is JSON: ``{"c": n}`` a constant, ``{"p": i}`` the function's parameter ``i``, ``None`` a
value the facts cannot name, or ``{"op": name, "a": [operands]}``: a C operator by its spelling
(``"neg"`` and ``"~"`` and ``"!"`` unary, ``"cast"`` a conversion), ``"in"`` (the operand is one of
``"set"``), or ``"?:"``, whose None condition makes it a choice between its two values. Each carries
its C type: its width ``w`` in bits, its signedness ``s``, and ``b`` for _Bool.
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


def values(expression: Expression | None) -> frozenset[int] | None:
    """Every value the expression may take, or None where one depends on something unknown (a
    parameter, a value the facts cannot name) or there are too many to keep. ``&&``, ``||`` and ``?:``
    take what the operand that decides them gives, as C evaluates them; ``x & 0`` and ``x * 0`` are 0
    whatever ``x`` is."""
    if expression is None or "p" in expression:
        return None
    if "c" in expression:
        return frozenset({normalized(int(expression["c"]), expression)})
    operator, operands = expression["op"], expression["a"]
    if operator in ("&&", "||"):
        decisive = int(operator == "||")
        sides = [values(operand) for operand in operands[:2]]
        truths = [None if side is None else {int(value != 0) for value in side} for side in sides]
        if {decisive} in truths:
            return frozenset({decisive})
        if truths[0] is None or truths[1] is None:
            return None
        if operator == "&&":
            return frozenset({left & right for left in truths[0] for right in truths[1]})
        return frozenset({left | right for left in truths[0] for right in truths[1]})
    if operator == "?:":
        condition = values(operands[0])
        found: set[int] = set()
        for truth in {int(value != 0) for value in condition} if condition is not None else {0, 1}:
            arm = values(operands[1] if truth else operands[2])
            if arm is None:
                return None
            found |= {normalized(value, expression) for value in arm}
        return _bounded(found)
    first = values(operands[0])
    if operator == "in":
        return None if first is None else frozenset({int(value in expression["set"]) for value in first})
    if operator in _UNARY:
        unary = _UNARY[operator]
        return None if first is None else _bounded({normalized(unary(value), expression) for value in first})
    second = values(operands[1])
    if operator in ("*", "&") and frozenset({0}) in (first, second):
        return frozenset({0})
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
    found = values(expression)
    truths = {int(value != 0) for value in found} if found is not None else set()
    return truths.pop() if len(truths) == 1 else None


def mentions_parameter(expression: Expression | None) -> bool:
    if expression is None:
        return False
    return "p" in expression or any(mentions_parameter(operand) for operand in expression.get("a", []))


def substitute(expression: Expression | None, arguments: list[Expression | None]) -> Expression | None:
    """The expression in the caller's terms: each parameter becomes its argument, as the parameter's
    type holds it, or unknown where no argument is stated."""
    if expression is None or "c" in expression:
        return expression
    if "p" in expression:
        index = expression["p"]
        value = arguments[index] if index < len(arguments) else None
        if value is None:
            return None
        return {"op": "cast", "a": [value], **{key: expression[key] for key in _TYPE_KEYS if key in expression}}
    return {**expression, "a": [substitute(operand, arguments) for operand in expression["a"]]}
