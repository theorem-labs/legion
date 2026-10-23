package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewRestartCompletesPendingReturnBeforeRebuilding(t *testing.T) {
	ledger := &ledgerFake{}
	store := &MemoryStore{}
	runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-keep": {Green: true}}})
	runner.Store = store
	batch := Batch{
		ID:            "batch",
		Repository:    "repo",
		Base:          "base",
		Candidate:     gitx.Result{Tree: "tree-bad+keep", Commit: "bad+keep"},
		FrozenMembers: []Member{member("bad"), member("keep")},
		ActiveMembers: []Member{member("keep")},
		Dropped:       []DroppedMember{{Migration: "bad", Reason: "build: evidence"}},
		State:         Blaming,
		Operations: map[string]Operation{
			"return/bad": {Key: "return/bad", Kind: "return", Member: "bad", Reason: "build: evidence", State: OperationPending},
		},
	}
	if err := store.Save(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := resumed[0].Receipt.Tree, "tree-keep"; got != want {
		t.Fatalf("receipt tree = %q, want rebuilt %q", got, want)
	}
	if got, want := ledger.returned, []returned{{migration: "bad", reason: "build: evidence"}}; !sameReturned(got, want) {
		t.Fatalf("returns = %#v, want %#v", got, want)
	}
}
