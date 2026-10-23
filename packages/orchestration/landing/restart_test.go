package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestRestartFromEveryPreGreenTransitionMatchesUninterruptedRun(t *testing.T) {
	input := Input{ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")}}

	for _, state := range []State{Frozen, Checked, Building, Blaming} {
		t.Run(string(state), func(t *testing.T) {
			ledger := &ledgerFake{}
			builder := &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}}
			store := &MemoryStore{}
			runner := testRunner(ledger, builder)
			runner.Store = store
			batch := Batch{
				ID:            input.ID,
				Repository:    input.Repository,
				Base:          input.Base,
				Candidate:     gitx.Result{Tree: "tree-one", Commit: "one"},
				FrozenMembers: []Member{member("one")},
				ActiveMembers: []Member{member("one")},
				State:         state,
			}
			if err := store.Save(context.Background(), batch); err != nil {
				t.Fatal(err)
			}

			resumed, err := runner.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if got, want := len(resumed), 1; got != want {
				t.Fatalf("reconciled batches = %d, want %d", got, want)
			}
			if got, want := resumed[0].State, Green; got != want {
				t.Fatalf("state = %q, want %q", got, want)
			}
			if got, want := resumed[0].ActiveMembers, []Member{member("one")}; !sameMembers(got, want) {
				t.Fatalf("active members = %#v, want %#v", got, want)
			}
			if resumed[0].Receipt == nil {
				t.Fatal("restart did not create a green receipt")
			}
		})
	}
}

func TestRestartContinuesPublishingWithoutLandingMemberTwice(t *testing.T) {
	ledger := &ledgerFake{}
	store := &MemoryStore{}
	runner := testRunner(ledger, &builderFake{})
	runner.Store = store
	runner.GitHub = &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
	batch := greenBatchFor(runner)
	batch.State = Publishing
	batch.Branch = "land/batch"
	if err := store.Save(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].State, Landed; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if got, want := ledger.landed, []string{"one"}; !sameStrings(got, want) {
		t.Fatalf("ledger lands = %#v, want %#v", got, want)
	}
}
