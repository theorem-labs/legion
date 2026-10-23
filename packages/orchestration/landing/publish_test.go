package landing

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestPublishLandsMergedCandidateAfterQueueConfirmation(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", State: PullRequestMerged}}
	score := &scoreFake{output: "score"}
	events := &eventsFake{}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, GitHub: github, Score: score, Events: events}
	batch := greenBatch()

	landed, err := runner.Publish(context.Background(), batch)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := landed.State, Landed; got != want {
		t.Fatalf("Publish() state = %q, want %q", got, want)
	}
	if got, want := github.pushedBranch, "land/batch"; got != want {
		t.Fatalf("pushed branch = %q, want %q", got, want)
	}
	if got, want := ledger.landed, []string{"one"}; !sameStrings(got, want) {
		t.Fatalf("ledger lands = %#v, want %#v", got, want)
	}
	if got, want := events.members, []string{"one"}; !sameStrings(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if got, want := score.commits, 1; got != want {
		t.Fatalf("score commits = %d, want %d", got, want)
	}
}

func TestPublishKeepsMembersWhenQueueRemovesPullRequest(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", State: PullRequestRemoved, Reason: "check failed"}}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, GitHub: github}

	failed, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := failed.State, Failed; got != want {
		t.Fatalf("Publish() state = %q, want %q", got, want)
	}
	if len(ledger.landed) != 0 {
		t.Fatalf("ledger lands = %#v, want none", ledger.landed)
	}
}

func TestPublishKeepsMembersWhenPersonClosesPullRequest(t *testing.T) {
	ledger := &ledgerFake{}
	github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", State: PullRequestClosed, Reason: "closed"}}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, GitHub: github}

	failed, err := runner.Publish(context.Background(), greenBatch())
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := failed.State, Failed; got != want {
		t.Fatalf("Publish() state = %q, want %q", got, want)
	}
	if len(ledger.landed) != 0 {
		t.Fatalf("ledger lands = %#v, want none", ledger.landed)
	}
}

func greenBatch() Batch {
	return Batch{
		ID:            "batch",
		Repository:    "repo",
		BaseBranch:    "trunk",
		Candidate:     candidateResult("candidate"),
		FrozenMembers: []Member{member("one")},
		ActiveMembers: []Member{member("one")},
		State:         Green,
		Receipt:       &Receipt{Tree: "tree", Members: []Member{member("one")}},
	}
}

func candidateResult(commit string) gitx.Result {
	return gitx.Result{Tree: "tree", Commit: commit}
}

type githubFake struct {
	pushedBranch string
	pull         PullRequest
	wait         PullRequest
}

func (f *githubFake) Push(context.Context, string, string, string) error {
	f.pushedBranch = "land/batch"
	return nil
}

func (f *githubFake) FindPullRequest(context.Context, string, string) (PullRequest, bool, error) {
	return f.pull, f.pull.Number != 0, nil
}

func (f *githubFake) Open(context.Context, string, string, string, string) (PullRequest, error) {
	if f.wait.Number == 0 {
		return PullRequest{}, errors.New("missing pull request")
	}
	return PullRequest{Number: f.wait.Number, Head: f.wait.Head, State: PullRequestOpen}, nil
}

func (f *githubFake) Enqueue(context.Context, string, int) error { return nil }

func (f *githubFake) Wait(context.Context, string, int, string) (PullRequest, error) {
	return f.wait, nil
}

type scoreFake struct {
	output  string
	commits int
}

func (f *scoreFake) Score(context.Context, string, string) (string, error) { return f.output, nil }
func (f *scoreFake) Commit(context.Context, string, string, string, string) error {
	f.commits++
	return nil
}

type eventsFake struct{ members []string }

func (f *eventsFake) Publish(_ context.Context, event LandingEvent) error {
	f.members = append(f.members, event.Member)
	return nil
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
