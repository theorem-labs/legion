package landing

import (
	"context"
	"testing"
)

func TestReviewWrongMergedHeadNeverRunsPostMergeEffects(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "different", MergedCommit: "merged", State: PullRequestMerged}}
	admission := &batchLedgerFake{ledger: ledger}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: admission, GitHub: github}

	batch, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	// The content is on the base branch, so the batch cannot be declined: it is landed_unrecorded, and nothing lands.
	if got, want := batch.State, LandedUnrecorded; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if len(ledger.landed) != 0 || len(admission.landings) != 0 {
		t.Fatalf("ledger lands = %#v, want none", ledger.landed)
	}
}
