#!/usr/bin/env bash
# Tests for .github/actions/tests-scope: what the action writes to GITHUB_OUTPUT, for each shape of
# head it can run on. It runs the action's own `run:` body, read out of action.yml, so the code
# under test is the code CI runs, against miniature git repositories.
#
# What a step writes to its output file is what these cover: the runner refuses a line that is not
# key=value and fails the step, so a job-log line (a `::warning::`) that reaches the file turns the
# fallback "every step runs" into "nothing after this runs".
#
# Run from anywhere: .github/scripts/tests-scope-action.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository=$(cd "$script_dir/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

body=$work/action-body.sh
python3 - "$repository/.github/actions/tests-scope/action.yml" "$body" <<'PY'
import sys, yaml
action = yaml.safe_load(open(sys.argv[1]))
steps = [s for s in action["runs"]["steps"] if s.get("id") == "scope"]
if len(steps) != 1:
    sys.exit("action.yml has no single step with id: scope")
open(sys.argv[2], "w").write(steps[0]["run"])
PY

git_quiet() { git -c init.defaultBranch=main -c user.name=t -c user.email=t@example.invalid -C "$1" "${@:2}"; }

# fixture <name> <parents>: a repository whose HEAD has one or two parents, with the real
# tests-scope.sh symlinked where the action calls it, so the classifier under test is the file CI
# runs. The two-parent head is a merge commit of a branch into main, as GitHub builds for a pull
# request; its first parent is the base. It changes packages/orchestration/a.go alone, which
# selects the orchestration ledger only.
fixture() {
  local root="$work/$1" parents=$2
  mkdir -p "$root/packages/orchestration" "$root/docs" "$root/.github/scripts"
  ln -s "$script_dir/tests-scope.sh" "$root/.github/scripts/tests-scope.sh"
  git_quiet "$root" init -q
  echo base > "$root/docs/index.md"
  git_quiet "$root" add -A
  git_quiet "$root" commit -qm base
  if [ "$parents" = 2 ]; then
    git_quiet "$root" checkout -q -b feature
    echo 'package orchestration' > "$root/packages/orchestration/a.go"
    git_quiet "$root" add -A
    git_quiet "$root" commit -qm feature
    git_quiet "$root" checkout -q main
    echo more >> "$root/docs/index.md"
    git_quiet "$root" add -A
    git_quiet "$root" commit -qm "base moves on"
    git_quiet "$root" merge -q --no-ff -m merge feature
  fi
  echo "$root"
}

# run_body <root> <event>: the action's body in root, with a fresh output file. Sets $out (the job
# log) and $output (every line of GITHUB_OUTPUT).
run_body() {
  local root=$1 event=$2
  local runner_temp="$work/runner-temp"
  mkdir -p "$runner_temp"
  : > "$root/github-output"
  set +e
  out=$(cd "$root" && EVENT="$event" RUNNER_TEMP="$runner_temp" GITHUB_OUTPUT="$root/github-output" \
    bash "$body" 2>&1)
  status=$?
  set -e
  output=$(cat "$root/github-output")
}

# every_line_is_an_output <text>: true when each line is key=value, as the runner requires.
every_line_is_an_output() {
  local line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    [[ $line =~ ^[a-z_]+=[^[:space:]]+$ ]] || { echo false; return; }
  done <<<"$1"
  echo true
}

echo "case: a pull request's merge commit, an orchestration-only change"
root=$(fixture merge 2)
run_body "$root" pull_request
check "exits 0" "$(is "$status" 0)"
check "selects the orchestration ledger alone" "$(is "$output" "$(printf 'orchestration=true\nother=false')")"
check "says how many files changed" "$(contains "$out" 'The pull request changes 1 file')"
check "the file count is not an output" "$(is "$(contains "$output" 'changes 1 file')" false)"

echo "case: a head with no second parent falls back to every step"
root=$(fixture single 1)
run_body "$root" pull_request
check "exits 0" "$(is "$status" 0)"
check "warns in the job log" "$(contains "$out" '^::warning::HEAD is not a pull request')"
check "every output line is key=value" "$(every_line_is_an_output "$output")"
check "the warning is not an output" "$(is "$(contains "$output" '::warning::')" false)"
check "selects every step" "$(is "$output" "$(printf 'orchestration=true\nother=true')")"

echo "case: a push and a merge group select every step, parents unread"
for event in push merge_group; do
  root=$(fixture "$event" 1)
  run_body "$root" "$event"
  check "$event exits 0" "$(is "$status" 0)"
  check "$event selects every step" "$(is "$output" "$(printf 'orchestration=true\nother=true')")"
  check "$event writes only key=value" "$(every_line_is_an_output "$output")"
  check "$event names itself in the log" "$(contains "$out" "^$event runs every step")"
done

echo "case: a classifier that cannot run fails the step rather than leaving checks unselected"
root=$(fixture broken 2)
rm "$root/.github/scripts/tests-scope.sh"
run_body "$root" pull_request
check "exits non-zero" "$([ "$status" -ne 0 ] && echo true || echo false)"
check "writes no output" "$(is "$output" "")"

echo "case: the action appends, never truncates, what another step wrote"
root=$(fixture appends 2)
run_body "$root" pull_request
printf 'earlier=kept\n' > "$root/github-output"
out=$(cd "$root" && EVENT=pull_request RUNNER_TEMP="$work/runner-temp" \
  GITHUB_OUTPUT="$root/github-output" bash "$body" 2>&1)
output=$(cat "$root/github-output")
check "keeps the earlier line" "$(contains "$output" '^earlier=kept$')"
check "adds its own" "$(contains "$output" '^orchestration=true$')"

summary "the tests-scope action's outputs"
