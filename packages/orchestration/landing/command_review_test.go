package landing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCommandBuilderClassifiesKilledProcessAsInfrastructure(t *testing.T) {
	repo, commit := commandRepository(t, false)
	builder := CommandBuilder{RepositoryPath: repo, Command: []string{"sh", "-c", "kill -9 $$"}}

	result, err := builder.Build(context.Background(), Tree(commit), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if result.InfraClass == "" {
		t.Fatalf("Build() = %#v, want infrastructure class", result)
	}
}

func TestReviewCommandBuilderMaterializesSymlink(t *testing.T) {
	repo, commit := commandRepository(t, true)
	builder := CommandBuilder{RepositoryPath: repo, Command: []string{"sh", "-c", "test -L Link.lean"}}

	result, err := builder.Build(context.Background(), Tree(commit), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !result.Green {
		t.Fatalf("Build() = %#v, want symlink-preserving green result", result)
	}
}

func TestReviewCommandLedgerRejectsEmptyCheckDecision(t *testing.T) {
	ledger := CommandLedger{CheckCommand: []string{"sh", "-c", "exit 0"}}

	if _, err := ledger.Check(context.Background(), "repo", "member", 1, nil, nil); err == nil {
		t.Fatal("Check() error = nil, want explicit-decision error")
	}
}

func TestReviewCommandHasherRequiresEveryStructuredHash(t *testing.T) {
	hasher := CommandHasher{Command: []string{"sh", "-c", "printf 'Alpha.One\\thash-one\\n'"}}

	if _, err := hasher.Hashes(context.Background(), Tree("tree"), []string{"Alpha.One", "Alpha.Two"}); err == nil {
		t.Fatal("Hashes() error = nil, want missing-name error")
	}
}

func commandRepository(t *testing.T, symlink bool) (string, string) {
	t.Helper()
	repo := t.TempDir()
	commandReviewGit(t, repo, "init", "-q")
	commandReviewGit(t, repo, "config", "user.email", "test@example.invalid")
	commandReviewGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "Target.lean"), []byte("def target := 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if symlink {
		if err := os.Symlink("Target.lean", filepath.Join(repo, "Link.lean")); err != nil {
			t.Fatal(err)
		}
	}
	commandReviewGit(t, repo, "add", ".")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	return repo, commandReviewGit(t, repo, "rev-parse", "HEAD")
}

func commandReviewGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
