package landing

import (
	"context"
	"testing"
)

// The landing lands at GitHub's merged commit, not the candidate's, and so do the effects that follow it.
func TestReviewPublishUsesMergedCommitForPostMergeEffects(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
	events := &eventsFake{}
	effects := newEffectLog()
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: &batchLedgerFake{ledger: ledger, effects: effects},
		GitHub: github, Events: events, Effects: effects}
	batch, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := ledger.commits["one"], "merged"; got != want {
		t.Fatalf("land commit = %q, want %q", got, want)
	}
	if got, want := events.commits, []string{"merged"}; !sameStrings(got, want) {
		t.Fatalf("event commits = %#v, want %#v", got, want)
	}
	if got, want := batch.PullRequest.MergedCommit, "merged"; got != want {
		t.Fatalf("stored merged commit = %q, want %q", got, want)
	}
}
