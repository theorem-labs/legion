package landing

import (
	"context"
	"testing"
)

func TestReviewWrongMergedHeadNeverRunsPostMergeEffects(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "different", MergedCommit: "merged", State: PullRequestMerged}}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, GitHub: github}

	batch, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := batch.State, Failed; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if len(ledger.landed) != 0 {
		t.Fatalf("ledger lands = %#v, want none", ledger.landed)
	}
	if batch.MergeValidated {
		t.Fatal("stored wrong head was marked validated")
	}
}
