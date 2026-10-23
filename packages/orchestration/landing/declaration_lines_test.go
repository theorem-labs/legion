package landing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewRepositoryGitReturnsOnlyDeclarations(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	gitRun(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "Alpha.lean"), []byte("-- Target in a comment\ntheorem Target : True := by trivial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "Alpha.lean")
	gitRun(t, repo, "commit", "-qm", "base")
	commit := gitRun(t, repo, "rev-parse", "HEAD")

	declarations, err := (RepositoryGit{}).Declarations(context.Background(), repo, Tree(commit), "Target")
	if err != nil {
		t.Fatalf("Declarations() error = %v", err)
	}
	if len(declarations) != 1 || declarations[0].File != "Alpha.lean" || declarations[0].Name != "Target" || declarations[0].Text != "theorem Target : True := by trivial" {
		t.Fatalf("Declarations() = %#v, want Target in Alpha.lean", declarations)
	}
}

func TestReviewRepositoryGitResolvesQualifiedNameInNamespace(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	gitRun(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "Alpha.lean"), []byte("namespace Alpha\ndef Shared := 0\nend Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "Alpha.lean")
	gitRun(t, repo, "commit", "-qm", "base")
	commit := gitRun(t, repo, "rev-parse", "HEAD")

	declarations, err := (RepositoryGit{}).Declarations(context.Background(), repo, Tree(commit), "Alpha.Shared")
	if err != nil {
		t.Fatalf("Declarations() error = %v", err)
	}
	if len(declarations) != 1 || declarations[0].File != "Alpha.lean" || declarations[0].Name != "Alpha.Shared" || declarations[0].Text != "def Shared := 0" {
		t.Fatalf("Declarations() = %#v, want Alpha.Shared in Alpha.lean", declarations)
	}
}
