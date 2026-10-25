#!/bin/sh
# Times the declaration index's commands against one index, and holds them to the acceptance bars: `users` with
# its reach, median under 1 s and 95th percentile under 3 s; `where`, `states` and `same`, median under 1 s.
# `land-hash`, `gate` and `ledger` are reported with no bar. --refresh-landing times a refresh after a landing
# instead, against its own bar of 30 s besides the build.
set -eu

usage() {
	printf '%s\n' 'usage: timings.sh [--index-dsn-file FILE] [--limit N] [--concepts FILE] [--proposer OWNER] [--gate-proposal FILE] CONFIG SAMPLE' >&2
	printf '%s\n' '       timings.sh [--index-dsn-file FILE] --refresh-landing CONFIG' >&2
	printf '%s\n' '       timings.sh --bars RUNS.tsv' >&2
	printf '%s\n' 'SAMPLE is one declaration name for each line; write one with go run ./declindex/e2e/sample.' >&2
	printf '%s\n' 'The landed commit of --refresh-landing is TIMINGS_LAND_COMMIT. DECLINDEX_BIN overrides the command.' >&2
	printf '%s\n' '--bars decides the bars again over the runs of an earlier measurement, and prints the same report.' >&2
	exit 2
}

die() {
	printf 'timings.sh: %s\n' "$1" >&2
	exit 1
}

# decide prints the report of the runs in the file it is given and exits nonzero when a bar is not met: the
# median and the 95th percentile of each command that has one, over the runs that answered, and a line for each
# command without a bar. A command with a bar must answer every run of its sample: the plan names what the sample
# holds, and a command that fails returns fast, so a bar decided on what happened to answer would pass for an
# index that is not there.
decide() {
	python3 - "$1" <<'PY'
import math
import sys

# The bars of the plan's acceptance: the median and the 95th percentile that each command must stay under. A
# command with no bar is reported only.
bars = {"users": (1.0, 3.0), "where": (1.0, None), "states": (1.0, None), "same": (1.0, None)}
order = ["where", "users", "states", "same", "land-hash", "gate", "ledger"]

runs: dict[str, list[float]] = {}
answers: dict[str, list[float]] = {}
for line in open(sys.argv[1], encoding="utf-8"):
    label, _name, seconds, status = line.rstrip("\n").split("\t")
    runs.setdefault(label, []).append(float(seconds))
    if status == "0":
        answers.setdefault(label, []).append(float(seconds))


def median(values: list[float]) -> float:
    half = len(values) // 2
    if len(values) % 2:
        return values[half]
    return (values[half - 1] + values[half]) / 2


def percentile(values: list[float], fraction: float) -> float:
    # Nearest rank: the smallest value that at least `fraction` of the runs are at or below.
    return values[min(len(values) - 1, max(0, math.ceil(fraction * len(values)) - 1))]


print("summary\tcommand\truns\tanswered\tmedian_s\tp95_s\tworst_s")
for label in order + sorted(set(runs) - set(order)):
    values = sorted(runs.get(label, []))
    if not values:
        continue
    print(
        f"summary\t{label}\t{len(values)}\t{len(answers.get(label, []))}\t{median(values):.3f}"
        f"\t{percentile(values, 0.95):.3f}\t{values[-1]:.3f}"
    )

failed = False
for label, (median_bar, p95_bar) in bars.items():
    # A bar is about how long an answer takes, so only the runs that answered are timed. A run that did not
    # answer is not a slow answer: it is a measurement the acceptance did not get, and the bar fails.
    values = sorted(answers.get(label, []))
    tried = len(runs.get(label, []))
    if tried == 0:
        # A bar no run measured is not a bar that was met: the sample, or the concepts of `states`, was empty.
        print(f"bar\t{label}\truns\t0\tof the sample\tFAIL")
        failed = True
        continue
    if len(values) < tried:
        print(f"bar\t{label}\tanswered\t{len(values)}\tof {tried}\tFAIL")
        failed = True
    if not values:
        continue
    got = median(values)
    ok = got < median_bar
    failed = failed or not ok
    print(f"bar\t{label}\tmedian\t{got:.3f}\t< {median_bar}\t{'ok' if ok else 'FAIL'}")
    if p95_bar is None:
        continue
    got = percentile(values, 0.95)
    ok = got < p95_bar
    failed = failed or not ok
    print(f"bar\t{label}\tp95\t{got:.3f}\t< {p95_bar}\t{'ok' if ok else 'FAIL'}")
raise SystemExit(1 if failed else 0)
PY
}

index_dsn_file=
limit=0
proposer=function:timings
gate_proposal=
concepts=
refresh_landing=false
bars_only=
while [ "$#" -gt 0 ]; do
	case $1 in
	--bars)
		[ "$#" -ge 2 ] || usage
		bars_only=$2
		shift 2
		;;
	--index-dsn-file | --limit | --proposer | --gate-proposal | --concepts)
		[ "$#" -ge 2 ] || usage
		case $1 in
		--index-dsn-file) index_dsn_file=$2 ;;
		--limit) limit=$2 ;;
		--proposer) proposer=$2 ;;
		--gate-proposal) gate_proposal=$2 ;;
		--concepts) concepts=$2 ;;
		esac
		shift 2
		;;
	--refresh-landing)
		refresh_landing=true
		shift
		;;
	--)
		shift
		break
		;;
	-*) usage ;;
	*) break ;;
	esac
done

# --bars decides the bars again over the runs of an earlier measurement, and runs no command of its own.
if [ -n "$bars_only" ]; then
	[ "$#" -eq 0 ] || usage
	[ -r "$bars_only" ] || die "cannot read the runs $bars_only"
	decide "$bars_only"
	exit $?
fi

declindex_bin=${DECLINDEX_BIN-declindex}
if $refresh_landing; then
	[ "$#" -eq 1 ] || usage
	cfg=$1
	sample=
else
	[ "$#" -eq 2 ] || usage
	cfg=$1
	sample=$2
	[ -r "$sample" ] || die "cannot read the sample $sample"
fi
[ -r "$cfg" ] || die "cannot read the configuration $cfg"
case $limit in
'' | *[!0-9]*) die "--limit takes a count" ;;
esac

# GNU time, which -f and -o need; the shell's own time keyword cannot write to a file.
[ -x /usr/bin/time ] || die '/usr/bin/time is not installed (GNU time)'
/usr/bin/time -f '%e' -o /dev/null true 2>/dev/null || die '/usr/bin/time does not take -f: install GNU time'

# The index is one schema of a Postgres database. The configuration names the schema and the indexed tree; the
# connection string comes from --index-dsn-file or from DECLINDEX_DSN in the environment, never from the tree.
cfg_values=$(python3 - "$cfg" <<'PY'
import os
import sys

cfg = os.path.abspath(sys.argv[1])
values = {}
for line in open(cfg, encoding="utf-8"):
    key, sep, rest = line.partition(":")
    if not sep or key.strip() != key or key not in ("tree", "index"):
        continue
    values[key] = rest.strip().strip("'\"")
tree = values.get("tree")
if not tree:
    raise SystemExit("timings.sh: configuration has no tree")
if not values.get("index"):
    raise SystemExit("timings.sh: configuration has no index")
print(tree if os.path.isabs(tree) else os.path.normpath(os.path.join(os.path.dirname(cfg), tree)))
print(values["index"])
PY
)
tree=$(printf '%s\n' "$cfg_values" | sed -n '1p')
index=$(printf '%s\n' "$cfg_values" | sed -n '2p')

work=$(mktemp -d "${TMPDIR:-/tmp}/timings.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# Every run gets the connection string the same way, with --index-dsn-file; a string from the environment goes to
# a file of the run's own private directory first.
dsn_file=$work/index.dsn
if [ -n "$index_dsn_file" ]; then
	[ -r "$index_dsn_file" ] || die "cannot read the connection string $index_dsn_file"
	cat "$index_dsn_file" >"$dsn_file"
else
	[ -n "${DECLINDEX_DSN-}" ] || die 'no connection string: pass --index-dsn-file or set DECLINDEX_DSN'
	printf '%s\n' "$DECLINDEX_DSN" >"$dsn_file"
fi

runs=$work/runs.tsv
: >"$runs"

# run records one timed run: its seconds, its exit status, and one line on stdout. It never stops the driver; a
# command that fails is reported with its status, and the bars below decide the exit code.
run() {
	label=$1
	name=$2
	shift 2
	set +e
	/usr/bin/time -f '%e' -o "$work/time" "$@" >"$work/out" 2>"$work/err"
	status=$?
	set -e
	seconds=$(tail -n 1 "$work/time")
	printf 'run\t%s\t%s\t%s\t%s\n' "$label" "$name" "$seconds" "$status"
	printf '%s\t%s\t%s\t%s\n' "$label" "$name" "$seconds" "$status" >>"$runs"
}

printf 'index\t%s\n' "$index"
printf 'tree\t%s\n' "$tree"
printf 'command\t%s\n' "$declindex_bin"

if $refresh_landing; then
	land_commit=${TIMINGS_LAND_COMMIT-}
	[ -n "$land_commit" ] || die '--refresh-landing needs TIMINGS_LAND_COMMIT, the landed commit to refresh to'
	case $land_commit in
	*[!0-9a-f]*) die 'TIMINGS_LAND_COMMIT must be 40 lowercase hexadecimal characters' ;;
	esac
	[ "${#land_commit}" -eq 40 ] || die 'TIMINGS_LAND_COMMIT must be 40 lowercase hexadecimal characters'
	run refresh-landing "$land_commit" env LAND_COMMIT="$land_commit" \
		"$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" --json refresh
	cp "$work/out" "$work/refresh.json"
	cat "$work/err" >&2
	[ "$status" -eq 0 ] || die "the refresh of $land_commit exited $status"
	# A refresh of a commit the index already covers prints that and no report: it did no work, so there is
	# nothing to time. The measurement needs a commit this index has not read.
	[ -s "$work/refresh.json" ] || die "the index already covers $land_commit, so this refresh did no work: give TIMINGS_LAND_COMMIT a commit the index has not read"
	python3 - "$work/refresh.json" "$seconds" <<'PY'
import json
import sys

report = json.load(open(sys.argv[1], encoding="utf-8"))
wall = float(sys.argv[2])
index_seconds = report["elapsed_ns"] / 1e9
# elapsed_ns is the refresh's own work: the landed sync, the checkout and the build run before it starts, so this
# is the time the bar names, "besides the build".
print(f"wall\trefresh-landing\t{wall:.3f}")
print(f"index\trefresh-landing\t{index_seconds:.3f}")
ok = index_seconds < 30.0
print(f"bar\trefresh-landing\tindex\t{index_seconds:.3f}\t< 30\t{'ok' if ok else 'FAIL'}")
raise SystemExit(0 if ok else 1)
PY
	exit $?
fi

names=$work/names
if [ "$limit" -gt 0 ]; then
	grep -v -e '^#' -e '^[[:space:]]*$' "$sample" | head -n "$limit" >"$names"
else
	grep -v -e '^#' -e '^[[:space:]]*$' "$sample" >"$names"
fi
[ -s "$names" ] || die "the sample $sample names no declaration"
printf 'declarations\t%s\n' "$(wc -l <"$names" | tr -d ' ')"

land_tree=$(git -C "$tree" rev-parse 'HEAD^{tree}')
land_git_dir=$(git -C "$tree" rev-parse --absolute-git-dir)
contracts=$work/contracts.json
printf '[]\n' >"$contracts"

i=0
while IFS= read -r name; do
	i=$((i + 1))
	run where "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" where "$name"
	run users "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" users "$name"
	if [ -z "$concepts" ]; then
		run states "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" states "$name"
	fi
	run same "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" same "$name"
	run land-hash "$name" env LAND_TREE="$land_tree" LAND_GIT_DIR="$land_git_dir" LAND_NAMES="$(printf '["%s"]' "$name")" \
		"$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" land-hash
	if [ -n "$gate_proposal" ]; then
		proposal=$gate_proposal
	else
		proposal=$work/proposal.json
		python3 - "$proposal" "$name" "$i" <<'PY'
import json
import sys

out, name, serial = sys.argv[1], sys.argv[2], sys.argv[3]
module = name.rsplit(".", 1)[0] if "." in name else name
concept = f"Timings.Probe{serial}"
json.dump(
    {
        "id": f"timings-{serial}",
        "concept": {"name": concept, "home": f"{module.replace('.', '/')}.lean", "meaning": f"A timing probe beside {name}."},
        "source": f"def {concept} : Prop := True\n",
    },
    open(out, "w", encoding="utf-8"),
)
PY
	fi
	run gate "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" gate --proposer "$proposer" "$proposal"
	run ledger "$name" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" \
		ledger --contracts-file "$contracts" --emit "$work/operations.jsonl"
done <"$names"

# `states` answers for a registered concept, which a sample of declarations need not hold: with --concepts it is
# timed over those names instead, so that its bar measures the walk and not a name that the registry does not have.
if [ -n "$concepts" ]; then
	while IFS= read -r concept; do
		case $concept in
		'' | '#'*) continue ;;
		esac
		run states "$concept" "$declindex_bin" --index-dsn-file "$dsn_file" --config "$cfg" states "$concept"
	done <"$concepts"
fi

decide "$runs"
