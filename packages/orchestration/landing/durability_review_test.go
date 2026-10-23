package landing

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

func TestReviewMergedBatchRemainsReconciliableAfterLedgerFailure(t *testing.T) {
	ledger := &flakyLedger{failLand: true}
	store := &MemoryStore{}
	runner := Runner{
		Store:   store,
		Ledger:  ledger,
		Builder: &builderFake{},
		Hasher:  hasherFake{},
		Git:     newGitFake(),
		GitHub:  &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}},
	}

	batch, err := runner.Publish(context.Background(), greenBatch())
	if err == nil {
		t.Fatal("Publish() error = nil, want transient ledger failure")
	}
	if got, want := batch.State, Publishing; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if got, want := batch.PullRequest.MergedCommit, "merged"; got != want {
		t.Fatalf("merged observation = %q, want %q", got, want)
	}
	ledger.failLand = false

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].State, Landed; got != want {
		t.Fatalf("reconciled state = %q, want %q", got, want)
	}
	if got, want := ledger.landCalls, 2; got != want {
		t.Fatalf("land calls = %d, want retry after pending operation", got)
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

func (f *flakyLedger) Check(context.Context, string, string, int64, []string, map[string]string) (CheckResult, error) {
	return CheckResult{}, nil
}
func (f *flakyLedger) Land(context.Context, LandRequest) error {
	f.landCalls++
	if f.failLand {
		return errors.New("temporary ledger failure")
	}
	return nil
}
func (f *flakyLedger) ReturnMember(context.Context, string, string, int64, string) error { return nil }
