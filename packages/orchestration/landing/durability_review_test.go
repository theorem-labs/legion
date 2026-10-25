package landing

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// A transport error from the landing leaves the batch admitted in Publishing with GitHub's merge recorded, and a
// later round lands it once.
func TestReviewMergedBatchRemainsReconciliableAfterLedgerFailure(t *testing.T) {
	ledger := &ledgerFake{}
	admission := &batchLedgerFake{ledger: ledger, landErr: errors.New("connection reset")}
	store := &MemoryStore{}
	runner := Runner{
		Store:     store,
		Ledger:    ledger,
		Admission: admission,
		Builder:   &builderFake{},
		Hasher:    hasherFake{},
		Git:       newGitFake(),
		GitHub:    &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}},
	}
	batch := greenBatch()
	if _, err := store.Create(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}

	batch, err := runner.Publish(context.Background(), batch)
	if err == nil {
		t.Fatal("Publish() error = nil, want the transport failure")
	}
	if batch.State != Publishing || batch.Admission != Admitted {
		t.Fatalf("batch = %q/%q, want admitted in publishing", batch.State, batch.Admission)
	}
	if got, want := batch.PullRequest.MergedCommit, "merged"; got != want {
		t.Fatalf("merged observation = %q, want %q", got, want)
	}
	stored, _, _ := store.Load(context.Background(), batch.ID)
	stored.Admission = Admitted
	if err := store.Save(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	admission.landErr = nil

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].State, Landed; got != want {
		t.Fatalf("reconciled state = %q, want %q", got, want)
	}
	if !sameStrings(ledger.landed, []string{"one"}) || len(admission.landings) != 1 {
		t.Fatalf("lands = %v (%d landing calls), want one", ledger.landed, len(admission.landings))
	}
}

// A ledger refusal of the landing ends the batch LandedUnrecorded with the escalations owner, and the runner stops
// retrying it.
func TestReviewLandingRefusalIsLandedUnrecorded(t *testing.T) {
	ledger := &ledgerFake{}
	admission := &batchLedgerFake{ledger: ledger, landErr: &LedgerRefusal{Reason: "stale_batch", Message: "moved"}}
	runner := Runner{
		Store:      &MemoryStore{},
		Ledger:     ledger,
		Admission:  admission,
		GitHub:     &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}},
		EscalateTo: "landing-operator",
	}
	batch, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if batch.State != LandedUnrecorded || batch.EscalateTo != "landing-operator" || len(ledger.landed) != 0 {
		t.Fatalf("batch = %q escalated to %q, lands %v; want landed_unrecorded for the operator", batch.State, batch.EscalateTo, ledger.landed)
	}
}

func TestReviewRunDoesNotOverwriteExistingBatchIdentity(t *testing.T) {
	store := &MemoryStore{}
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}}
	runner := testRunner(ledger, builder)
	runner.Store = store
	input := Input{ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")}}

	first, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	second, err := runner.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if first.Candidate.Commit != second.Candidate.Commit || len(builder.trees) != 1 {
		t.Fatalf("existing batch was rebuilt or overwritten: first=%#v second=%#v builds=%#v", first, second, builder.trees)
	}
	input.Base = "other"
	if _, err := runner.Run(context.Background(), input); err == nil {
		t.Fatal("Run() error = nil, want conflicting identity refusal")
	}
}

type flakyLedger struct {
	failLand  bool
	landCalls int
}

func (f *flakyLedger) Check(_ context.Context, request CheckRequest) (CheckResult, error) {
	return passed(request), nil
}
func (f *flakyLedger) Land(context.Context, LandRequest) error {
	f.landCalls++
	if f.failLand {
		return errors.New("temporary ledger failure")
	}
	return nil
}
func (f *flakyLedger) ReturnMember(context.Context, RoundRequest) error { return nil }

func (f *flakyLedger) Contracts(context.Context, string) ([]Contract, error) { return nil, nil }

func (f *flakyLedger) Escalate(context.Context, string, string, string) error { return nil }

func (f *flakyLedger) MigrationState(context.Context, string, string) (string, string, error) {
	return "open", "", nil
}
