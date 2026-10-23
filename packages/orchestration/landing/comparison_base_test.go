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
	members := []Member{{Migration: "member", ComparisonBase: "current-base"}}

	if err := runner.readChanges(context.Background(), "repo", "frozen-base", members); err != nil {
		t.Fatalf("readChanges() error = %v", err)
	}
	if got, want := git.changeBases, []string{"current-base"}; !sameStrings(got, want) {
		t.Fatalf("change bases = %#v, want %#v", got, want)
	}
	if got, want := members[0].Changed, []string{"member"}; !sameStrings(got, want) {
		t.Fatalf("changed = %#v, want %#v", got, want)
	}
}
