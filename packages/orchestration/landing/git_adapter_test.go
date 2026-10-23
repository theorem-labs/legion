package landing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryGitReadsMemberDiff(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	gitRun(t, repo, "config", "user.name", "Test")
	writeSource(t, repo, "Source.lean", "def Target := 0\n")
	gitRun(t, repo, "add", "Source.lean")
	gitRun(t, repo, "commit", "-qm", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	writeSource(t, repo, "Source.lean", "def Replacement := 0\n")
	gitRun(t, repo, "add", "Source.lean")
	gitRun(t, repo, "commit", "-qm", "member")
	head := gitRun(t, repo, "rev-parse", "HEAD")

	diff, err := (RepositoryGit{}).Diff(context.Background(), repo, base, Member{Migration: "one", Head: head})
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if !strings.Contains(diff, "-def Target := 0") || !strings.Contains(diff, "+def Replacement := 0") {
		t.Fatalf("Diff() = %q, want declaration diff", diff)
	}
	change, err := (RepositoryGit{}).ChangeSet(context.Background(), repo, base, Member{Migration: "one", Head: head})
	if err != nil {
		t.Fatalf("ChangeSet() error = %v", err)
	}
	if got, want := change.Files, []string{"Source.lean"}; !sameStrings(got, want) {
		t.Fatalf("ChangeSet() files = %#v, want %#v", got, want)
	}
}

func writeSource(t *testing.T, repo, name, contents string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
