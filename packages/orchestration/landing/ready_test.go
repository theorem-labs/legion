package landing

import (
	"context"
	"errors"
	"testing"
)

// readyBatchLedger is a batch ledger whose ready list is fixed.
type readyBatchLedger struct {
	batchLedgerFake
	ready []Member
}

func (l *readyBatchLedger) ReadyMembers(context.Context, string) ([]Member, error) {
	return cloneMembers(l.ready), nil
}

// The next batch of a serving program is the ledger's ready migrations, in ready order, capped at the configured
// number, on the current tip of the base branch. No batch is configured: the program has none.
func TestReadyInputTakesTheReadyMigrationsAtTheBaseBranchTip(t *testing.T) {
	ctx := context.Background()
	ready := []Member{
		{Migration: "first", Generation: 1, Head: "head-first", ReadyEvent: 11},
		{Migration: "second", Generation: 2, Head: "head-second", ReadyEvent: 12},
		{Migration: "third", Generation: 1, Head: "head-third", ReadyEvent: 13},
	}
	runner := Runner{Admission: &readyBatchLedger{ready: ready}, Git: newGitFake()}

	input, err := runner.ReadyInput(ctx, "repo", "trunk", 2)
	if err != nil {
		t.Fatalf("ReadyInput() error = %v", err)
	}
	if input.Base != "trunk" || input.Repository != "repo" || input.BaseBranch != "trunk" {
		t.Fatalf("the input is %+v", input)
	}
	if len(input.Members) != 2 || input.Members[0].Migration != "first" || input.Members[1].Migration != "second" {
		t.Fatalf("the members are %+v, want the first two ready ones", input.Members)
	}
	if input.Members[1].ReadyEvent != 12 {
		t.Fatalf("the member kept no ready record: %+v", input.Members[1])
	}
	// The same ready list at the same base names the same batch; another list names another one.
	again, err := runner.ReadyInput(ctx, "repo", "trunk", 2)
	if err != nil || again.ID != input.ID {
		t.Fatalf("a second round named %q, want %q (%v)", again.ID, input.ID, err)
	}
	runner.Admission = &readyBatchLedger{ready: ready[1:]}
	other, err := runner.ReadyInput(ctx, "repo", "trunk", 2)
	if err != nil || other.ID == input.ID {
		t.Fatalf("another ready list named %q, the same batch (%v)", other.ID, err)
	}
}

// With no ready migration there is no batch to form: the round does nothing and waits.
func TestReadyInputWithNoReadyMigrationFormsNothing(t *testing.T) {
	runner := Runner{Admission: &readyBatchLedger{}, Git: newGitFake()}
	if _, err := runner.ReadyInput(context.Background(), "repo", "trunk", 0); !errors.Is(err, ErrNoReadyMembers) {
		t.Fatalf("ReadyInput() error = %v, want no ready member", err)
	}
}
