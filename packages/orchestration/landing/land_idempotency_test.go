package landing

import (
	"context"
	"errors"
	"testing"
)

func TestReviewRestartDoesNotRepeatObservedLedgerLand(t *testing.T) {
	ledger := &observedLedger{}
	store := &landCrashStore{}
	runner := Runner{
		Store:   store,
		Ledger:  ledger,
		Builder: &builderFake{},
		Hasher:  hasherFake{},
		Git:     newGitFake(),
		GitHub:  &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}},
	}

	if _, err := runner.Publish(context.Background(), greenBatch()); err == nil {
		t.Fatal("Publish() error = nil, want checkpoint failure after Land")
	}
	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].State, Landed; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if got, want := ledger.lands, 1; got != want {
		t.Fatalf("Land calls = %d, want %d", got, want)
	}
}

type observedLedger struct{ lands int }

func (l *observedLedger) Check(_ context.Context, request CheckRequest) (CheckResult, error) {
	return passed(request), nil
}
func (l *observedLedger) Land(context.Context, LandRequest) error { l.lands++; return nil }
func (l *observedLedger) ReturnMember(context.Context, RoundRequest) error {
	return nil
}
func (l *observedLedger) Contracts(context.Context, string) ([]Contract, error) { return nil, nil }

func (l *observedLedger) MigrationState(context.Context, string, string) (string, string, error) {
	if l.lands > 0 {
		return "landed", "merged", nil
	}
	return "open", "", nil
}

type landCrashStore struct {
	MemoryStore
	failed bool
}

func (s *landCrashStore) Save(ctx context.Context, batch Batch) error {
	if operation, found := batch.Operations["land/one"]; found && operation.State == OperationComplete && !s.failed {
		s.failed = true
		return errors.New("injected checkpoint failure")
	}
	return s.MemoryStore.Save(ctx, batch)
}
