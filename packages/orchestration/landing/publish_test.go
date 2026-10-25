package landing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

// The ledger admits the batch after the push and the open and before the enqueue; GitHub's merge of the candidate
// lands it in one ledger call at the merged commit, and the score and each member's event run after the landing as
// its post-landing effects, each recorded done.
func TestPublishLandsMergedCandidateAfterQueueConfirmation(t *testing.T) {
	ledger := &ledgerFake{}
	admission := &batchLedgerFake{ledger: ledger}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
	score := &scoreFake{output: "score"}
	events := &eventsFake{}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: admission, GitHub: github, Score: score, Events: events}

	landed, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := landed.State, Landed; got != want {
		t.Fatalf("Publish() state = %q, want %q", got, want)
	}
	if got, want := github.pushedBranch, "land/batch"; got != want {
		t.Fatalf("pushed branch = %q, want %q", got, want)
	}
	if !sameStrings(admission.admitted, []string{"batch"}) || len(admission.landings) != 1 {
		t.Fatalf("admissions %v, landings %v; want one of each", admission.admitted, admission.landings)
	}
	if got, want := ledger.landed, []string{"one"}; !sameStrings(got, want) || ledger.commits["one"] != "merged" {
		t.Fatalf("ledger lands = %#v at %v, want %#v at the merged commit", got, ledger.commits, want)
	}
	if got, want := events.members, []string{"one"}; !sameStrings(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if got, want := score.commits, 1; got != want {
		t.Fatalf("score commits = %d, want %d", got, want)
	}
	if !sameStrings(admission.done, []string{"event/one", "score"}) || len(landed.PendingEffects) != 0 {
		t.Fatalf("effects done %v, pending %v; want both done", admission.done, landed.PendingEffects)
	}
}

// A queue removal or a person's close of an admitted batch declines it with no member blamed: nothing lands, every
// member goes to the successor, and the decline carries the timeline's reason.
func TestPublishDeclinesWhenThePullRequestLeavesUnmerged(t *testing.T) {
	for _, outcome := range []PullRequest{
		{Number: 7, Head: "candidate", State: PullRequestRemoved, Reason: "check failed"},
		{Number: 7, Head: "candidate", State: PullRequestClosed, Reason: "closed"},
	} {
		t.Run(string(outcome.State), func(t *testing.T) {
			ledger := &ledgerFake{}
			admission := &batchLedgerFake{ledger: ledger}
			runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: admission, GitHub: &githubFake{wait: outcome}}

			declinedBatch, err := runner.Publish(context.Background(), greenBatch())
			if err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			if declinedBatch.State != Declined || declinedBatch.Successor != "batch-next" {
				t.Fatalf("Publish() = %q with successor %q, want declined to batch-next", declinedBatch.State, declinedBatch.Successor)
			}
			if len(ledger.landed) != 0 || len(admission.declines) != 1 || admission.declines[0].reason != outcome.Reason ||
				len(admission.declines[0].returns) != 0 {
				t.Fatalf("lands %v, declines %+v; want one decline with no returns and the reason %q", ledger.landed, admission.declines, outcome.Reason)
			}
		})
	}
}

// A push, find or open that fails is Failed with nothing admitted, so the members keep their ready records.
func TestPublishFailsBeforeAdmissionWhenThePushFails(t *testing.T) {
	ledger := &ledgerFake{}
	admission := &batchLedgerFake{ledger: ledger}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: admission, GitHub: &githubFake{pushErr: errors.New("rejected")}}
	failed, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if failed.State != Failed || len(admission.admitted) != 0 {
		t.Fatalf("Publish() = %q with admissions %v, want failed with none", failed.State, admission.admitted)
	}
}

// A member that admission refuses is dropped through the not-admitted branch, and nothing is enqueued; a batch
// conflict drops nothing; any other refusal stops the runner.
func TestPublishRoutesAdmissionRefusals(t *testing.T) {
	member := &Refusal{Reason: "stale_read", Member: "one", Violations: []string{"stale_read"}}
	conflict := &Refusal{Reason: "batch_admitted"}
	other := &Refusal{Reason: "something_new"}

	ledger := &ledgerFake{}
	runner := Runner{Store: &MemoryStore{}, Ledger: ledger, Admission: &batchLedgerFake{ledger: ledger, refuse: member}, GitHub: &githubFake{wait: PullRequest{Number: 7, Head: "candidate"}}}
	batch := greenBatch()
	if _, err := runner.Store.Create(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
	dropped, err := runner.Publish(context.Background(), batch)
	if err != nil {
		t.Fatalf("Publish() with a member refusal: %v", err)
	}
	if dropped.State != Empty || !ledgerReturned(ledger, "one") {
		t.Fatalf("after a member refusal the batch is %q, returns %v; want the member returned and the batch empty", dropped.State, ledger.returned)
	}

	ledger = &ledgerFake{}
	runner = Runner{Store: &MemoryStore{}, Ledger: ledger, Admission: &batchLedgerFake{ledger: ledger, refuse: conflict}, GitHub: &githubFake{wait: PullRequest{Number: 7, Head: "candidate"}}}
	batch = greenBatch()
	if _, err := runner.Store.Create(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
	kept, err := runner.Publish(context.Background(), batch)
	if err != nil || len(ledger.returned) != 0 || len(kept.ActiveMembers) != 1 {
		t.Fatalf("after a batch conflict: %+v, %v, returns %v; want nothing dropped", kept, err, ledger.returned)
	}

	ledger = &ledgerFake{}
	runner = Runner{Store: &storeFake{}, Ledger: ledger, Admission: &batchLedgerFake{ledger: ledger, refuse: other}, GitHub: &githubFake{wait: PullRequest{Number: 7, Head: "candidate"}}}
	_, err = runner.Publish(context.Background(), greenBatch())
	var stop *StopError
	if !errors.As(err, &stop) || stop.Reason != "unclassified_refusal" || len(ledger.returned) != 0 {
		t.Fatalf("after an unclassified refusal: %v, returns %v; want a stop and nothing dropped", err, ledger.returned)
	}
}

// Enqueue failures keep the batch admitted until the limit; at the limit the runner withdraws the pull request, and a
// closed, unmerged pull request then declines the batch.
func TestPublishWithdrawsAfterTheRetryLimit(t *testing.T) {
	ledger := &ledgerFake{}
	admission := &batchLedgerFake{ledger: ledger}
	github := &githubFake{enqueueErr: errors.New("queue unavailable"), wait: PullRequest{Number: 7, Head: "candidate", State: PullRequestClosed, Reason: "closed"}}
	store := &MemoryStore{}
	runner := Runner{Store: admittedStore{MemoryStore: store}, Ledger: ledger, Admission: admission, GitHub: github,
		PublishingRetryLimit: 2, PublishingBackoff: time.Millisecond}
	batch := greenBatch()
	if _, err := store.Create(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
	first, err := runner.Publish(context.Background(), batch)
	if err == nil || first.State != Publishing || first.PublishAttempts != 1 || len(admission.withdraws) != 0 {
		t.Fatalf("after one failed enqueue: %q, %d attempts, %v; want admitted in publishing", first.State, first.PublishAttempts, err)
	}
	second, err := runner.Publish(context.Background(), first)
	if err != nil {
		t.Fatalf("the withdraw: %v", err)
	}
	if second.State != Declined || len(admission.withdraws) != 1 || len(admission.declines) != 1 ||
		admission.declines[0].reason != "publish_failed: github: enqueue" {
		t.Fatalf("after the limit: %q, withdraws %v, declines %+v; want withdrawn then declined", second.State, admission.withdraws, admission.declines)
	}
}

// greenBatch is a green batch whose receipt is at the current key of a runner
// with no configuration digest and no checks.
func greenBatch() Batch {
	return greenBatchFor(Runner{})
}

// greenBatchFor is a green batch whose receipt, and every member's ledger
// decision, is at the runner's current key.
func greenBatchFor(runner Runner) Batch {
	batch := Batch{
		ID:            "batch",
		Repository:    "repo",
		BaseBranch:    "trunk",
		Candidate:     candidateResult("candidate"),
		FrozenMembers: []Member{member("one")},
		ActiveMembers: []Member{member("one")},
		State:         Green,
	}
	key := runner.key(batch)
	batch.CandidateKey = key
	batch.ChecksKey = key
	for index := range batch.ActiveMembers {
		batch.ActiveMembers[index].LedgerKey = key
		batch.ActiveMembers[index].HashesKey = key
	}
	batch.Receipt = &Receipt{Key: key, Tree: "tree", Members: cloneMembers(batch.ActiveMembers)}
	return batch
}

func candidateResult(commit string) gitx.Result {
	return gitx.Result{Tree: "tree", Commit: commit}
}

type githubFake struct {
	pushedBranch string
	pull         PullRequest
	wait         PullRequest
	pushErr      error
	enqueueErr   error
}

func (f *githubFake) Push(context.Context, string, string, string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushedBranch = "land/batch"
	return nil
}

func (f *githubFake) FindPullRequest(context.Context, string, string) (PullRequest, bool, error) {
	return f.pull, f.pull.Number != 0, nil
}

func (f *githubFake) Open(context.Context, string, string, string, string) (PullRequest, error) {
	if f.wait.Number == 0 {
		return PullRequest{}, errors.New("missing pull request")
	}
	return PullRequest{Number: f.wait.Number, Head: f.wait.Head, State: PullRequestOpen}, nil
}

func (f *githubFake) Enqueue(context.Context, string, int) error { return f.enqueueErr }

func (f *githubFake) Wait(context.Context, string, int, string) (PullRequest, error) {
	return f.wait, nil
}

type scoreFake struct {
	output  string
	commits int
}

func (f *scoreFake) Score(context.Context, string, string) (string, error) { return f.output, nil }
func (f *scoreFake) Commit(context.Context, string, string, string, string) error {
	f.commits++
	return nil
}

type eventsFake struct {
	members []string
	commits []string
}

func (f *eventsFake) Publish(_ context.Context, event LandingEvent) error {
	f.members = append(f.members, event.Member)
	f.commits = append(f.commits, event.Commit)
	return nil
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
