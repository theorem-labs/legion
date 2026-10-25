package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// listingStore is a MemoryStore that logs every batch id it reads or writes and, when listAll is set, lists every
// batch, terminal ones included, as a ledger lists a terminal batch with work left.
type listingStore struct {
	MemoryStore
	listAll bool
	touched *[]string
}

func (s *listingStore) Save(ctx context.Context, batch *Batch) error {
	*s.touched = append(*s.touched, "save "+batch.ID)
	return s.MemoryStore.Save(ctx, batch)
}

func (s *listingStore) Load(ctx context.Context, id string) (Batch, bool, error) {
	*s.touched = append(*s.touched, "load "+id)
	return s.MemoryStore.Load(ctx, id)
}

func (s *listingStore) ListActive(ctx context.Context) ([]Batch, error) {
	if !s.listAll {
		return s.MemoryStore.ListActive(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Batch, 0, len(s.batches))
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if batch, found := s.batches[id]; found {
			out = append(out, cloneBatch(batch))
		}
	}
	return out, nil
}

// readyLedger is a batch ledger whose ready list is fixed and which logs the ids of the batches it is called for.
type readyLedger struct {
	ready   []Member
	touched *[]string
}

func (l *readyLedger) Admit(_ context.Context, batch *Batch, _ []AdmitMember) error {
	*l.touched = append(*l.touched, "admit "+batch.ID)
	return nil
}
func (l *readyLedger) LandBatch(_ context.Context, batch *Batch, _ string) error {
	*l.touched = append(*l.touched, "land "+batch.ID)
	return nil
}
func (l *readyLedger) DeclineBatch(_ context.Context, batch *Batch, _ string, _ []Return, _ string) error {
	*l.touched = append(*l.touched, "decline "+batch.ID)
	return nil
}
func (l *readyLedger) BeginWithdraw(_ context.Context, _, batch string) error {
	*l.touched = append(*l.touched, "withdraw "+batch)
	return nil
}
func (l *readyLedger) EndWithdraw(_ context.Context, _, batch string) error {
	*l.touched = append(*l.touched, "withdrawn "+batch)
	return nil
}
func (l *readyLedger) ReadyMembers(context.Context, string) ([]Member, error) {
	return cloneMembers(l.ready), nil
}

func restartFixture(ready ...Member) (*listingStore, *readyLedger, *builderFake, Runner, *[]string) {
	touched := &[]string{}
	store := &listingStore{touched: touched}
	batches := &readyLedger{ready: ready, touched: touched}
	builder := &builderFake{results: map[string]blame.BuildResult{}}
	runner := testRunner(&ledgerFake{}, builder)
	runner.Store = store
	runner.Admission = batches
	return store, batches, builder, runner, touched
}

func seed(t *testing.T, store *listingStore, batch Batch) Batch {
	t.Helper()
	if _, err := store.MemoryStore.Create(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
	return batch
}

// A restart never advances a terminal batch, even when the store lists it: the batch is returned as it is stored,
// nothing is saved, built or sent to the ledger.
func TestRestartDoesNotAdvanceATerminalBatch(t *testing.T) {
	for _, state := range []State{Landed, Failed, Empty, LandedUnrecorded} {
		t.Run(string(state), func(t *testing.T) {
			store, _, builder, runner, touched := restartFixture()
			store.listAll = true
			stored := seed(t, store, Batch{
				ID: "a", Repository: "repo", Base: "base", BaseBranch: "trunk",
				FrozenMembers: []Member{member("one")}, ActiveMembers: []Member{member("one")}, State: state,
			})

			resumed, err := runner.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(resumed) != 1 || resumed[0].State != state || resumed[0].RecordVersion != stored.RecordVersion {
				t.Fatalf("resumed = %+v, want the %s batch at version %d", resumed, state, stored.RecordVersion)
			}
			if len(*touched) != 0 || len(builder.trees) != 0 {
				t.Fatalf("a restart of a %s batch touched %q and built %q", state, *touched, builder.trees)
			}
			if after, _, _ := store.MemoryStore.Load(context.Background(), "a"); after.RecordVersion != stored.RecordVersion {
				t.Fatalf("record version = %d, want %d", after.RecordVersion, stored.RecordVersion)
			}
		})
	}
}

// A restart that finds a declined batch whose successor does not exist creates the successor from the parent's
// members that are ready now, in ready order and at their current ready records, and stops there. A member that the
// decline returned and its owner has not readied again stays out, a ready migration that was not the parent's stays
// out, and the parent is never saved again. The next restart lists the successor and not the parent.
func TestRestartCreatesTheMissingSuccessorFromTheReadyMembersOnly(t *testing.T) {
	readyC := Member{Migration: "c", Generation: 2, Head: "c-head", ReadyEvent: 41}
	readyB := Member{Migration: "b", Generation: 1, Head: "b-head", ReadyEvent: 42}
	outsider := Member{Migration: "x", Generation: 1, Head: "x-head", ReadyEvent: 43}
	store, _, builder, runner, touched := restartFixture(readyC, outsider, readyB)
	frozen := []Member{
		{Migration: "a", Generation: 1, Head: "a-head", ReadyEvent: 10},
		{Migration: "b", Generation: 1, Head: "b-head", ReadyEvent: 11},
		{Migration: "c", Generation: 1, Head: "c-head", ReadyEvent: 12},
	}
	parent := seed(t, store, Batch{
		ID: "p", Repository: "repo", Base: "base-1", BaseBranch: "trunk",
		FrozenMembers: frozen, ActiveMembers: cloneMembers(frozen[1:]),
		Dropped: []DroppedMember{{Migration: "a", Reason: "stale_read"}}, State: Declined, Successor: "p-next",
		Admission: AdmissionDeclined,
	})

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(resumed) != 1 || resumed[0].ID != "p-next" {
		t.Fatalf("resumed = %+v, want the successor p-next", resumed)
	}
	successor, found, _ := store.MemoryStore.Load(context.Background(), "p-next")
	if !found {
		t.Fatal("the successor was not created")
	}
	if successor.State != Frozen || successor.Repository != "repo" || successor.Base != "base-1" || successor.BaseBranch != "trunk" {
		t.Fatalf("successor = %+v, want frozen on the parent's repository, base and base branch", successor)
	}
	want := []Member{readyC, readyB}
	if !sameMembers(successor.FrozenMembers, want) || !sameMembers(successor.ActiveMembers, want) {
		t.Fatalf("successor members = %+v / %+v, want the ready c and b", successor.FrozenMembers, successor.ActiveMembers)
	}
	for index, member := range successor.ActiveMembers {
		if member.ReadyEvent != want[index].ReadyEvent {
			t.Fatalf("member %s ready event = %d, want the current %d", member.Migration, member.ReadyEvent, want[index].ReadyEvent)
		}
	}
	if successor.Candidate.Tree != "tree-c+b" {
		t.Fatalf("successor candidate = %+v, want the frozen candidate of c and b", successor.Candidate)
	}
	if after, _, _ := store.MemoryStore.Load(context.Background(), "p"); after.RecordVersion != parent.RecordVersion || after.State != Declined {
		t.Fatalf("the parent became %+v, want it untouched at version %d", after, parent.RecordVersion)
	}
	for _, call := range *touched {
		if call == "save p" {
			t.Fatalf("the declined parent was saved again: %q", *touched)
		}
	}
	if len(builder.trees) != 0 {
		t.Fatalf("the restart built %q, want it to stop after creating the successor", builder.trees)
	}
	active, _ := store.ListActive(context.Background())
	if len(active) != 1 || active[0].ID != "p-next" {
		t.Fatalf("listed after the restart = %+v, want only the successor", active)
	}
}

// When none of a declined batch's members is ready, the restart creates its successor straight in Empty, with the
// reason "no ready members", so the parent stops being listed and nothing is listed at all.
func TestRestartEndsTheSuccessorEmptyWhenNoMemberIsReady(t *testing.T) {
	store, _, builder, runner, _ := restartFixture(Member{Migration: "x", Generation: 1, Head: "x-head", ReadyEvent: 5})
	seed(t, store, Batch{
		ID: "p", Repository: "repo", Base: "base-1", BaseBranch: "trunk",
		FrozenMembers: []Member{member("a")}, State: Declined, Successor: "p-next", Admission: AdmissionDeclined,
	})

	if _, err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	successor, found, _ := store.MemoryStore.Load(context.Background(), "p-next")
	if !found || successor.State != Empty || successor.Reason != "no ready members" || len(successor.ActiveMembers) != 0 {
		t.Fatalf("successor = %+v (found %v), want it empty with no ready members", successor, found)
	}
	if active, _ := store.ListActive(context.Background()); len(active) != 0 {
		t.Fatalf("listed after the restart = %+v, want nothing", active)
	}
	if again, err := runner.Reconcile(context.Background()); err != nil || len(again) != 0 || len(builder.trees) != 0 {
		t.Fatalf("a second restart = %+v, %v, built %q; want nothing done", again, err, builder.trees)
	}
}

// A restart resumes an admitted batch in Publishing before any other batch, so a withdraw that a crash interrupted
// finishes before anything else is built or saved.
func TestRestartResumesAnAdmittedPublishingBatchFirst(t *testing.T) {
	store, _, _, runner, touched := restartFixture()
	runner.GitHub = &orderedGitHub{touched: touched}
	seed(t, store, Batch{
		ID: "a", Repository: "repo", Base: "base", BaseBranch: "trunk",
		FrozenMembers: []Member{member("one")}, ActiveMembers: []Member{member("one")}, State: Frozen,
	})
	admitted := greenBatchFor(runner)
	admitted.ID = "b"
	admitted.State = Publishing
	admitted.Branch = "land/b"
	admitted.PullRequest = PullRequest{Number: 7, Head: "candidate", State: PullRequestOpen}
	admitted.Admission = Admitted
	seed(t, store, admitted)

	if _, err := runner.Reconcile(context.Background()); err != nil {
		t.Logf("Reconcile() error = %v", err)
	}
	for _, call := range *touched {
		if call == "save a" || call == "load a" {
			t.Fatalf("the frozen batch was resumed before the admitted publishing batch: %q", *touched)
		}
		if call == "github b" || call == "save b" || call == "load b" || call == "withdraw b" {
			return
		}
	}
	t.Fatalf("the admitted publishing batch was never resumed: %q", *touched)
}

// orderedGitHub logs each call for batch b and reports the pull request merged at its candidate.
type orderedGitHub struct{ touched *[]string }

func (g *orderedGitHub) log() { *g.touched = append(*g.touched, "github b") }
func (g *orderedGitHub) Push(context.Context, string, string, string) error {
	g.log()
	return nil
}
func (g *orderedGitHub) FindPullRequest(context.Context, string, string) (PullRequest, bool, error) {
	g.log()
	return PullRequest{Number: 7, Head: "candidate", State: PullRequestOpen}, true, nil
}
func (g *orderedGitHub) Open(context.Context, string, string, string, string) (PullRequest, error) {
	g.log()
	return PullRequest{Number: 7, Head: "candidate", State: PullRequestOpen}, nil
}
func (g *orderedGitHub) Enqueue(context.Context, string, int) error {
	g.log()
	return nil
}
func (g *orderedGitHub) Wait(context.Context, string, int, string) (PullRequest, error) {
	g.log()
	return PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}, nil
}
func (g *orderedGitHub) Withdraw(context.Context, string, int) error {
	g.log()
	return nil
}
func (g *orderedGitHub) Read(context.Context, string, int) (PullRequest, error) {
	g.log()
	return PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}, nil
}

// Withdraw dequeues a queued pull request and closes it; a pull request already merged or closed is left alone.
func TestGitHubCLIWithdrawDequeuesAndCloses(t *testing.T) {
	executor := &commandExecutorFake{outputs: []string{
		`{"data":{"repository":{"pullRequest":{"id":"PR_1","state":"OPEN","mergeQueueEntry":{"id":"MQE_1"}}}}}`,
	}}
	client := GitHubCLI{Executor: executor}
	if err := client.Withdraw(context.Background(), "owner/repo", 7); err != nil {
		t.Fatalf("Withdraw() error = %v", err)
	}
	if len(executor.calls) != 3 || !sameStrings(executor.calls[1].args[len(executor.calls[1].args)-2:], []string{"-f", "id=PR_1"}) ||
		!sameStrings(executor.calls[2].args, []string{"pr", "close", "7", "--repo", "owner/repo"}) {
		t.Fatalf("withdraw calls = %+v, want a dequeue of PR_1 and a close of 7", executor.calls)
	}

	merged := &commandExecutorFake{outputs: []string{
		`{"data":{"repository":{"pullRequest":{"id":"PR_1","state":"MERGED","mergeQueueEntry":null}}}}`,
	}}
	if err := (GitHubCLI{Executor: merged}).Withdraw(context.Background(), "owner/repo", 7); err != nil || len(merged.calls) != 1 {
		t.Fatalf("withdraw of a merged pull request: %v, calls %+v; want one read and nothing else", err, merged.calls)
	}
}
