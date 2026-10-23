package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewFrozenReturnRecoveryChecksSurvivingMembers(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{"keep": "refused"}}
	store := &MemoryStore{}
	runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-base": {Green: true}}})
	runner.Store = store
	batch := Batch{
		ID:            "batch",
		Repository:    "repo",
		Base:          "base",
		Candidate:     gitx.Result{Tree: "tree-bad+keep"},
		FrozenMembers: []Member{member("bad"), member("keep")},
		ActiveMembers: []Member{member("keep")},
		Dropped:       []DroppedMember{{Migration: "bad", Reason: "conflict"}},
		State:         Frozen,
		Operations: map[string]Operation{
			"return/bad": {Key: "return/bad", Kind: "return", Member: "bad", Reason: "conflict", State: OperationPending},
		},
	}
	if err := store.Save(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	if _, err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got, want := ledger.returned, []returned{{migration: "bad", reason: "conflict"}, {migration: "keep", reason: "refused"}}; !sameReturned(got, want) {
		t.Fatalf("returns = %#v, want %#v", got, want)
	}
}
