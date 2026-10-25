package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

// openReady opens migration id for owner with one declared write and the reads given, readies it at generation 1 with
// the head head-<id> and the ready flags given, and returns the member the landing program builds from it.
func (cli batchCLI) openReady(id, owner, write string, reads map[string]string, flags ...string) map[string]any {
	cli.t.Helper()
	args := []string{"open", "r", id, owner, "base", "branch/" + id, "--write", write}
	for _, name := range sortedMapKeys(reads) {
		args = append(args, "--read", name+"="+reads[name])
	}
	cli.invoke(args...)
	return cli.ready(id, write, reads, flags...)
}

// ready readies open migration id at generation 1 and returns the member the landing program builds from it.
func (cli batchCLI) ready(id, write string, reads map[string]string, flags ...string) map[string]any {
	cli.t.Helper()
	var record struct {
		ReadyEvent int64 `json:"ready_event"`
	}
	cli.decode(cli.invoke(append([]string{"--json", "ready", "r", id, "1", "--head", "head-" + id}, flags...)...), &record)
	current := map[string]string{}
	for name, hash := range reads {
		current[name] = hash
	}
	return map[string]any{"migration": id, "ready_event": record.ReadyEvent, "current_hashes": current, "changed_paths": []string{write}}
}

// admitFields is the admit-batch request of batch id, read at version, for members in that order.
func admitFields(id string, version int64, members ...map[string]any) map[string]any {
	return map[string]any{
		"repo": "r", "id": id, "base": "base-1", "candidate_commit": "candidate-" + id, "candidate_tree": "tree-" + id,
		"members": members, "expected_version": version, "record": cliRecord("publishing"),
	}
}

func (cli batchCLI) admit(fields map[string]any) cliBatch {
	cli.t.Helper()
	var admitted cliBatch
	cli.decode(cli.invoke("admit-batch", "--request-file", cli.request(fields)), &admitted)
	return admitted
}

type cliRefusal struct {
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	Member     string `json:"member"`
	Other      string `json:"other"`
	Violations []struct {
		Kind string `json:"kind"`
	} `json:"violations"`
}

// refused runs args with --json and checks that the command exits 1 with a refusal for reason.
func (cli batchCLI) refused(reason string, args ...string) cliRefusal {
	cli.t.Helper()
	code, stdout, stderr := cli.call(append([]string{"--json"}, args...)...)
	var refusal cliRefusal
	if code != 1 || json.Unmarshal([]byte(stdout), &refusal) != nil || refusal.Reason != reason {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 1 refusing %s", args, code, stdout, stderr, reason)
	}
	return refusal
}

func (cli batchCLI) refusedAdmit(reason string, fields map[string]any) cliRefusal {
	cli.t.Helper()
	return cli.refused(reason, "admit-batch", "--request-file", cli.request(fields))
}

// members lists the migrations of batch id in position order, each with whether it is admitted.
func (cli batchCLI) members(id string) []string {
	cli.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, cli.dsn)
	if err != nil {
		cli.t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT migration || ':' || admitted FROM batch_members WHERE repo = 'r' AND batch = $1 ORDER BY position`, id)
	if err != nil {
		cli.t.Fatal(err)
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		cli.t.Fatal(err)
	}
	if members == nil {
		members = []string{}
	}
	return members
}

// admit-batch admits ready members from their ready records and prints the admitted batch at its new version. batch
// shows it, batches --active lists it, and its members are frozen in the requested order. A retry of the same request
// prints the stored batch and changes nothing; another request is refused with stale_batch from the old version and
// with batch_admitted at the current one.
func TestAdmitBatchCommandAdmitsReadyMembers(t *testing.T) {
	cli := newBatchCLI(t)
	cli.invoke("contract", "register", "r", "C", "alice", "c1", "false")
	first := cli.openReady("M-1", "alice", "a.lean", map[string]string{"C": "c1"}, "--changed-contract", "C=c2", "--review", "C=alice@head-M-1")
	second := cli.openReady("M-2", "bob", "b.lean", nil)
	cli.create("batch-1", cliRecord("checked"))
	request := admitFields("batch-1", 1, second, first)
	admitted := cli.admit(request)
	if admitted.Admission != "admitted" || admitted.RecordVersion != 2 || admitted.Phase != "publishing" ||
		len(admitted.AdmissionDigest) != 64 || !sameRecord(t, admitted.Record, cliRecord("publishing")) {
		t.Fatalf("admit-batch printed %+v", admitted)
	}
	if loaded := cli.load("batch-1"); !reflect.DeepEqual(loaded, admitted) {
		t.Fatalf("batch printed %+v, want the admitted %+v", loaded, admitted)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active = %q", active)
	}
	if members := cli.members("batch-1"); !slices.Equal(members, []string{"M-2:true", "M-1:true"}) {
		t.Fatalf("frozen members = %q", members)
	}
	if got := cli.events("batch-1"); !slices.Equal(got, []string{"batch.created", "batch.admitted"}) {
		t.Fatalf("batch events = %q", got)
	}

	events := len(cli.events(""))
	retry := admitFields("batch-1", 1, second, first)
	retry["record"] = cliRecord("other")
	if replayed := cli.admit(retry); !reflect.DeepEqual(replayed, admitted) {
		t.Fatalf("retry after a lost response printed %+v, want the stored %+v", replayed, admitted)
	}
	cli.assertUnchanged(admitted, events)
	changed := admitFields("batch-1", 1, second, first)
	changed["candidate_tree"] = "tree-2"
	cli.refusedAdmit("stale_batch", changed)
	cli.assertUnchanged(admitted, events)
	changed["expected_version"] = 2
	cli.refusedAdmit("batch_admitted", changed)
	cli.assertUnchanged(admitted, events)
	if members := cli.members("batch-1"); !slices.Equal(members, []string{"M-2:true", "M-1:true"}) {
		t.Fatalf("frozen members after the refusals = %q", members)
	}
}

// A refused admit-batch exits 1 with the refusal on stdout, names the member and Check's reasons, and records nothing:
// an unready member, a member readied again after the program read it, an in-batch reader of a contract that another
// member changes, a member that Check refuses, and an admission from a version that a competing save moved past.
func TestAdmitBatchCommandRefusesWithNothingRecorded(t *testing.T) {
	cli := newBatchCLI(t)
	cli.invoke("contract", "register", "r", "C", "alice", "c1", "false")
	cli.invoke("contract", "register", "r", "P", "carol", "p1", "true")
	changer := cli.openReady("M-1", "alice", "a.lean", map[string]string{"C": "c1"}, "--changed-contract", "C=c2", "--review", "C=alice@head-M-1")
	cli.invoke("open", "r", "M-2", "bob", "base", "branch/M-2", "--write", "b.lean")
	unready := map[string]any{"migration": "M-2", "ready_event": 0, "current_hashes": map[string]string{}, "changed_paths": []string{"b.lean"}}
	readiedAgain := cli.openReady("M-3", "carol", "c.lean", nil)
	cli.ready("M-3", "c.lean", nil, "--compatibility-proof", "M3.compat")
	reader := cli.openReady("M-4", "dave", "d.lean", map[string]string{"C": "c1"})
	unapproved := cli.openReady("M-5", "erin", "e.lean", map[string]string{"P": "p1"}, "--changed-contract", "P=p2", "--review", "P=carol@head-M-5")
	created := cli.create("batch-1", cliRecord("checked")).Batch
	events := len(cli.events(""))
	for _, test := range []struct {
		name, reason, member, other string
		members                     []map[string]any
		kinds                       []string
	}{
		{"an unready member", "not_ready", "M-2", "", []map[string]any{changer, unready}, nil},
		{"a member readied again", "ready_changed", "M-3", "", []map[string]any{changer, readiedAgain}, nil},
		{"an in-batch reader of a changed contract", "batch_conflict", "M-4", "M-1", []map[string]any{changer, reader}, nil},
		{"a member Check refuses", "pinned", "M-5", "", []map[string]any{changer, unapproved}, []string{"pinned"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			refusal := cli.refusedAdmit(test.reason, admitFields("batch-1", 1, test.members...))
			kinds := []string{}
			for _, violation := range refusal.Violations {
				kinds = append(kinds, violation.Kind)
			}
			if refusal.Member != test.member || refusal.Other != test.other || (test.kinds != nil && !slices.Equal(kinds, test.kinds)) {
				t.Fatalf("refusal %+v, want member %q, other %q and reasons %q", refusal, test.member, test.other, test.kinds)
			}
			cli.assertUnchanged(created, events)
			// Without --json, the refusal on stderr names the member, and for a member that Check refuses, its reasons.
			code, _, stderr := cli.call("admit-batch", "--request-file", cli.request(admitFields("batch-1", 1, test.members...)))
			want := []string{"refused (" + test.reason + ")", test.member}
			if test.kinds != nil {
				want = append(want, "member "+test.member+": ")
				for _, kind := range test.kinds {
					want = append(want, kind+": ")
				}
			}
			for _, part := range want {
				if code != 1 || !strings.Contains(stderr, part) {
					t.Fatalf("text refusal: code=%d stderr=%q, want exit 1 with %q", code, stderr, part)
				}
			}
			cli.assertUnchanged(created, events)
			if members := cli.members("batch-1"); len(members) != 0 {
				t.Fatalf("a refused admission recorded members %q", members)
			}
		})
	}

	competing := cli.save("batch-1", cliRecord("publishing"), 1)
	events = len(cli.events(""))
	cli.refusedAdmit("stale_batch", admitFields("batch-1", 1, changer))
	cli.assertUnchanged(competing, events)
	if admitted := cli.admit(admitFields("batch-1", cli.load("batch-1").RecordVersion, changer)); admitted.RecordVersion != 3 || admitted.Admission != "admitted" {
		t.Fatalf("admit-batch after a reload printed %+v", admitted)
	}
}

// While a migration is admitted, takeover, a round that would escalate it and the transfer of a contract it changes
// each exit 1 with batch_admitted and change nothing. After DeclineBatch releases the admission, each succeeds.
func TestAdmitBatchCommandFencesTheAdmittedMigration(t *testing.T) {
	cli := newBatchCLI(t)
	cli.invoke("repo", "ensure", "r", "1")
	cli.invoke("contract", "register", "r", "C", "alice", "c1", "false")
	member := cli.openReady("M-1", "alice", "a.lean", map[string]string{"C": "c1"}, "--changed-contract", "C=c2", "--review", "C=alice@head-M-1")
	cli.create("batch-1", cliRecord("checked"))
	admitted := cli.admit(admitFields("batch-1", 1, member))
	events := len(cli.events(""))
	for _, args := range [][]string{
		{"takeover", "r", "M-1", "bob", "1"},
		{"round", "r", "M-1", "1", "build failed", "--key", "round-1"},
		{"contract", "transfer", "r", "C", "alice", "bob"},
	} {
		cli.refused("batch_admitted", args...)
		cli.assertUnchanged(admitted, events)
	}
	// DeclineBatch releases the admission; the test releases it directly.
	cli.sql(`UPDATE batch_members SET admitted = false WHERE repo = 'r' AND batch = 'batch-1'`)
	cli.setAdmission("batch-1", "declined", "")
	cli.invoke("contract", "transfer", "r", "C", "alice", "bob")
	var round struct {
		LandingRounds int    `json:"landing_rounds"`
		Escalated     string `json:"escalated_at"`
	}
	cli.decode(cli.invoke("--json", "round", "r", "M-1", "1", "build failed", "--key", "round-1"), &round)
	if round.LandingRounds != 1 || round.Escalated == "" {
		t.Fatalf("the round after the release = %+v, want one escalated round", round)
	}
	cli.invoke("takeover", "r", "M-1", "bob", "1")
}

// A malformed admit-batch exits 2 and changes nothing.
func TestAdmitBatchCommandRefusesMalformedInput(t *testing.T) {
	cli := newBatchCLI(t)
	before := cli.create("batch-1", cliRecord("checked")).Batch
	events := len(cli.events(""))
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"repo":"r","id":"batch-1","members":[],"expected_version":1,"record":{},"reviews":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"admit-batch", "r", "batch-1"}, "admit-batch --request-file"},
		{[]string{"admit-batch", "--request-file", path}, "unknown field"},
	} {
		if code, stdout, stderr := cli.call(test.args...); code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
		}
	}
	cli.assertUnchanged(before, events)
}

// admitCommandChildEnv names the database and request file of TestAdmitBatchCommandCrashChild, joined by a newline.
const admitCommandChildEnv = "LEDGER_TEST_ADMIT_COMMAND_CHILD"

// TestAdmitBatchCommandCrashChild is not a test of its own: it runs ledger admit-batch in a child process, then kills
// that process with SIGKILL right after the command returns, before any cleanup.
func TestAdmitBatchCommandCrashChild(t *testing.T) {
	dsn, file, ok := strings.Cut(os.Getenv(admitCommandChildEnv), "\n")
	if !ok {
		t.Skip("run only as the child process of TestAdmitBatchCommandSurvivesAKillAfterTheCommit")
	}
	if code, _, _ := runLedger(dsn, "admit-batch", "--request-file", file); code != 0 {
		os.Exit(code)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// A ledger process killed right after its admit-batch commits leaves the batch admitted with its members, which a new
// ledger process finds, and a repeat of the request prints the stored batch and changes nothing.
func TestAdmitBatchCommandSurvivesAKillAfterTheCommit(t *testing.T) {
	cli := newBatchCLI(t)
	member := cli.openReady("M-1", "alice", "a.lean", nil)
	cli.create("batch-1", cliRecord("checked"))
	file := cli.request(admitFields("batch-1", 1, member))
	child := exec.Command(os.Args[0], "-test.run=^TestAdmitBatchCommandCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), admitCommandChildEnv+"="+cli.dsn+"\n"+file)
	err := child.Run()
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended with %v (%v), want SIGKILL", child.ProcessState, err)
	}

	stored := cli.load("batch-1")
	if stored.Admission != "admitted" || stored.RecordVersion != 2 || !slices.Equal(cli.members("batch-1"), []string{"M-1:true"}) {
		t.Fatalf("after the kill the batch is %+v with members %q", stored, cli.members("batch-1"))
	}
	events := len(cli.events(""))
	var replayed cliBatch
	cli.decode(cli.invoke("admit-batch", "--request-file", file), &replayed)
	if !reflect.DeepEqual(replayed, stored) {
		t.Fatalf("the repeat printed %+v, want the stored %+v", replayed, stored)
	}
	cli.assertUnchanged(stored, events)
}
