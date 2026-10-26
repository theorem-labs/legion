#!/usr/bin/env bash
# Tests for `tests-scope.sh`: which parts of the Tests workflow a pull request's changed files
# select. Each case pipes a file list into the real script.
#
# Run from anywhere: .github/scripts/tests-scope.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scope_script="$script_dir/tests-scope.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# scope <path>...: the script's two output lines for those paths, joined by a space.
scope() { printf '%s\n' "$@" | "$scope_script" | tr '\n' ' ' | sed 's/ $//'; }
both="orchestration=true other=true"
orchestration_only="orchestration=true other=false"
neither="orchestration=false other=false"

echo "case: an orchestration source, test or testdata file selects the orchestration ledger alone"
check "a Go source" "$(is "$(scope packages/orchestration/ledger/ledger.go)" "$orchestration_only")"
check "a testdata file" "$(is "$(scope packages/orchestration/declindex/testdata/a.json)" "$orchestration_only")"
check "a file at the module root" "$(is "$(scope packages/orchestration/README.md)" "$orchestration_only")"

echo "case: the orchestration module's manifests select both, as the workspace shares them"
check "go.mod" "$(is "$(scope packages/orchestration/go.mod)" "$both")"
check "go.sum" "$(is "$(scope packages/orchestration/go.sum)" "$both")"
check "go.mod beside a source" "$(is "$(scope packages/orchestration/a.go packages/orchestration/go.mod)" "$both")"

echo "case: the orchestration-only workflows and their selection script select nothing"
check "orchestration-lean.yaml" "$(is "$(scope .github/workflows/orchestration-lean.yaml)" "$neither")"
check "orchestration-branches.yaml" "$(is "$(scope .github/workflows/orchestration-branches.yaml)" "$neither")"
check "orchestration_packages.py and its test" \
  "$(is "$(scope .github/scripts/orchestration_packages.py .github/scripts/orchestration_packages.test.sh)" "$neither")"
check "beside an orchestration source" \
  "$(is "$(scope .github/workflows/orchestration-lean.yaml packages/orchestration/a.go)" "$orchestration_only")"

echo "case: every other path selects both"
for path in go.work go.work.sum packages/daemon/main.go packages/daemon/go.sum packages/envoy/go.mod \
  packages/contracts/fixtures/x.json packages/pi-legion/package.json deploy/kubernetes/daemon/legion.yaml.example \
  scripts/e2e/stage1-skeleton.sh .github/workflows/pr-and-main.yaml .github/actions/tests-scope/action.yml \
  .github/scripts/tests-scope.sh .github/scripts/test-lib.sh docs/index.md AGENTS.md \
  packages/orchestrationx/a.go packages/orchestration; do
  check "$path" "$(is "$(scope "$path")" "$both")"
done
check "one such path among orchestration files" \
  "$(is "$(scope packages/orchestration/a.go docs/index.md .github/workflows/orchestration-lean.yaml)" "$both")"

echo "case: no path at all selects both"
check "empty input" "$(is "$("$scope_script" </dev/null | tr '\n' ' ' | sed 's/ $//')" "$both")"
check "blank lines only" "$(is "$(printf '\n\n' | "$scope_script" | tr '\n' ' ' | sed 's/ $//')" "$both")"

echo "case: a path holding a space is read whole"
check "an orchestration file" "$(is "$(scope 'packages/orchestration/a b.go')" "$orchestration_only")"

summary "tests-scope.sh"
