package landing_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	. "github.com/sjawhar/legion/orchestration/landing"
)

// stepOrderBuild is red, with a log that names Other's duplicate declaration, while the candidate holds Broken.lean,
// and green otherwise, leaving the file that the fixture hasher reads in the kept build.
const stepOrderBuild = `if [ -f Broken.lean ]; then
	printf '✖ [1/1] Building Other\nerror: Other.lean:1:1: environment already contains '"'"'other'"'"'\n'
	exit 1
fi
touch built`

// stepOrderCheck is a configured check that refuses Check.lean while the candidate tree holds it.
const stepOrderCheck = `if git ls-tree --name-only "$LAND_TREE" -- Check.lean | grep -q .; then
	printf 'Check.lean\tP1: Check.lean is not allowed\n'
fi`

// The runner on the ledger store takes each candidate through decision 4's steps in this order: the deferred Check,
// the configured checks, the candidate build, the two hasher calls, the statement derivation, and the full Check. Each
// step returns one member of the batch, so the order of the ledger's rounds is the order of the steps:
//   - M-deferred writes a path it did not declare: the deferred Check refuses it with undeclared_write;
//   - M-check adds Check.lean: the configured check refuses it;
//   - M-build adds a second declaration of other: the build is red, and blame returns it;
//   - M-derive adds E.lean, which declares registered contract E, and declares no statement change: the derivation
//     returns it with undeclared_statement_change;
//   - M-reader reads contract C, which M-change changes: the full Check refuses it with stale_read;
//   - M-change, a reviewed change of C, is green, and the batch lands it.
//
// The hasher exits 75 on its first two calls, at the first green build: the runner retries it, the batch neither
// fails nor escalates, and its attempt count is back to 0 once the hasher answers.
func TestRunnerOnTheStoreRunsTheStepsInOrder(t *testing.T) {
	ctx := context.Background()
	f := newStatementFixture(t)
	f.run("contract", "register", "R", "E", "alice", "e-unregistered-tree", "false")

	changeHead := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", changeHead+":C.lean")
	change := reviewed(f, "M-change", changeHead)
	change.ReadyEvent = f.ready("M-change", "k-change", changeHead, "--changed-contract", "C="+changed, "--review", "C=alice@"+changeHead)

	// commit adds file on the base and returns the commit; the fixture's member helper commits edits of tracked
	// files only.
	commit := func(id, file, contents string) string {
		gitRun(t, f.repo, "checkout", "-q", "--detach", f.base)
		writeSource(t, f.repo, file, contents)
		gitRun(t, f.repo, "add", file)
		gitRun(t, f.repo, "commit", "-qm", id)
		return gitRun(t, f.repo, "rev-parse", "HEAD")
	}
	deferredHead := commit("M-deferred", "Undeclared.lean", "def undeclared := 0\n")
	f.run("open", "R", "M-deferred", "alice", f.base, "branch-M-deferred", "--write", "Declared.lean", "--read", "C="+f.baseHash)

	plain := func(id, file, contents string) Member {
		head := commit(id, file, contents)
		f.run("open", "R", id, "alice", f.base, "branch-"+id, "--write", file, "--read", "C="+f.baseHash)
		return Member{Migration: id, Generation: 1, Head: head, ReadNames: []string{"C"}, ReadyEvent: f.ready(id, "k-"+id, head)}
	}
	deferred := Member{Migration: "M-deferred", Generation: 1, Head: deferredHead, ReadNames: []string{"C"},
		ReadyEvent: f.ready("M-deferred", "k-M-deferred", deferredHead)}
	checked := plain("M-check", "Check.lean", "def checked := 0\n")
	broken := plain("M-build", "Broken.lean", "def other := 1\n")
	derive := plain("M-derive", "E.lean", "def E : Nat := 2\n")
	reader := plain("M-reader", "Reader.lean", "def reader := C\n")

	ledger := &recordingLedger{}
	runner := f.runner(ledger, "retry-2")
	runner.Builder = CommandBuilder{RepositoryPath: f.repo, Command: []string{"sh", "-c", stepOrderBuild}}
	runner.MaxRounds = 4
	runner.Checks = []Checker{CommandChecker{CheckName: "policy", Command: []string{"sh", "-c", stepOrderCheck}, Directory: f.repo}}

	batch := f.runBatch(runner, deferred, checked, broken, derive, reader, change)

	want := []struct{ id, reason string }{
		{"M-deferred", "undeclared_write"},
		{"M-check", "P1: Check.lean is not allowed"},
		{"M-build", "build: "},
		{"M-derive", "undeclared_statement_change"},
		{"M-reader", "stale_read"},
	}
	if len(ledger.rounds) != len(want) {
		t.Fatalf("rounds = %+v, want one return at each of %d steps (batch %s: %s)", ledger.rounds, len(want), batch.State, batch.Reason)
	}
	for index, step := range want {
		round := ledger.rounds[index]
		if round.ID != step.id || !strings.HasPrefix(round.Reason, step.reason) {
			t.Fatalf("round %d = %s (%s), want %s returned with %q: the steps ran out of order", index, round.ID, round.Reason, step.id, step.reason)
		}
		if round.Key != "batch/"+step.id+"/1" || round.ReadyEvent < 1 {
			t.Fatalf("round %d = %+v, want the batch's key and the member's frozen ready event", index, round)
		}
	}
	if batch.State != Green || len(batch.ActiveMembers) != 1 || batch.ActiveMembers[0].Migration != "M-change" {
		t.Fatalf("batch = %s (%s) with %+v, want green with M-change", batch.State, batch.Reason, batch.ActiveMembers)
	}

	body, err := os.ReadFile(f.hashLog + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	if calls, err := strconv.Atoi(strings.TrimSpace(string(body))); err != nil || calls < 3 {
		t.Fatalf("hasher calls = %q, want the two calls that exited 75 and the ones after", body)
	}
	if batch.HashAttempts != 0 || strings.Contains(f.run("items", "R", "--owner", "steward"), "landing:") {
		t.Fatalf("hash attempts %d, items %s; want the retried hasher to leave no count and no escalation", batch.HashAttempts, f.run("items", "R", "--owner", "steward"))
	}
	assertBuildDirectoriesGone(t, f.hashLog)

	runner.GitHub = &mergedGitHub{head: batch.Candidate.Commit}
	landed, err := runner.Publish(ctx, batch)
	if err != nil || landed.State != Landed {
		t.Fatalf("Publish() = %s (%s), %v; want landed", landed.State, landed.Reason, err)
	}
	for _, id := range []string{"M-deferred", "M-check", "M-build", "M-derive", "M-reader"} {
		if m, err := f.store.Migration(ctx, "R", id); err != nil || m.State != "open" || m.LandingRounds != 1 {
			t.Fatalf("returned member %s = %+v (%v), want open with one round", id, m, err)
		}
	}
	if m, err := f.store.Migration(ctx, "R", "M-change"); err != nil || m.State != "landed" {
		t.Fatalf("M-change = %+v (%v), want landed", m, err)
	}
}
