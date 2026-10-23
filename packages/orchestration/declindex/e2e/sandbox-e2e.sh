#!/bin/sh
# Runs E4 of the INFRA-145 plan. On the host this script only records Docker metadata and starts one-shot
# containers; the fixture build, the index refresh and every gate probe run inside them.
set -eu

usage() {
	printf '%s\n' 'usage: sandbox-e2e.sh --image IMAGE --out RUN-DIRECTORY' >&2
	exit 2
}

image=
out=
while [ "$#" -gt 0 ]; do
	case "$1" in
	--image)
		[ "$#" -ge 2 ] || usage
		image=$2
		shift 2
		;;
	--out)
		[ "$#" -ge 2 ] || usage
		out=$2
		shift 2
		;;
	*)
		usage
		;;
	esac
done
[ -n "$image" ] && [ -n "$out" ] || usage
[ -d "$out" ] || {
	printf '%s\n' "sandbox-e2e.sh: output directory does not exist: $out" >&2
	exit 2
}
out=$(CDPATH= cd -- "$out" && pwd)
work="$out/work"
if [ -e "$work" ] || [ -e "$out/sandbox.tsv" ]; then
	printf '%s\n' "sandbox-e2e.sh: $out already holds a run; give a fresh directory" >&2
	exit 2
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$script_dir/../../../.." && pwd)
profile="$script_dir/deny-landlock.json"
[ -r "$profile" ] || {
	printf '%s\n' "sandbox-e2e.sh: deny profile does not exist: $profile" >&2
	exit 2
}

docker image inspect "$image" --format '{{.Id}}' > "$out/image-id.txt"
docker version --format '{{json .}}' > "$out/docker-version.json"
mkdir "$work"
printf '%s\n' 'case	exit	expected	witness	verdict' > "$out/sandbox.tsv"

# Every container runs as the invoking user, so the run directory stays the user's. The run's scratch is /e2e.
run() {
	name=$1
	shift
	docker run --rm --init --interactive --name "declindex-e2e-$name-$$" --cpus 4 \
		--user "$(id -u):$(id -g)" -e HOME=/tmp -e GIT_OPTIONAL_LOCKS=0 \
		--mount "type=bind,src=$repo,dst=/repo,readonly" \
		--mount "type=bind,src=$out,dst=/out" \
		--mount "type=bind,src=$work,dst=/e2e" \
		"$@"
}

# The fixture tree is built and indexed once. Each probe gets its own copy of the index and no exporter, so the
# deny-landlock case can show that the gate created none.
run prepare "$image" sh -s <<'CONTAINER'
set -eu
uname -r > /out/uname-r.txt
lean --version > /out/lean-version.txt
cp -R /repo/packages/orchestration/declindex/testdata/lean /e2e/tree
cd /e2e/tree
printf '%s\n' 'name = "declindex_e2e"' 'defaultTargets = ["Fixture"]' '' '[[lean_lib]]' 'name = "Fixture"' \
	'globs = ["Fixture", "Copies"]' > lakefile.toml
printf '%s\n' '.lake/' > .gitignore
mkdir -p registry/concepts
printf '%s\n' 'version: 2' > registry/registry.yaml
lake build
git init -q
git add .
git -c user.name=declindex-e2e -c user.email=declindex-e2e@example.invalid commit -qm fixture
test -z "$(git status --porcelain)"
for name in seed default deny; do
	printf '%s\n' 'tree: tree' 'lib: .lake/build/lib/lean' "db: $name/index.sqlite" 'roots: [Fixture, Copies]' \
		'registry: registry' 'jobs: 1' 'lean:' '  export: true' '  jobs: 1' > "/e2e/$name.yaml"
done
declindex --config /e2e/seed.yaml refresh
for name in default deny; do
	mkdir "/e2e/$name"
	cp /e2e/seed/index.sqlite /e2e/seed/index.sqlite.graph "/e2e/$name/"
done
CONTAINER

# Proposals and the record of one case. The hostile ones would write a witness file below /out.
cases_lib=$(cat <<'LIB'
proposal() {
	python3 - "$1" "$2" > /e2e/proposal.json <<'PY'
import json
import sys

case, name = sys.argv[1:]
source = {
    "eval": '#eval IO.FS.writeFile "/out/pwned-eval" "pwned"\ndef ' + name + ' : Prop := True\n',
    "char": 'def quote : Char := \'"\'\n#eval IO.FS.writeFile "/out/pwned-eval" "pwned"\n-- \'"\'\ndef ' + name + ' : Prop := True\n',
    "elab": 'def run : Nat := by_elab do IO.FS.writeFile "/out/pwned-elab" "pwned"; return Lean.mkNatLit 1\ndef ' + name + ' : Prop := True\n',
    "benign": 'def ' + name + ' : Prop := True\n',
}[case]
print(json.dumps({"id": "e2e-" + case, "concept": {"name": name, "home": name + ".lean", "meaning": "A sandbox acceptance fixture."}, "source": source}))
PY
}

witness() {
	if [ -e /out/pwned-eval ] || [ -e /out/pwned-elab ]; then
		printf present
	else
		printf absent
	fi
}

record() {
	verdict=PASS
	[ "$2" -eq "$3" ] && [ "$4" = absent ] && [ "$5" = ok ] || verdict=FAIL
	printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$verdict" >> /out/sandbox.tsv
}
LIB
)

# (a), (b) and (d): Docker's default seccomp profile, with the fixture tree mounted read-only.
run default --mount "type=bind,src=$work/tree,dst=/e2e/tree,readonly" "$image" sh -s <<CONTAINER
set -eu
$cases_lib
for c in eval:E2EEval:3 char:E2EChar:3 elab:E2EElab:3 benign:E2EBenign:0; do
	case_name=\${c%%:*}
	rest=\${c#*:}
	name=\${rest%%:*}
	expected=\${rest#*:}
	proposal "\$case_name" "\$name"
	set +e
	declindex --config /e2e/default.yaml --json gate --proposer function:alloc_mnt_ns /e2e/proposal.json \
		> "/out/\$case_name.stdout" 2> "/out/\$case_name.stderr"
	code=\$?
	set -e
	extra=ok
	if [ "\$case_name" = benign ]; then
		git -C /e2e/tree status --porcelain > /out/benign.tree-status
		[ ! -s /out/benign.tree-status ] && grep -q '"added"' /out/benign.stdout || extra=bad
	fi
	record "\$case_name" "\$code" "\$expected" "\$(witness)" "\$extra"
done
CONTAINER

# (c): the same image with the Landlock syscalls failing with ENOSYS.
run deny-landlock --security-opt "seccomp=$profile" --mount "type=bind,src=$work/tree,dst=/e2e/tree,readonly" \
	"$image" sh -s <<CONTAINER
set -eu
$cases_lib
proposal benign E2EDenyLandlock
set +e
strace -f -e trace=execve,execveat -o /out/deny-landlock.strace \
	declindex --config /e2e/deny.yaml --json gate --proposer function:alloc_mnt_ns /e2e/proposal.json \
	> /out/deny-landlock.stdout 2> /out/deny-landlock.stderr
code=\$?
set -e
execs=\$(grep -Ec 'execve(at)?\\(' /out/deny-landlock.strace || true)
printf '%s\n' "\$execs" > /out/deny-landlock.execs
extra=ok
if ! grep -q Landlock /out/deny-landlock.stderr || [ "\$execs" -ne 1 ] || [ -e /e2e/deny/exporter ]; then
	extra=bad
fi
record deny-landlock "\$code" 3 "\$(witness)" "\$extra"
CONTAINER

cat "$out/sandbox.tsv"
rows=$(($(wc -l < "$out/sandbox.tsv") - 1))
if [ "$rows" -ne 5 ] || grep -q 'FAIL$' "$out/sandbox.tsv"; then
	printf '%s\n' "sandbox-e2e.sh: $rows of 5 cases recorded, or a case failed" >&2
	exit 1
fi
