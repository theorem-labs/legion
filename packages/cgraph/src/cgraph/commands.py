"""Compile arguments for each C file: from a compilation database, or from a flag template."""

from __future__ import annotations

import json
import os
import re
import shlex
from pathlib import Path

# Arguments that take the next argument as their value and change what the preprocessor or the
# parser sees.
_KEEP_WITH_VALUE = {"-include", "-imacros", "-isystem", "-iquote", "-idirafter", "-I", "-D", "-U", "-target"}
_PATH_VALUE = {"-include", "-imacros", "-isystem", "-iquote", "-idirafter", "-I"}
# Flags without a value that change the language the parser accepts or the types it gives.
_KEEP_FLAGS = {
    "-nostdinc",
    "-ffreestanding",
    "-fms-extensions",
    "-fplan9-extensions",
    "-fshort-wchar",
    "-funsigned-char",
    "-fsigned-char",
    "-fno-builtin",
}
_DROP_WITH_VALUE = {"-o", "-MF", "-MT", "-MQ", "-Xclang", "-mllvm", "-x"}
# The parser is libclang, not the compiler that built the image, so its warnings never matter.
PARSER_FLAGS = ("-Wno-everything",)


def _absolute(value: str, directory: Path) -> str:
    return value if os.path.isabs(value) else os.path.normpath(directory / value)


def filter_arguments(arguments: list[str], directory: Path) -> list[str]:
    """Keep the arguments that decide the preprocessed text and the target; drop code generation."""
    kept: list[str] = []
    index = 1  # arguments[0] is the compiler
    while index < len(arguments):
        argument = arguments[index]
        if argument in _KEEP_WITH_VALUE and index + 1 < len(arguments):
            value = arguments[index + 1]
            kept += [argument, _absolute(value, directory) if argument in _PATH_VALUE else value]
            index += 2
            continue
        if argument in _DROP_WITH_VALUE:
            index += 2
            continue
        if argument.startswith(("-I", "-isystem", "-iquote", "-idirafter")) and argument not in _KEEP_WITH_VALUE:
            flag = next(prefix for prefix in ("-isystem", "-iquote", "-idirafter", "-I") if argument.startswith(prefix))
            kept.append(flag + _absolute(argument[len(flag) :], directory))
        elif argument.startswith(("-D", "-U", "-std=", "--target=", "-m")) or argument in _KEEP_FLAGS:
            kept.append(argument)
        index += 1
    return kept


def rewrite(arguments: list[str], rewrites: tuple[tuple[re.Pattern[str], str], ...]) -> list[str]:
    """Replace each argument a rewrite's pattern matches in full; an empty replacement drops it."""
    result = []
    for argument in arguments:
        for pattern, replacement in rewrites:
            if pattern.fullmatch(argument):
                argument = replacement
                break
        if argument:
            result.append(argument)
    return result


def load_database(
    path: Path, c_tree: Path, rewrites: tuple[tuple[re.Pattern[str], str], ...] = ()
) -> dict[str, list[str]]:
    """Map each C file of a compilation database, relative to the C tree, to its parser arguments."""
    entries = json.loads(path.read_text())
    tree = c_tree.resolve()
    result: dict[str, list[str]] = {}
    for entry in entries:
        directory = Path(entry["directory"])
        source = Path(_absolute(entry["file"], directory)).resolve()
        if source.suffix != ".c":
            continue
        try:
            relative = str(source.relative_to(tree))
        except ValueError:
            continue
        arguments = entry.get("arguments") or shlex.split(entry["command"])
        result[relative] = [*rewrite(filter_arguments(arguments, directory), rewrites), *PARSER_FLAGS]
    return dict(sorted(result.items()))


def template_arguments(flags: tuple[str, ...], c_tree: Path, source: str) -> list[str]:
    """Arguments from ``c.flags``: ``{c_tree}``, ``{module}`` (the path without .c) and ``{stem}``."""
    module = source[:-2]
    stem = Path(source).stem
    return [flag.format(c_tree=c_tree, module=module, stem=stem) for flag in flags] + list(PARSER_FLAGS)
