#!/usr/bin/env bash
# Which steps of the Tests workflow (pr-and-main.yaml) a pull request's changed files can reach.
# Reads one repository-relative path per line on stdin, every path the pull request adds, changes or
# deletes (both sides of a rename), and prints two GITHUB_OUTPUT lines:
#
#   orchestration=true|false  the orchestration ledger's go vet and go test, and the Go setup and
#                             workspace checksum check they share with the rest
#   other=true|false          every other expensive step of the lint, typecheck and test jobs: the
#                             Go daemon's vet and tests, the release notices cross-build, the
#                             script and live-proof library tests, Stage 1
#
# other=true always comes with orchestration=true. The cheap checks of the lint job (the workflow
# and manifest checks, gofmt, Biome) and the jobs themselves run whatever this prints, so each
# required check reports at every head.
#
# A path selects everything unless this script knows exactly which steps read it, so a file
# nobody listed runs the whole workflow:
#
# - packages/orchestration/go.mod and go.sum select everything: every Go build here runs in
#   workspace mode (go.work), where each workspace module's requirements move the versions the
#   other modules compile against.
# - Any other file under packages/orchestration/ selects the orchestration ledger alone. No other
#   module imports it (`go list -deps -test ./...` in packages/daemon, with and without -tags e2e,
#   in packages/envoy and in packages/envoy/infra names none of its packages), and no step outside
#   the ledger's own compiles or reads its files. Two gated steps do scan the whole repository,
#   this file and packages/orchestration included: check-plugin-names.test.sh and
#   check-private-names.test.sh each end by running their checker over the real tree. Skipping
#   them loses no check of the tree, because the lint job runs those same two checkers,
#   unconditionally, over the same files on every head; what the skipped tests prove is the
#   checkers' own behaviour on fixtures, which only a change to a checker or its test can break,
#   and either of those selects everything.
# - The orchestration-lean and orchestration-branches workflows and the package selection they run
#   select nothing: no step of the Tests workflow reads them but the lint job's workflow checks,
#   which always run.
# - Every other path selects everything, this script and pr-and-main.yaml included.
# - No path at all selects everything: a diff this cannot read is tested in full.
#
# Usage: git diff --name-only --no-renames <base> <head> | .github/scripts/tests-scope.sh
# CI runs it through .github/actions/tests-scope in each job of pr-and-main.yaml, and its tests
# (tests-scope.test.sh, and tests-scope-action.test.sh for the action) in that workflow's test job.
set -euo pipefail

orchestration=false
other=false
seen=false
while IFS= read -r path; do
  [ -n "$path" ] || continue
  seen=true
  case "$path" in
    packages/orchestration/go.mod | packages/orchestration/go.sum)
      orchestration=true other=true ;;
    packages/orchestration/*)
      orchestration=true ;;
    .github/workflows/orchestration-lean.yaml | .github/workflows/orchestration-branches.yaml | \
      .github/scripts/orchestration_packages.py | .github/scripts/orchestration_packages.test.sh) ;;
    *)
      orchestration=true other=true ;;
  esac
done
if [ "$seen" = false ]; then
  orchestration=true other=true
fi
echo "orchestration=$orchestration"
echo "other=$other"
