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
candidate=${JOIN_CANDIDATE_COMMIT-}
changed_hash=${JOIN_CHANGED_HASH-}

if [ -z "$candidate" ] || [ -z "$changed_hash" ]; then
	printf '%s\n' 'ledger-join.sh: JOIN_CANDIDATE_COMMIT and JOIN_CHANGED_HASH are required' >&2
	exit 2
fi
if [ "${#candidate}" -ne 40 ]; then
	printf '%s\n' 'ledger-join.sh: JOIN_CANDIDATE_COMMIT must be 40 lowercase hexadecimal characters' >&2
	exit 2
fi
case "$candidate" in
*[!0-9a-f]*)
	printf '%s\n' 'ledger-join.sh: JOIN_CANDIDATE_COMMIT must be 40 lowercase hexadecimal characters' >&2
	exit 2
	;;
esac
if [ "${#changed_hash}" -ne 16 ]; then
	printf '%s\n' 'ledger-join.sh: JOIN_CHANGED_HASH must be a 16-character lowercase hexadecimal statement hash' >&2
	exit 2
fi
case "$changed_hash" in
*[!0-9a-f]*)
	printf '%s\n' 'ledger-join.sh: JOIN_CHANGED_HASH must be a 16-character lowercase hexadecimal statement hash' >&2
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

printf '%s\n' 'Step 1: register the selected contracts'
"$declindex_bin" --config "$cfg" ledger --dsn-file "$dsn_file"

printf '%s\n' 'Step 2: open join-a at the base statement hash'
"$declindex_bin" --config "$cfg" hash "$contract" > "$hash_file"
IFS="$(printf '\t')" read -r got_contract base_hash location < "$hash_file"
if [ "$got_contract" != "$contract" ] || [ -z "$base_hash" ] || [ -z "$location" ]; then
	printf '%s\n' 'ledger-join.sh: declindex hash did not return one name, hash, and path:line row' >&2
	exit 1
fi
source_path=${location%:*}
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" join-a "$owner" "$base" join-a --write "$source_path" --read "$contract=$base_hash"
"$ledger_bin" --dsn-file "$dsn_file" --json status "$repo" > "$status_a_file"
python3 - "$status_a_file" join-a "$contract" 1 <<'PY'
import json
import sys

status, migration, contract, version = sys.argv[1:]
for row in json.load(open(status, encoding="utf-8"))["Migrations"]:
    if row["id"] == migration and row.get("read_versions", {}).get(contract) == int(version):
        break
else:
    raise SystemExit(f"ledger-join.sh: {migration} does not record {contract} at version {version}")
PY

printf '%s\n' 'Step 3: open join-b at the same base statement hash'
other_path="$source_path.join-b"
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" join-b "$owner" "$base" join-b --write "$other_path" --read "$contract=$base_hash"

printf '%s\n' 'Step 4: land join-a with the prepared candidate statement hash'
"$ledger_bin" --dsn-file "$dsn_file" land --repo "$repo" --migration join-a --generation 1 --commit "$candidate" --path "$source_path" --current "$contract=$base_hash" --changed-contract "$contract=$changed_hash" --head "$candidate" --reviewed-head "$candidate"
"$ledger_bin" --dsn-file "$dsn_file" --json contract show "$repo" "$contract" > "$contract_file"
python3 - "$contract_file" "$contract" "$changed_hash" <<'PY'
import json
import sys

path, contract, changed_hash = sys.argv[1:]
row = json.load(open(path, encoding="utf-8"))
if row.get("name") != contract or row.get("version") != 2 or row.get("statement_hash") != changed_hash:
    raise SystemExit(f"ledger-join.sh: expected {contract} at version 2 with hash {changed_hash}, got {row}")
PY

printf '%s\n' 'Step 5: open join-c at the landed statement hash'
new_path="$source_path.join-c"
"$ledger_bin" --dsn-file "$dsn_file" open "$repo" join-c "$owner" "$candidate" join-c --write "$new_path" --read "$contract=$changed_hash"
"$ledger_bin" --dsn-file "$dsn_file" --json status "$repo" > "$status_c_file"
python3 - "$status_c_file" join-c "$contract" 2 <<'PY'
import json
import sys

status, migration, contract, version = sys.argv[1:]
for row in json.load(open(status, encoding="utf-8"))["Migrations"]:
    if row["id"] == migration and row.get("read_versions", {}).get(contract) == int(version):
        break
else:
    raise SystemExit(f"ledger-join.sh: {migration} does not record {contract} at version {version}")
PY

printf '%s\n' 'Step 6: reject join-b as a stale read'
if "$ledger_bin" --dsn-file "$dsn_file" --json check "$repo" join-b 1 --path "$other_path" --current "$contract=$changed_hash" > "$check_file" 2>&1; then
	printf '%s\n' 'ledger-join.sh: join-b unexpectedly passed the stale-read check' >&2
	exit 1
fi
python3 - "$check_file" "$contract" <<'PY'
import json
import sys

path, contract = sys.argv[1:]
report = json.load(open(path, encoding="utf-8"))
if report.get("pass") or not any(v.get("kind") == "stale_read" and v.get("name") == contract for v in report.get("violations", [])):
    raise SystemExit(f"ledger-join.sh: join-b did not report stale_read for {contract}: {report}")
PY
