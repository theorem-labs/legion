package landing

import (
	"context"
	"testing"
)

// The landing lands at GitHub's merged commit, not the candidate's, and so do the score and the events that follow
// it.
func TestReviewPublishUsesMergedCommitForPostMergeEffects(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
	score := &commitScoreFake{}
	events := &eventsFake{}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Admission: &batchLedgerFake{ledger: ledger}, GitHub: github, Score: score, Events: events}
	batch, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := ledger.commits["one"], "merged"; got != want {
		t.Fatalf("land commit = %q, want %q", got, want)
	}
	if got, want := score.commit, "merged"; got != want {
		t.Fatalf("score commit = %q, want %q", got, want)
	}
	if got, want := events.commits, []string{"merged"}; !sameStrings(got, want) {
		t.Fatalf("event commits = %#v, want %#v", got, want)
	}
	if got, want := batch.PullRequest.MergedCommit, "merged"; got != want {
		t.Fatalf("stored merged commit = %q, want %q", got, want)
	}
}

type commitScoreFake struct{ commit string }

func (f *commitScoreFake) Score(context.Context, string, string) (string, error) { return "score", nil }
func (f *commitScoreFake) Commit(_ context.Context, _ string, commit string, _ string, _ string) error {
	f.commit = commit
	return nil
}
