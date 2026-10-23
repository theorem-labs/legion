#!/bin/sh
set -eu

usage() {
	printf '%s\n' 'usage: ledger-join.sh <cfg> <repo> <contract> <owner> <dsn-file>' >&2
	exit 2
}

[ "$#" -eq 5 ] || usage

cfg=$1
repo=$2
contract=$3
owner=$4
dsn_file=$5
declindex_bin=${DECLINDEX_BIN-declindex}
ledger_bin=${LEDGER_BIN-ledger}
candidate=${E7_CANDIDATE_COMMIT-}
changed_hash=${E7_CHANGED_HASH-}

if [ -z "$candidate" ] || [ -z "$changed_hash" ]; then
	printf '%s\n' 'ledger-join.sh: E7_CANDIDATE_COMMIT and E7_CHANGED_HASH are required' >&2
	exit 2
fi
if [ "${#candidate}" -ne 40 ]; then
	printf '%s\n' 'ledger-join.sh: E7_CANDIDATE_COMMIT must be 40 lowercase hexadecimal characters' >&2
	exit 2
fi
case "$candidate" in
*[!0-9a-f]*)
	printf '%s\n' 'ledger-join.sh: E7_CANDIDATE_COMMIT must be 40 lowercase hexadecimal characters' >&2
	exit 2
	;;
esac
if [ "${#changed_hash}" -ne 16 ]; then
	printf '%s\n' 'ledger-join.sh: E7_CHANGED_HASH must be a 16-character lowercase hexadecimal statement hash' >&2
	exit 2
fi
case "$changed_hash" in
*[!0-9a-f]*)
	printf '%s\n' 'ledger-join.sh: E7_CHANGED_HASH must be a 16-character lowercase hexadecimal statement hash' >&2
	exit 2
	;;
esac

hash_file=$(mktemp "${TMPDIR:-/tmp}/ledger-join.hash.XXXXXX")
status_a_file=$(mktemp "${TMPDIR:-/tmp}/ledger-join.status-a.XXXXXX")
contract_file=$(mktemp "${TMPDIR:-/tmp}/ledger-join.contract.XXXXXX")
status_c_file=$(mktemp "${TMPDIR:-/tmp}/ledger-join.status-c.XXXXXX")
check_file=$(mktemp "${TMPDIR:-/tmp}/ledger-join.check.XXXXXX")
trap 'rm -f "$hash_file" "$status_a_file" "$contract_file" "$status_c_file" "$check_file"' EXIT HUP INT TERM

tree=$(python3 - "$cfg" <<'PY'
import os
import sys

cfg = os.path.abspath(sys.argv[1])
for line in open(cfg, encoding="utf-8"):
    if line.lstrip().startswith("tree:"):
        value = line.split(":", 1)[1].strip().strip("'\"")
        if not value:
            break
        print(value if os.path.isabs(value) else os.path.normpath(os.path.join(os.path.dirname(cfg), value)))
        break
else:
    raise SystemExit("ledger-join.sh: configuration has no tree")
PY
)

base=$(git -C "$tree" rev-parse HEAD)
git -C "$tree" merge-base --is-ancestor "$base" "$candidate"

"$ledger_bin" --dsn-file "$dsn_file" repo ensure "$repo" 3

printf '%s\n' 'E7 step 1: register the selected contracts'
"$declindex_bin" --config "$cfg" ledger --dsn-file "$dsn_file"

printf '%s\n' 'E7 step 2: open e7-a at the base statement hash'
"$declindex_bin" --config "$cfg" hash "$contract" > "$hash_file"
IFS="$(printf '\t')" read -r got_contract base_hash location < "$hash_file"
if [ "$got_contract" != "$contract" ] || [ -z "$base_hash" ] || [ -z "$location" ]; then
	printf '%s\n' 'ledger-join.sh: declindex hash did not return one name, hash, and path:line row' >&2
	exit 1
fi
source_path=${location%:*}
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" e7-a "$owner" "$base" e7-a --write "$source_path" --read "$contract=$base_hash"
"$ledger_bin" --dsn-file "$dsn_file" --json status "$repo" > "$status_a_file"
python3 - "$status_a_file" e7-a "$contract" 1 <<'PY'
import json
import sys

status, migration, contract, version = sys.argv[1:]
for row in json.load(open(status, encoding="utf-8"))["Migrations"]:
    if row["id"] == migration and row.get("read_versions", {}).get(contract) == int(version):
        break
else:
    raise SystemExit(f"ledger-join.sh: {migration} does not record {contract} at version {version}")
PY

printf '%s\n' 'E7 step 3: open e7-b at the same base statement hash'
other_path="$source_path.e7-b"
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" e7-b "$owner" "$base" e7-b --write "$other_path" --read "$contract=$base_hash"

printf '%s\n' 'E7 step 4: land e7-a with the prepared candidate statement hash'
"$ledger_bin" --dsn-file "$dsn_file" land --repo "$repo" --migration e7-a --generation 1 --commit "$candidate" --path "$source_path" --current "$contract=$base_hash" --changed-contract "$contract=$changed_hash" --head "$candidate" --reviewed-head "$candidate"
"$ledger_bin" --dsn-file "$dsn_file" --json contract show "$repo" "$contract" > "$contract_file"
python3 - "$contract_file" "$contract" "$changed_hash" <<'PY'
import json
import sys

path, contract, changed_hash = sys.argv[1:]
row = json.load(open(path, encoding="utf-8"))
if row.get("name") != contract or row.get("version") != 2 or row.get("statement_hash") != changed_hash:
    raise SystemExit(f"ledger-join.sh: expected {contract} at version 2 with hash {changed_hash}, got {row}")
PY

printf '%s\n' 'E7 step 5: open e7-c at the landed statement hash'
new_path="$source_path.e7-c"
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" e7-c "$owner" "$candidate" e7-c --write "$new_path" --read "$contract=$changed_hash"
"$ledger_bin" --dsn-file "$dsn_file" --json status "$repo" > "$status_c_file"
python3 - "$status_c_file" e7-c "$contract" 2 <<'PY'
import json
import sys

status, migration, contract, version = sys.argv[1:]
for row in json.load(open(status, encoding="utf-8"))["Migrations"]:
    if row["id"] == migration and row.get("read_versions", {}).get(contract) == int(version):
        break
else:
    raise SystemExit(f"ledger-join.sh: {migration} does not record {contract} at version {version}")
PY

printf '%s\n' 'E7 step 6: reject e7-b as a stale read'
if "$ledger_bin" --dsn-file "$dsn_file" --json check "$repo" e7-b 1 --path "$other_path" --current "$contract=$changed_hash" > "$check_file" 2>&1; then
	printf '%s\n' 'ledger-join.sh: e7-b unexpectedly passed the stale-read check' >&2
	exit 1
fi
python3 - "$check_file" "$contract" <<'PY'
import json
import sys

path, contract = sys.argv[1:]
report = json.load(open(path, encoding="utf-8"))
if report.get("pass") or not any(v.get("kind") == "stale_read" and v.get("name") == contract for v in report.get("violations", [])):
    raise SystemExit(f"ledger-join.sh: e7-b did not report stale_read for {contract}: {report}")
PY
