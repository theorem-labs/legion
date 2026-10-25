package landing

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// manifestBuilder answers a candidate that holds the manifest member with PackagesChangedError, as the package
// step does for a manifest with no store entry, and builds every other candidate green.
type manifestBuilder struct {
	member string
	builds []Tree
}

func (b *manifestBuilder) Build(_ context.Context, tree blame.Tree, _ []string) (blame.BuildResult, error) {
	return blame.BuildResult{Green: true}, nil
}

func (b *manifestBuilder) BuildKept(_ context.Context, tree Tree) (KeptBuild, error) {
	if b.member == "" || strings.Contains(string(tree), b.member) {
		return KeptBuild{Release: noRelease}, PackagesChangedError{Hash: "new"}
	}
	b.builds = append(b.builds, tree)
	return KeptBuild{Result: blame.BuildResult{Green: true}, Release: noRelease}, nil
}

// Of two members, the one whose diff changes lake-manifest.json to a manifest with no store entry is returned with
// packages_changed before any candidate build, and the other is built and goes green.
func TestAMemberThatChangesTheManifestIsReturnedWithPackagesChanged(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &manifestBuilder{member: "manifest"}
	runner := testRunner(ledger, nil)
	runner.Builder = builder
	git := runner.Git.(*gitFake)
	git.changes["manifest"] = changes("lake-manifest.json")
	git.changes["other"] = changes("Alpha.lean")
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("manifest"), member("other")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := ledger.returned, []returned{{migration: "manifest", reason: "packages_changed"}}; !sameReturned(got, want) {
		t.Fatalf("returned = %+v, want %+v", got, want)
	}
	if batch.State != Green || len(builder.builds) != 1 || builder.builds[0] != "tree-other" {
		t.Fatalf("batch %s, builds %q; want green after one build of the other member", batch.State, builder.builds)
	}
}

// A base whose manifest has no store entry, with no member that touches it, stops the batch as unable to check and
// returns nobody.
func TestABaseWithoutAStoreEntryStopsTheBatch(t *testing.T) {
	ledger := &ledgerFake{}
	runner := testRunner(ledger, nil)
	runner.Builder = &manifestBuilder{}
	runner.Git.(*gitFake).changes["one"] = changes("Alpha.lean")
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("one")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if batch.State != Failed || batch.Reason != "packages-unable" || len(ledger.returned) != 0 {
		t.Fatalf("batch %s (%q), returned %+v; want failed as packages-unable with no return", batch.State, batch.Reason, ledger.returned)
	}
}
