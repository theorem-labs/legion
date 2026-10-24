package declindex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Worktrees of one repository share its refs. Two refreshes with LAND_COMMIT in two worktrees of one repository
// fetch from the remote at the same time: each must check out its own commit, and neither may leave a ref that the
// other, or a later run, can move. The refs a sync makes are its own and are gone when it ends.
func TestTwoConcurrentSyncsOfOneRepositoryLeaveNoSharedRef(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "init", "-q", "-b", "main")
	writeSource(t, seed, "A.lean", "def a := 0\n")
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "base")
	remote := filepath.Join(root, "remote.git")
	gitIn(t, root, "clone", "-q", "--bare", seed, remote)
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "clone", "-q", remote, repo)
	gitIn(t, repo, "worktree", "add", "-q", "--detach", filepath.Join(root, "wt"))
	var commits []string
	for i, branch := range []string{"one", "two"} {
		gitIn(t, seed, "checkout", "-q", "-b", branch, "main")
		writeSource(t, seed, "A.lean", "def a := "+string(rune('1'+i))+"\n")
		gitIn(t, seed, "commit", "-q", "-am", branch)
		commits = append(commits, gitIn(t, seed, "rev-parse", "HEAD"))
		gitIn(t, seed, "push", "-q", remote, branch)
	}

	trees := []string{repo, filepath.Join(root, "wt")}
	errs := make([]error, len(trees))
	var done sync.WaitGroup
	for i, tree := range trees {
		cfg := Config{Tree: tree, Remote: remote, Build: []string{"true"}, BuildTimeout: time.Minute}
		done.Add(1)
		go func() {
			defer done.Done()
			errs[i] = Sync(cfg, commits[i], io.Discard)(ctx)
		}()
	}
	done.Wait()
	for i, tree := range trees {
		if errs[i] != nil {
			t.Fatalf("sync %d: %v", i, errs[i])
		}
		if got := gitIn(t, tree, "rev-parse", "HEAD"); got != commits[i] {
			t.Errorf("worktree %d is at %s, want its own commit %s", i, got, commits[i])
		}
	}
	if refs := gitIn(t, repo, "for-each-ref", "--format=%(refname)", "refs/declindex/"); refs != "" {
		t.Errorf("the syncs left refs that every worktree shares:\n%s", strings.TrimSpace(refs))
	}
}
