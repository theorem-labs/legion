package landing

import (
	"context"
	"testing"
)

func TestReviewPublishUsesMergedCommitForPostMergeEffects(t *testing.T) {
	ledger := &requestLedgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
	score := &commitScoreFake{}
	events := &eventsFake{}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, GitHub: github, Score: score, Events: events}
	input := greenBatch()
	input.ActiveMembers[0].Changed = []string{"path"}
	input.ActiveMembers[0].CurrentHashes = map[string]string{"Read": "current"}
	input.ActiveMembers[0].DerivedContracts = map[string]string{"Contract": "changed"}
	input.ActiveMembers[0].ApprovedPinned = []string{"Contract"}
	batch, err := runner.Publish(context.Background(), input)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := ledger.requests[0].Commit, "merged"; got != want {
		t.Fatalf("Land commit = %q, want %q", got, want)
	}
	if got, want := ledger.requests[0].ChangedPaths, []string{"path"}; !sameStrings(got, want) {
		t.Fatalf("changed paths = %#v, want %#v", got, want)
	}
	if got, want := ledger.requests[0].Current["Read"], "current"; got != want {
		t.Fatalf("current hash = %q, want %q", got, want)
	}
	if got, want := ledger.requests[0].ChangedContracts["Contract"], "changed"; got != want {
		t.Fatalf("changed contract = %q, want %q", got, want)
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

type requestLedgerFake struct{ requests []LandRequest }

func (f *requestLedgerFake) Check(_ context.Context, request CheckRequest) (CheckResult, error) {
	return passed(request), nil
}
func (f *requestLedgerFake) Land(_ context.Context, request LandRequest) error {
	f.requests = append(f.requests, request)
	return nil
}
func (f *requestLedgerFake) ReturnMember(context.Context, RoundRequest) error {
	return nil
}

func (f *requestLedgerFake) Contracts(context.Context, string) ([]Contract, error) { return nil, nil }

func (f *requestLedgerFake) MigrationState(context.Context, string, string) (string, string, error) {
	return "open", "", nil
}

type commitScoreFake struct{ commit string }

func (f *commitScoreFake) Score(context.Context, string, string) (string, error) { return "score", nil }
func (f *commitScoreFake) Commit(_ context.Context, _ string, commit string, _ string, _ string) error {
	f.commit = commit
	return nil
}
