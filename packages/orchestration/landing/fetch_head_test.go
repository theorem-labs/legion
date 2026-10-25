package landing

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in directory and returns its trimmed output.
func gitIn(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", directory}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// FetchHead brings a head into the program's repository by its SHA from the fetch URL, also when no ref of the
// source names it; a head already present needs no fetch; a SHA the source does not have cannot be fetched. The
// fetch URL defaults to the repository's path.
func TestRepositoryGitFetchesAHeadBySHA(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	gitIn(t, source, "init", "-q")
	gitIn(t, source, "-c", "user.email=t@example.invalid", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "base")
	gitIn(t, source, "checkout", "-q", "-b", "owner-branch")
	gitIn(t, source, "-c", "user.email=t@example.invalid", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "head")
	head := gitIn(t, source, "rev-parse", "HEAD")
	gitIn(t, source, "checkout", "-q", "-")
	gitIn(t, source, "branch", "-q", "-D", "owner-branch")

	program := filepath.Join(t.TempDir(), "program.git")
	gitIn(t, filepath.Dir(program), "clone", "-q", "--bare", source, program)
	if _, err := exec.Command("git", "-C", program, "cat-file", "-e", head+"^{commit}").CombinedOutput(); err == nil {
		// A clone may carry the unreferenced object; remove it so the fetch is what brings it.
		gitIn(t, program, "reflog", "expire", "--expire=now", "--all")
		gitIn(t, program, "gc", "-q", "--prune=now")
	}

	git := RepositoryGit{Path: program, FetchURL: source}
	if err := git.FetchHead(ctx, "repo", head); err != nil {
		t.Fatalf("FetchHead() error = %v", err)
	}
	gitIn(t, program, "cat-file", "-e", head+"^{commit}")
	if err := git.FetchHead(ctx, "repo", head); err != nil {
		t.Fatalf("FetchHead() of a present head = %v", err)
	}
	if err := git.FetchHead(ctx, "repo", strings.Repeat("1", 40)); err == nil {
		t.Fatal("FetchHead() of a SHA the source does not have succeeded")
	}
	if got := (RepositoryGit{Path: program}).fetchURL("repo"); got != program {
		t.Fatalf("the default fetch URL is %q, want the repository path %q", got, program)
	}
}
