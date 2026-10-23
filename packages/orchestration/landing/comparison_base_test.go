package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewChangeSetUsesMemberComparisonBase(t *testing.T) {
	git := newGitFake()
	git.changes["member"] = gitx.Change{Files: []string{"member"}}
	runner := testRunner(&ledgerFake{}, &builderFake{})
	runner.Git = git
	batch := Batch{
		Repository:    "repo",
		Base:          "frozen-base",
		Candidate:     gitx.Result{Tree: "tree"},
		ActiveMembers: []Member{{Migration: "member", ComparisonBase: "current-base"}},
	}

	if err := runner.collectChanges(context.Background(), &batch); err != nil {
		t.Fatalf("collectChanges() error = %v", err)
	}
	if got, want := git.changeBases, []string{"current-base"}; !sameStrings(got, want) {
		t.Fatalf("change bases = %#v, want %#v", got, want)
	}
}
