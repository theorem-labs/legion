package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewCurrentHashesRefreshForCurrentCandidateTree(t *testing.T) {
	ledger := &hashLedger{}
	runner := Runner{Store: &storeFake{}, Ledger: ledger, Builder: &builderFake{}, Hasher: treeHasher{}, Git: newGitFake(), ConfigDigest: "digest", Toolchain: func(context.Context, Tree) (string, error) { return "tool", nil }}
	batch := Batch{
		Repository:    "repo",
		Candidate:     gitx.Result{Tree: "tree-first"},
		ActiveMembers: []Member{{Migration: "reader", Generation: 1, ReadNames: []string{"Read"}}},
	}

	if err := runner.revalidateCandidate(context.Background(), &batch); err != nil {
		t.Fatalf("first revalidate error = %v", err)
	}
	batch.Candidate.Tree = "tree-final"
	if err := runner.revalidateCandidate(context.Background(), &batch); err != nil {
		t.Fatalf("second revalidate error = %v", err)
	}
	if got, want := batch.ActiveMembers[0].CurrentHashes["Read"], "tree-final"; got != want {
		t.Fatalf("current hash = %q, want %q", got, want)
	}
	if got, want := batch.ActiveMembers[0].HashesTree, "tree-final"; got != want {
		t.Fatalf("hash tree = %q, want %q", got, want)
	}
}

type treeHasher struct{}

func (treeHasher) Hashes(_ context.Context, tree Tree, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = string(tree)
	}
	return out, nil
}

type hashLedger struct{}

func (*hashLedger) Check(context.Context, string, string, int64, []string, map[string]string) (CheckResult, error) {
	return CheckResult{}, nil
}
func (*hashLedger) Land(context.Context, LandRequest) error                           { return nil }
func (*hashLedger) ReturnMember(context.Context, string, string, int64, string) error { return nil }
func (*hashLedger) MigrationState(context.Context, string, string) (string, string, error) {
	return "open", "", nil
}
