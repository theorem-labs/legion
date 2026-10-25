#!/usr/bin/env bash
# Builds the fixture project with the lake on PATH and copies its reference files (.ilean) to OUT
# (default: golden/ beside this script). The fixture has no lean-toolchain file: it builds with the
# toolchain that the orchestration-lean workflow links as elan's default, and golden/ holds that
# toolchain's output. Run it again, and commit golden/, whenever a fixture file or that toolchain changes.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="${1:-$here/golden}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -R "$here/lakefile.toml" "$here/Lib.lean" "$here/Lib" "$work/"
(cd "$work" && lake build >&2)
rm -rf "$out"
mkdir -p "$out"
(cd "$work/.lake/build/lib/lean" && find . -name '*.ilean' | while IFS= read -r f; do
	mkdir -p "$out/$(dirname "$f")"
	cp "$f" "$out/$f"
done)
