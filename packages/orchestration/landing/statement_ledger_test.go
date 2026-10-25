package landing

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// These tests run the runner's statement path through the real ledger binary,
// on a Postgres of their own, and a fixture hasher that follows the hasher
// contract: it reads LAND_NAMES_FILE, checks LAND_NAMES against it, hashes each
// name as the blob of <name>.lean at LAND_TREE in LAND_GIT_DIR, and prints
// name<TAB>hash<TAB>path, with "-" for an absent name. For the candidate it
// requires the kept build in LAND_BUILD_DIR, and its "unable" mode exits 3
// there. Its "retry-<n>" mode exits 75 on its first n calls and its
// "retry-always" mode on every call. It appends each LAND_BUILD_DIR it receives
// to a log.
const fixtureHasher = `set -eu
log="$1"
mode="${2:-}"
test "${LAND_NAMES:-}" = "$(cat "$LAND_NAMES_FILE")"
echo "${LAND_BUILD_DIR:-}" >> "$log"
if [ -n "${LAND_BUILD_DIR:-}" ]; then
	test -f "$LAND_BUILD_DIR/built" || { echo "no build in $LAND_BUILD_DIR" >&2; exit 1; }
	if [ "$mode" = unable ]; then echo "cannot check the candidate" >&2; exit 3; fi
fi
case "$mode" in
retry-always) echo "index database unreachable" >&2; exit 75 ;;
retry-*)
	calls=$(( $(cat "$log.calls" 2>/dev/null || echo 0) + 1 ))
	echo "$calls" > "$log.calls"
	if [ "$calls" -le "${mode#retry-}" ]; then echo "index database unreachable" >&2; exit 75; fi ;;
esac
for name in $(tr -d '[]"' < "$LAND_NAMES_FILE" | tr ',' ' '); do
	if hash=$(git --git-dir="$LAND_GIT_DIR" rev-parse -q --verify "$LAND_TREE:$name.lean"); then
		printf '%s\t%s\t%s\n' "$name" "$hash" "$name.lean"
	else
		printf '%s\t\t-\n' "$name"
	fi
done
`

// statementFixture is a git repository whose base declares contract C in
// C.lean, a ledger database where alice owns C at the base's hash, and the
// ledger binary built from this module.
type statementFixture struct {
	t        *testing.T
	repo     string
	base     string
	baseHash string
	ledger   []string
	hashLog  string
}

func newStatementFixture(t *testing.T) *statementFixture {
	t.Helper()
	dsn := testpg.Database(t, "landing_statements")
	bin := filepath.Join(t.TempDir(), "ledger")
	if output, err := exec.Command("go", "build", "-o", bin, "../cmd/ledger").CombinedOutput(); err != nil {
		t.Fatalf("build ledger: %v\n%s", err, output)
	}
	dsnFile := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	gitRun(t, repo, "config", "user.name", "Test")
	writeSource(t, repo, "C.lean", "def C : Nat := 1\n")
	writeSource(t, repo, "Other.lean", "def other := 0\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-qm", "base")
	f := &statementFixture{
		t:       t,
		repo:    repo,
		base:    gitRun(t, repo, "rev-parse", "HEAD"),
		ledger:  []string{bin, "--dsn-file", dsnFile},
		hashLog: filepath.Join(t.TempDir(), "hasher.log"),
	}
	f.baseHash = gitRun(t, repo, "rev-parse", f.base+":C.lean")
	f.run("repo", "ensure", "R", "0")
	f.run("contract", "register", "R", "C", "alice", f.baseHash, "false")
	return f
}

// run runs the ledger binary and returns its stdout; it fails the test unless
// the command exits 0.
func (f *statementFixture) run(args ...string) string {
	f.t.Helper()
	command := exec.Command(f.ledger[0], append(append([]string(nil), f.ledger[1:]...), args...)...)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		f.t.Fatalf("ledger %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(output)
}

// member commits one file's new contents on the base and opens migration id in
// the ledger: it writes that file and reads C at the base's hash.
func (f *statementFixture) member(id, file, contents string) string {
	f.t.Helper()
	gitRun(f.t, f.repo, "checkout", "-q", "--detach", f.base)
	writeSource(f.t, f.repo, file, contents)
	gitRun(f.t, f.repo, "commit", "-qam", id)
	head := gitRun(f.t, f.repo, "rev-parse", "HEAD")
	f.run("open", "R", id, "alice", f.base, "branch-"+id, "--write", file, "--read", "C="+f.baseHash)
	return head
}

// ready readies a migration with a new key and returns its ready_event.
func (f *statementFixture) ready(id, key, head string, records ...string) int64 {
	f.t.Helper()
	args := append([]string{"--json", "ready", "R", id, "1", "--key", key, "--head", head}, records...)
	var record struct {
		ReadyEvent int64 `json:"ready_event"`
	}
	if err := json.Unmarshal([]byte(f.run(args...)), &record); err != nil || record.ReadyEvent < 1 {
		f.t.Fatalf("ready %s: event %d, %v", id, record.ReadyEvent, err)
	}
	return record.ReadyEvent
}

// readyEvents is the ledger's ready list: each ready migration's ready_event.
func (f *statementFixture) readyEvents() map[string]int64 {
	f.t.Helper()
	var records []struct {
		ID         string `json:"id"`
		ReadyEvent int64  `json:"ready_event"`
	}
	if err := json.Unmarshal([]byte(f.run("--json", "ready-list", "R")), &records); err != nil {
		f.t.Fatal(err)
	}
	events := make(map[string]int64, len(records))
	for _, record := range records {
		events[record.ID] = record.ReadyEvent
	}
	return events
}

func (f *statementFixture) runner(ledger *recordingLedger, hasherMode string) Runner {
	ledger.CommandLedger = CommandLedger{Directory: f.repo, Command: f.ledger}
	hasher := []string{"sh", "-c", fixtureHasher, "hasher", f.hashLog}
	if hasherMode != "" {
		hasher = append(hasher, hasherMode)
	}
	return Runner{
		Store:         &MemoryStore{},
		Ledger:        ledger,
		Builder:       CommandBuilder{RepositoryPath: f.repo, Command: []string{"sh", "-c", "touch built"}},
		Hasher:        CommandHasher{Command: hasher, Directory: f.repo},
		Git:           RepositoryGit{Path: f.repo},
		ConfigDigest:  "digest",
		Toolchain:     func(context.Context, Tree) (string, error) { return "tool", nil },
		EscalateTo:    "steward",
		EscalateAfter: 3,
		RetryBackoff:  time.Millisecond,
	}
}

func (f *statementFixture) runBatch(runner Runner, members ...Member) Batch {
	f.t.Helper()
	batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: "R", Base: f.base, BaseBranch: "trunk", Members: members})
	if err != nil {
		f.t.Fatalf("Run() error = %v", err)
	}
	return batch
}

// recordingLedger is the command ledger with each round and landing recorded.
// beforeReturn runs before a round reaches the ledger. MigrationState reports
// open, since the configured state command is not under test here.
type recordingLedger struct {
	CommandLedger
	rounds       []RoundRequest
	lands        []LandRequest
	beforeReturn func(RoundRequest)
}

func (l *recordingLedger) ReturnMember(ctx context.Context, round RoundRequest) error {
	if l.beforeReturn != nil {
		l.beforeReturn(round)
	}
	l.rounds = append(l.rounds, round)
	return l.CommandLedger.ReturnMember(ctx, round)
}

func (l *recordingLedger) Land(ctx context.Context, request LandRequest) error {
	l.lands = append(l.lands, request)
	return l.CommandLedger.Land(ctx, request)
}

func (l *recordingLedger) MigrationState(context.Context, string, string) (string, string, error) {
	return "open", "", nil
}

func reviewed(f *statementFixture, id, head string) Member {
	return Member{
		Migration:        id,
		Generation:       1,
		Head:             head,
		ReadNames:        []string{"C"},
		ChangedContracts: map[string]string{"C": gitRun(f.t, f.repo, "rev-parse", head+":C.lean")},
		Reviews:          []Review{{Contract: "C", Reviewer: "alice", Head: head}},
	}
}

// A reviewed statement change is derived from the trees, passes the full
// check and lands through the ledger, which advances the contract to the
// candidate's hash. The hasher reads the base without a build directory and
// the candidate with the kept build, which is gone once the hasher exits.
func TestReviewedStatementChangeLands(t *testing.T) {
	f := newStatementFixture(t)
	head := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", head+":C.lean")
	member := reviewed(f, "M-change", head)
	member.ReadyEvent = f.ready("M-change", "k-1", head, "--changed-contract", "C="+changed, "--review", "C=alice@"+head)
	ledger := &recordingLedger{}
	runner := f.runner(ledger, "")

	batch := f.runBatch(runner, member)
	if batch.State != Green || len(batch.ActiveMembers) != 1 || len(ledger.rounds) != 0 {
		t.Fatalf("batch = %s (%s), active %d, rounds %+v; want green with the member", batch.State, batch.Reason, len(batch.ActiveMembers), ledger.rounds)
	}
	if got := batch.ActiveMembers[0].DerivedContracts["C"]; got != changed {
		t.Fatalf("derived C = %q, want the candidate's hash %q", got, changed)
	}
	if got := batch.ActiveMembers[0].CurrentHashes["C"]; got != f.baseHash {
		t.Fatalf("current C = %q, want the base's hash %q for a read the member changes", got, f.baseHash)
	}
	assertBuildDirectoriesGone(t, f.hashLog)

	runner.GitHub = &githubFake{wait: PullRequest{Number: 1, Head: batch.Candidate.Commit, MergedCommit: batch.Candidate.Commit, State: PullRequestMerged}}
	landed, err := runner.Publish(context.Background(), batch)
	if err != nil || landed.State != Landed {
		t.Fatalf("Publish() = %s (%s), %v; want landed", landed.State, landed.Reason, err)
	}
	var contract struct {
		StatementHash string `json:"statement_hash"`
		Version       int64  `json:"version"`
	}
	if err := json.Unmarshal([]byte(f.run("contract", "show", "R", "C")), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.StatementHash != changed || contract.Version != 2 {
		t.Fatalf("contract C = %+v, want hash %q at version 2", contract, changed)
	}
}

// An unreviewed statement change is returned with statement_unreviewed, and
// nothing is landed for it.
func TestUnreviewedStatementChangeIsReturned(t *testing.T) {
	f := newStatementFixture(t)
	head := f.member("M-unreviewed", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", head+":C.lean")
	member := Member{Migration: "M-unreviewed", Generation: 1, Head: head, ReadNames: []string{"C"}, ChangedContracts: map[string]string{"C": changed}}
	member.ReadyEvent = f.ready("M-unreviewed", "k-1", head, "--changed-contract", "C="+changed)
	ledger := &recordingLedger{}

	batch := f.runBatch(f.runner(ledger, ""), member)
	if len(ledger.rounds) != 1 || ledger.rounds[0].Reason != "statement_unreviewed" {
		t.Fatalf("rounds = %+v, want one statement_unreviewed return", ledger.rounds)
	}
	if len(batch.ActiveMembers) != 0 || len(ledger.lands) != 0 {
		t.Fatalf("active %+v, lands %+v; want the member returned and nothing landed", batch.ActiveMembers, ledger.lands)
	}
}

// A statement change the member does not declare is returned with
// undeclared_statement_change and both sets in the round's detail. The round
// carries the ready_event the batch read: a member returned without a newer
// submission leaves the ready list, and one whose owner readied it again with
// a new key before the return stays ready at its new event.
func TestUndeclaredStatementChangeIsReturnedWithItsReadyEvent(t *testing.T) {
	f := newStatementFixture(t)
	quietHead := f.member("M-quiet", "C.lean", "def C : Int := 1\n")
	quiet := Member{Migration: "M-quiet", Generation: 1, Head: quietHead, ReadNames: []string{"C"}}
	quiet.ReadyEvent = f.ready("M-quiet", "k-quiet-1", quietHead)
	ledger := &recordingLedger{}
	var again int64
	ledger.beforeReturn = func(RoundRequest) { again = f.ready("M-quiet", "k-quiet-2", quietHead) }

	f.runBatch(f.runner(ledger, ""), quiet)
	if len(ledger.rounds) != 1 {
		t.Fatalf("rounds = %+v, want one", ledger.rounds)
	}
	round := ledger.rounds[0]
	if round.Reason != "undeclared_statement_change" || round.Detail != "declared []; derived [C]" {
		t.Fatalf("round = %+v, want undeclared_statement_change with both sets", round)
	}
	if round.ReadyEvent != quiet.ReadyEvent || round.Key != "batch/M-quiet/1" || round.Batch != "batch" {
		t.Fatalf("round = %+v, want ready_event %d and key batch/M-quiet/1", round, quiet.ReadyEvent)
	}
	if got := f.readyEvents()["M-quiet"]; got != again || again <= quiet.ReadyEvent {
		t.Fatalf("ready_event after the return = %d, want the newer submission %d", got, again)
	}

	g := newStatementFixture(t)
	head := g.member("M-once", "C.lean", "def C : Int := 1\n")
	once := Member{Migration: "M-once", Generation: 1, Head: head, ReadNames: []string{"C"}}
	once.ReadyEvent = g.ready("M-once", "k-once-1", head)
	g.runBatch(g.runner(&recordingLedger{}, ""), once)
	if _, ready := g.readyEvents()["M-once"]; ready {
		t.Fatal("M-once is still ready after its round, want its ready record cleared")
	}
}

// A member that reads a contract another member of the batch changes is
// returned with stale_read, and the changing member stays.
func TestReadOfContractAnotherMemberChangesIsStale(t *testing.T) {
	f := newStatementFixture(t)
	changeHead := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", changeHead+":C.lean")
	change := reviewed(f, "M-change", changeHead)
	change.ReadyEvent = f.ready("M-change", "k-change-1", changeHead, "--changed-contract", "C="+changed, "--review", "C=alice@"+changeHead)
	readerHead := f.member("M-reader", "Other.lean", "def other := C\n")
	reader := Member{Migration: "M-reader", Generation: 1, Head: readerHead, ReadNames: []string{"C"}}
	reader.ReadyEvent = f.ready("M-reader", "k-reader-1", readerHead)
	ledger := &recordingLedger{}

	batch := f.runBatch(f.runner(ledger, ""), change, reader)
	if len(ledger.rounds) != 1 || ledger.rounds[0].ID != "M-reader" || ledger.rounds[0].Reason != "stale_read" {
		t.Fatalf("rounds = %+v, want M-reader returned with stale_read", ledger.rounds)
	}
	if batch.State != Green || len(batch.ActiveMembers) != 1 || batch.ActiveMembers[0].Migration != "M-change" {
		t.Fatalf("batch = %s (%s) with %+v, want green with M-change", batch.State, batch.Reason, batch.ActiveMembers)
	}
}

// A candidate hasher exit 3 stops the batch as unable to check, returns no
// member, and removes the kept build.
func TestCandidateHasherUnableStopsTheBatch(t *testing.T) {
	f := newStatementFixture(t)
	head := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", head+":C.lean")
	member := reviewed(f, "M-change", head)
	member.ReadyEvent = f.ready("M-change", "k-1", head, "--changed-contract", "C="+changed, "--review", "C=alice@"+head)
	ledger := &recordingLedger{}

	batch := f.runBatch(f.runner(ledger, "unable"), member)
	if batch.State != Failed || batch.Reason != "hash-unable" {
		t.Fatalf("batch = %s (%s), want failed hash-unable", batch.State, batch.Reason)
	}
	if len(ledger.rounds) != 0 || len(batch.Dropped) != 0 {
		t.Fatalf("rounds %+v, dropped %+v; want no member returned", ledger.rounds, batch.Dropped)
	}
	if _, ready := f.readyEvents()["M-change"]; !ready {
		t.Fatal("M-change left the ready list, want it still ready")
	}
	assertBuildDirectoriesGone(t, f.hashLog)
}

// assertBuildDirectoriesGone checks the hasher log: the base was hashed with
// no build directory, the candidate with one, and none of those directories
// is left.
func assertBuildDirectoriesGone(t *testing.T, log string) {
	t.Helper()
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var empty, kept int
	for _, directory := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if directory == "" {
			empty++
			continue
		}
		kept++
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatalf("build directory %s still exists after the hasher exited (%v)", directory, err)
		}
	}
	if empty == 0 || kept == 0 {
		t.Fatalf("hasher calls: %d without a build directory, %d with one; want both", empty, kept)
	}
}

// A hasher exit 75 (the index database cannot be reached) is retried with
// backoff: a hasher that exits 75 twice and then answers lands the batch.
func TestHasherRetryThenLands(t *testing.T) {
	f := newStatementFixture(t)
	head := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", head+":C.lean")
	member := reviewed(f, "M-change", head)
	member.ReadyEvent = f.ready("M-change", "k-1", head, "--changed-contract", "C="+changed, "--review", "C=alice@"+head)
	ledger := &recordingLedger{}
	runner := f.runner(ledger, "retry-2")

	batch := f.runBatch(runner, member)
	if batch.State != Green || len(batch.ActiveMembers) != 1 || len(ledger.rounds) != 0 || batch.HashAttempts != 0 {
		t.Fatalf("batch = %s (%s), active %d, rounds %+v, attempts %d; want green with the member", batch.State, batch.Reason, len(batch.ActiveMembers), ledger.rounds, batch.HashAttempts)
	}
	runner.GitHub = &githubFake{wait: PullRequest{Number: 1, Head: batch.Candidate.Commit, MergedCommit: batch.Candidate.Commit, State: PullRequestMerged}}
	landed, err := runner.Publish(context.Background(), batch)
	if err != nil || landed.State != Landed {
		t.Fatalf("Publish() = %s (%s), %v; want landed", landed.State, landed.Reason, err)
	}
	if items := f.run("items", "R", "--owner", "steward"); strings.Contains(items, "landing:") {
		t.Fatalf("escalations = %s, want none", items)
	}
	assertBuildDirectoriesGone(t, f.hashLog)
}

// A hasher that always exits 75 escalates once, at the threshold, to the
// escalations owner. The batch neither fails nor returns a member: it stays
// active for a later retry, and its member stays ready.
func TestHasherAlwaysRetryEscalatesAtThreshold(t *testing.T) {
	f := newStatementFixture(t)
	head := f.member("M-change", "C.lean", "def C : Int := 1\n")
	changed := gitRun(t, f.repo, "rev-parse", head+":C.lean")
	member := reviewed(f, "M-change", head)
	member.ReadyEvent = f.ready("M-change", "k-1", head, "--changed-contract", "C="+changed, "--review", "C=alice@"+head)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ledger := &recordingLedger{}
	runner := f.runner(ledger, "retry-always")

	batch := f.runBatch(runner, member)
	if batch.State == Failed || batch.Reason != "hash-retry" || batch.HashAttempts != 3 {
		t.Fatalf("batch = %s (%s), attempts %d; want active hash-retry after 3 attempts", batch.State, batch.Reason, batch.HashAttempts)
	}
	if len(ledger.rounds) != 0 || len(batch.Dropped) != 0 || len(batch.ActiveMembers) != 1 {
		t.Fatalf("rounds %+v, dropped %+v; want no member returned", ledger.rounds, batch.Dropped)
	}
	if _, ready := f.readyEvents()["M-change"]; !ready {
		t.Fatal("M-change left the ready list, want it still ready")
	}
	if items := f.run("items", "R", "--owner", "steward"); !strings.Contains(items, `"landing:batch:hash@3"`) {
		t.Fatalf("escalations = %s, want landing:batch:hash@3", items)
	}
	active, err := runner.Store.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("active batches = %+v, %v; want the batch still active", active, err)
	}
	resumed, err := runner.Reconcile(context.Background())
	if err != nil || len(resumed) != 1 || resumed[0].Reason != "hash-retry" || resumed[0].HashAttempts != 4 {
		t.Fatalf("Reconcile() = %+v, %v; want one more attempt and still hash-retry", resumed, err)
	}
	if items := f.run("items", "R", "--owner", "steward"); strings.Count(items, "landing:batch:hash@") != 1 {
		t.Fatalf("escalations = %s, want exactly one", items)
	}
	if left, _ := filepath.Glob(filepath.Join(scratch, "landing-build-*")); len(left) != 0 {
		t.Fatalf("build directories left: %v", left)
	}
}
