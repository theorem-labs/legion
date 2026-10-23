package landing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewRepositoryGitReturnsOnlyDeclarationLines(t *testing.T) {
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

	lines, err := (RepositoryGit{}).DeclarationLines(context.Background(), repo, Tree(commit), "Target")
	if err != nil {
		t.Fatalf("DeclarationLines() error = %v", err)
	}
	if got, want := lines, []string{"theorem Target : True := by trivial"}; !sameStrings(got, want) {
		t.Fatalf("DeclarationLines() = %#v, want %#v", got, want)
	}
}
