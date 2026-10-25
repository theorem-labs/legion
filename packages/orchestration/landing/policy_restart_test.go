package landing

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// A changed checking policy changes the batch key, so a restarted batch that is
// not merged reruns the new checks before it builds or publishes, whatever
// phase it stopped in.
func TestReviewRestartRerunsChecksWhenPolicyChanges(t *testing.T) {
	for _, state := range []State{Frozen, Checked, Building, Blaming, Green, Publishing} {
		t.Run(string(state), func(t *testing.T) {
			ledger := &ledgerFake{}
			store := &MemoryStore{}
			runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}})
			runner.Store = store
			runner.ConfigDigest = "old-policy"
			runner.Checks = []Checker{checkerFake{name: "old-check"}}
			batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: "repo", Base: "base", BaseBranch: "trunk", Members: []Member{member("one")}})
			if err != nil || batch.State != Green {
				t.Fatalf("Run() = %q, %v; want green", batch.State, err)
			}
			batch.State = state
			if err := store.Save(context.Background(), &batch); err != nil {
				t.Fatal(err)
			}

			runner.ConfigDigest = "new-policy"
			runner.Checks = []Checker{checkerFake{name: "new-blocking-check", err: errors.New("unable")}}
			github := &githubFake{wait: PullRequest{Number: 7, Head: batch.Candidate.Commit, MergedCommit: "merged", State: PullRequestMerged}}
			runner.GitHub = github
			resumed, err := runner.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if got := resumed[0]; got.State != Failed || got.Reason != "check-unable" {
				t.Fatalf("resumed = %q/%q, want failed/check-unable", got.State, got.Reason)
			}
			if github.pushedBranch != "" || len(ledger.landed) != 0 {
				t.Fatalf("pushed %q and landed %v under a policy whose check was unable", github.pushedBranch, ledger.landed)
			}
		})
	}
}

// An admitted batch completes under the receipt and policy that its admission
// checked: it is in the merge queue, so a later policy cannot recheck it.
func TestMergedBatchCompletesUnderValidatedPolicy(t *testing.T) {
	ledger := &ledgerFake{}
	store := &MemoryStore{}
	runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}})
	runner.Store = store
	runner.ConfigDigest = "old-policy"
	batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: "repo", Base: "base", BaseBranch: "trunk", Members: []Member{member("one")}})
	if err != nil || batch.State != Green {
		t.Fatalf("Run() = %q, %v; want green", batch.State, err)
	}
	batch.State = Publishing
	batch.Branch = "land/batch"
	batch.PullRequest = PullRequest{Number: 7, Head: batch.Candidate.Commit, State: PullRequestOpen}
	if err := store.Save(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}

	runner.Store = admittedStore{MemoryStore: store}
	runner.ConfigDigest = "new-policy"
	runner.Checks = []Checker{checkerFake{name: "new-blocking-check", err: errors.New("unable")}}
	runner.GitHub = &githubFake{pull: batch.PullRequest, wait: PullRequest{Number: 7, Head: batch.Candidate.Commit, MergedCommit: "merged", State: PullRequestMerged}}
	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := resumed[0]
	if got.State != Landed || !sameStrings(ledger.landed, []string{"one"}) {
		t.Fatalf("resumed = %q, lands %v; want landed once", got.State, ledger.landed)
	}
	if got.Receipt.ConfigurationHash != "old-policy" || got.Receipt.Key != batch.Receipt.Key || got.CheckPolicyDigest != batch.CheckPolicyDigest {
		t.Fatalf("receipt = %+v, policy %q; want the receipt and policy %q that the merge validated", got.Receipt, got.CheckPolicyDigest, batch.CheckPolicyDigest)
	}
}

// A batch whose pull request is recorded is in the merge queue, and the queue
// may merge it at any moment. A changed policy therefore does not fail it, which
// would leave the queued pull request to merge with no ledger record: recovery
// waits for the queue's outcome, and only a merge of the candidate lands it.
func TestQueuedBatchWaitsForTheQueueUnderAChangedPolicy(t *testing.T) {
	for _, outcome := range []PullRequestState{PullRequestMerged, PullRequestRemoved} {
		t.Run(string(outcome), func(t *testing.T) {
			ledger := &ledgerFake{}
			store := &MemoryStore{}
			runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}})
			runner.Store = store
			runner.ConfigDigest = "old-policy"
			runner.Checks = []Checker{checkerFake{name: "old-check"}}
			batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: "repo", Base: "base", BaseBranch: "trunk", Members: []Member{member("one")}})
			if err != nil || batch.State != Green {
				t.Fatalf("Run() = %q, %v; want green", batch.State, err)
			}
			batch.State = Publishing
			batch.Branch = "land/batch"
			batch.PullRequest = PullRequest{Number: 7, Head: batch.Candidate.Commit, State: PullRequestOpen}
			if err := store.Save(context.Background(), &batch); err != nil {
				t.Fatal(err)
			}

			admission := runner.Admission.(*batchLedgerFake)
			runner.Store = admittedStore{MemoryStore: store}
			runner.ConfigDigest = "new-policy"
			runner.Checks = []Checker{checkerFake{name: "new-blocking-check", err: errors.New("unable")}}
			wait := PullRequest{Number: 7, Head: batch.Candidate.Commit, State: outcome, Reason: "checks failed"}
			if outcome == PullRequestMerged {
				wait.MergedCommit = "merged"
			}
			runner.GitHub = &githubFake{pull: batch.PullRequest, wait: wait}
			resumed, err := runner.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			got := resumed[0]
			switch outcome {
			case PullRequestMerged:
				if got.State != Landed || !sameStrings(ledger.landed, []string{"one"}) || got.Receipt.Key != batch.Receipt.Key {
					t.Fatalf("resumed = %q, lands %v, receipt key %q; want landed once under the queued receipt %q", got.State, ledger.landed, got.Receipt.Key, batch.Receipt.Key)
				}
			default:
				// An admitted batch that the queue removes is declined with the queue's reason; no member is blamed.
				if got.State != Declined || len(ledger.landed) != 0 || len(admission.declines) != 1 ||
					admission.declines[0].reason != "checks failed" || len(admission.declines[0].returns) != 0 {
					t.Fatalf("resumed = %q/%q, lands %v, declines %+v; want one decline with the queue's reason and no land", got.State, got.Reason, ledger.landed, admission.declines)
				}
			}
		})
	}
}

// admittedStore reads every active batch as the ledger shows it once admitted: its pull request is in the merge queue.
type admittedStore struct{ *MemoryStore }

func (s admittedStore) Load(ctx context.Context, id string) (Batch, bool, error) {
	batch, found, err := s.MemoryStore.Load(ctx, id)
	if found && !batch.State.terminal() {
		batch.Admission = Admitted
	}
	return batch, found, err
}

func (s admittedStore) ListActive(ctx context.Context) ([]Batch, error) {
	batches, err := s.MemoryStore.ListActive(ctx)
	for index := range batches {
		if !batches[index].State.terminal() {
			batches[index].Admission = Admitted
		}
	}
	return batches, err
}
