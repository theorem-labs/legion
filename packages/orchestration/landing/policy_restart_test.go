package landing

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewRestartRerunsChecksWhenPolicyChanges(t *testing.T) {
	store := &MemoryStore{}
	runner := testRunner(&ledgerFake{}, &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}})
	runner.Store = store
	runner.ConfigDigest = "new-policy"
	runner.Checks = []Checker{checkerFake{name: "new", err: errors.New("policy unavailable")}}
	batch := Batch{
		ID:                "batch",
		Repository:        "repo",
		Base:              "base",
		Candidate:         gitx.Result{Tree: "tree-one", Commit: "one"},
		FrozenMembers:     []Member{member("one")},
		ActiveMembers:     []Member{member("one")},
		State:             Checked,
		CheckedTree:       "tree-one",
		CheckPolicyDigest: "old-policy",
		CheckPolicy:       []string{"old"},
	}
	if err := store.Save(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].State, Failed; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
	if got, want := resumed[0].Reason, "check-unable"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}
