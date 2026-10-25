package landing

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// fileBuilder builds a tree red, with log as its output, when the tree holds
// path, and green otherwise.
type fileBuilder struct {
	repo string
	path string
	log  string
}

func (b fileBuilder) Build(ctx context.Context, tree blame.Tree, _ []string) (blame.BuildResult, error) {
	output, err := exec.CommandContext(ctx, "git", "-C", b.repo, "ls-tree", "--name-only", string(tree), "--", b.path).Output()
	if err != nil {
		return blame.BuildResult{}, fmt.Errorf("list %s in %s: %w", b.path, tree, err)
	}
	if strings.TrimSpace(string(output)) == "" {
		return blame.BuildResult{Green: true}, nil
	}
	return blame.BuildResult{Output: b.log}, nil
}

func (b fileBuilder) BuildKept(ctx context.Context, tree Tree) (KeptBuild, error) {
	result, err := b.Build(ctx, tree, nil)
	return KeptBuild{Result: result, Release: noRelease}, err
}

// Six old members restate the base's own `def Shared := 0`, and one member
// adds a second Shared in another file. The runner reads the base and
// candidate declarations through the real Git adapter, drops the
// restatements, and returns the duplicate on its evidence. Without the tree
// comparison the six restatements bury the duplicate's evidence and the
// runner can only find it by bisecting.
func TestRunnerSuppressesRestatementsOfTheBase(t *testing.T) {
	repo, root := commandRepository(t, false)
	writeSource(t, repo, "Alpha.lean", "def Shared := 0\n")
	commandReviewGit(t, repo, "add", "Alpha.lean")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	base := commandReviewGit(t, repo, "rev-parse", "HEAD")
	var members []Member
	for index := range 6 {
		name := fmt.Sprintf("stale-%d", index)
		commandReviewGit(t, repo, "checkout", "-q", "-b", name, root)
		writeSource(t, repo, "Alpha.lean", "def Shared := 0\n")
		commandReviewGit(t, repo, "add", "Alpha.lean")
		commandReviewGit(t, repo, "commit", "-qm", name)
		members = append(members, Member{Migration: name, Generation: 1, Head: commandReviewGit(t, repo, "rev-parse", "HEAD")})
	}
	commandReviewGit(t, repo, "checkout", "-q", "-b", "duplicate", base)
	writeSource(t, repo, "Beta.lean", "def Shared := 1\n")
	commandReviewGit(t, repo, "add", "Beta.lean")
	commandReviewGit(t, repo, "commit", "-qm", "duplicate")
	members = append(members, Member{Migration: "duplicate", Generation: 1, Head: commandReviewGit(t, repo, "rev-parse", "HEAD")})

	ledger := &ledgerFake{}
	runner := testRunner(ledger, nil)
	runner.Git = RepositoryGit{}
	runner.Builder = fileBuilder{repo: repo, path: "Beta.lean", log: "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: environment already contains 'Shared'\n"}

	batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: repo, Base: base, Members: members})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := ledger.returned, []returned{{migration: "duplicate", reason: "build: evidence"}}; !sameReturned(got, want) {
		t.Fatalf("returned = %#v, want %#v", got, want)
	}
	if batch.State != Green {
		t.Fatalf("state = %q (%s), want green", batch.State, batch.Reason)
	}
}
