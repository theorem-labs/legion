package landing

import (
	"context"
	"errors"
	"testing"
)

// A landing whose response is lost lands nothing twice: the restart sends the same landing again, the ledger replays
// it, and the batch ends landed with each member landed once.
func TestReviewRestartDoesNotRepeatObservedLedgerLand(t *testing.T) {
	ledger := &ledgerFake{}
	store := &MemoryStore{}
	admission := &lostResponseLedger{batchLedgerFake: batchLedgerFake{ledger: ledger, store: store}}
	runner := Runner{
		Store:     admittedStore{MemoryStore: store},
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

	if _, err := runner.Publish(context.Background(), batch); err == nil {
		t.Fatal("Publish() error = nil, want the lost response")
	}
	// The ledger committed the landing, so the restart has nothing to resume and the stored batch is landed.
	if _, err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if stored, _, _ := store.Load(context.Background(), batch.ID); stored.State != Landed {
		t.Fatalf("state = %q, want %q", stored.State, Landed)
	}
	if !sameStrings(ledger.landed, []string{"one"}) {
		t.Fatalf("lands = %v, want one once", ledger.landed)
	}
}

// lostResponseLedger lands the first time and loses that response.
type lostResponseLedger struct {
	batchLedgerFake
	lost bool
}

func (l *lostResponseLedger) LandBatch(ctx context.Context, batch *Batch, mergedCommit string) error {
	record := cloneBatch(*batch)
	if err := l.batchLedgerFake.LandBatch(ctx, &record, mergedCommit); err != nil {
		return err
	}
	if !l.lost {
		l.lost = true
		return errors.New("connection reset after the commit")
	}
	*batch = record
	return nil
}
