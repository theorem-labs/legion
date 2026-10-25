#!/usr/bin/env bash
# Tests for `orchestration_packages.py`: which packages of packages/orchestration a range of commits
# selects. Each case runs the real script in a miniature repository with a three-package module:
# lib, app (imports lib) and cli, whose test builds ../app by a relative path and imports nothing.
#
# Run from anywhere: .github/scripts/orchestration_packages.test.sh
# CI runs it in the orchestration-branches workflow (job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
select_script="$script_dir/orchestration_packages.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

repo="$work/repo"
module="$repo/packages/orchestration"
mkdir -p "$module/lib/testdata" "$module/app" "$module/cli" "$repo/docs"
git -C "$repo" init -q
git -C "$repo" config user.email test@example.invalid
git -C "$repo" config user.name Test
printf 'module example.com/orch\n\ngo 1.22\n' > "$module/go.mod"
printf 'package lib\n\nfunc Value() int { return 1 }\n' > "$module/lib/lib.go"
printf 'fixture\n' > "$module/lib/testdata/input.txt"
printf 'package app\n\nimport "example.com/orch/lib"\n\nfunc Value() int { return lib.Value() }\n' > "$module/app/app.go"
printf 'package cli\n' > "$module/cli/cli.go"
printf 'package cli\n\nimport "testing"\n\nfunc TestBuild(t *testing.T) { _ = "../app" }\n' > "$module/cli/cli_test.go"
printf 'docs\n' > "$repo/docs/readme.md"
git -C "$repo" add -A
git -C "$repo" commit -qm base
base=$(git -C "$repo" rev-parse HEAD)

# commit FILE TEXT: append TEXT to FILE and commit it; prints the new head.
commit() {
  printf '%s\n' "$2" >> "$repo/$1"
  git -C "$repo" commit -qam "change $1"
  git -C "$repo" rev-parse HEAD
}

selected() { (cd "$repo" && "$select_script" "$1" "$2" | tr '\n' ' ' | sed 's/ $//'); }

all="example.com/orch/app example.com/orch/cli example.com/orch/lib"

head=$(commit docs/readme.md more)
check "a change outside the module selects nothing" "$(is "$(selected "$base" "$head")" "")"

base=$head
head=$(commit packages/orchestration/lib/lib.go '// more')
check "a change to lib selects lib, app (imports it) and cli (its test builds ../app)" \
  "$(is "$(selected "$base" "$head")" "$all")"

base=$head
head=$(commit packages/orchestration/app/app.go '// more')
check "a change to app selects app and cli" "$(is "$(selected "$base" "$head")" "example.com/orch/app example.com/orch/cli")"

base=$head
head=$(commit packages/orchestration/cli/cli.go '// more')
check "a change to cli selects cli alone" "$(is "$(selected "$base" "$head")" "example.com/orch/cli")"

base=$head
head=$(commit packages/orchestration/lib/testdata/input.txt more)
check "a testdata change selects its package and its dependents" "$(is "$(selected "$base" "$head")" "$all")"

base=$head
head=$(commit packages/orchestration/go.mod '// more')
check "a go.mod change selects every package" "$(is "$(selected "$base" "$head")" "$all")"

check "an empty base selects every package" "$(is "$(selected "" "$head")" "$all")"

branch=$(git -C "$repo" symbolic-ref --short HEAD)
git -C "$repo" checkout -q --orphan unrelated
git -C "$repo" commit -qm unrelated
unrelated=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" checkout -q "$branch"
check "a base that is not an ancestor (a force push) selects every package" \
  "$(is "$(selected "$unrelated" "$head")" "$all")"

summary "orchestration_packages.py"
