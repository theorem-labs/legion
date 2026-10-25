#!/usr/bin/env python3
"""The packages of packages/orchestration that the commits from <base> to <head> can break: the ones
they change and every package whose build or tests depend on one of them.

Usage: .github/scripts/orchestration_packages.py <base> <head>
Run from the repository root. It prints one import path per line, sorted, and nothing when the
commits change no package. It prints every package of the module when it cannot tell what changed:
an empty base, a base that is not an ancestor of head (a force push), a changed file inside the
module that belongs to no package at head, or a Go file whose directory holds no package at head (a
deleted or moved package, nested or not, whose old importers it cannot name), or a change to what the module builds against (its go.mod or go.sum, the root go.work or
go.work.sum, any other workspace module's go.mod or go.sum) or to the workflow, this script or its
test.

A package depends on another through its imports, its test imports, and any directory that a test
file names as a relative path and that holds a package of the module: a literal such as
"../cmd/ledger", or a filepath.Join of string literals that starts with "." or "..". A test that
runs `go build ../cmd/ledger` depends on cmd/ledger without importing it. The dependency is
transitive.

The orchestration-branches workflow runs it (.github/workflows/orchestration-branches.yaml), and
orchestration_packages.test.sh tests it.
"""

import fnmatch
import json
import re
import subprocess
import sys
from pathlib import Path

MODULE_DIR = Path("packages/orchestration")
# A change to any of these can change the result of every package, so it selects them all. The
# workspace files are here because CI builds the module inside the root go.work, where every
# workspace module's requirements choose the versions it compiles against.
WHOLE_MODULE = (
    "go.work",
    "go.work.sum",
    "packages/*/go.mod",
    "packages/*/go.sum",
    ".github/workflows/orchestration-branches.yaml",
    ".github/scripts/orchestration_packages.py",
    ".github/scripts/orchestration_packages.test.sh",
    ".github/scripts/test-lib.sh",
)
RELATIVE_LITERAL = re.compile(r'"(\.\.?/[^"\s]*)"')
# filepath.Join("..", "cmd", "ledger"): string literals only, the first "." or "..".
RELATIVE_JOIN = re.compile(r'filepath\.Join\(\s*("\.\.?"(?:\s*,\s*"[^"\s]*")*)\s*[,)]')
JOIN_PART = re.compile(r'"([^"]*)"')


def git(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", *args], capture_output=True, text=True)


def packages() -> list[dict]:
    """Every package of the module, as `go list -json` reports it."""
    output = subprocess.run(
        ["go", "list", "-e", "-json", "./..."], cwd=MODULE_DIR, capture_output=True, text=True, check=True
    ).stdout
    decoder, index, found = json.JSONDecoder(), 0, []
    while index < len(output):
        if output[index].isspace():
            index += 1
            continue
        package, index = decoder.raw_decode(output, index)
        found.append(package)
    return found


def changed_files(base: str, head: str) -> list[str] | None:
    """The files changed from base to head, or None when base is empty or not an ancestor of head."""
    if not base or git("merge-base", "--is-ancestor", base, head).returncode != 0:
        return None
    diff = git("diff", "--name-only", "--no-renames", base, head)
    if diff.returncode != 0:
        return None
    return [line for line in diff.stdout.splitlines() if line]


def dependencies(package: dict, by_dir: dict[str, str]) -> set[str]:
    """The module's packages that this package's build or tests use directly."""
    found = set(package.get("Imports", [])) | set(package.get("TestImports", [])) | set(package.get("XTestImports", []))
    directory = Path(package["Dir"])
    for name in package.get("TestGoFiles", []) + package.get("XTestGoFiles", []):
        source = (directory / name).read_text(errors="replace")
        paths = RELATIVE_LITERAL.findall(source)
        paths += ["/".join(JOIN_PART.findall(arguments)) for arguments in RELATIVE_JOIN.findall(source)]
        for path in paths:
            target = str((directory / path).resolve())
            if target in by_dir:
                found.add(by_dir[target])
    return found


def whole_module(path: str) -> bool:
    return any(fnmatch.fnmatchcase(path, pattern) for pattern in WHOLE_MODULE)


def owner(path: str, by_dir: dict[str, str], root: Path) -> str | None:
    """The package whose directory is the nearest one at or above the file's, inside the module."""
    directory = Path(path).resolve().parent
    while directory == root or root in directory.parents:
        if str(directory) in by_dir:
            return by_dir[str(directory)]
        directory = directory.parent
    return None


def select(base: str, head: str) -> list[str]:
    listed = packages()
    every = {package["ImportPath"] for package in listed}
    changed = changed_files(base, head)
    if changed is None or any(whole_module(path) for path in changed):
        return sorted(every)
    by_dir = {str(Path(package["Dir"]).resolve()): package["ImportPath"] for package in listed}
    root = MODULE_DIR.resolve()
    inside = [path for path in changed if path.startswith(f"{MODULE_DIR.as_posix()}/")]
    # A Go file belongs to the package of its own directory. When that directory holds no package at
    # head, the package was deleted or moved, and its old importers cannot be named from head.
    if any(path.endswith(".go") and str(Path(path).resolve().parent) not in by_dir for path in inside):
        return sorted(every)
    owners = [owner(path, by_dir, root) for path in inside]
    if None in owners:
        return sorted(every)
    selected = set(owners)
    direct = {package["ImportPath"]: dependencies(package, by_dir) & every for package in listed}
    grew = True
    while grew:
        grew = False
        for path, uses in direct.items():
            if path not in selected and uses & selected:
                selected.add(path)
                grew = True
    return sorted(selected)


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: orchestration_packages.py <base> <head>", file=sys.stderr)
        return 2
    for path in select(sys.argv[1], sys.argv[2]):
        print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
