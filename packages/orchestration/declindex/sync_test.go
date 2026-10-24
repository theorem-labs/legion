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

// syncRemote is a seed repository, a bare remote cloned from it with one commit on main, and a clone of the remote:
// the tree that a landed sync checks out in.
func syncRemote(t *testing.T) (seed, remote, tree string) {
	t.Helper()
	root := t.TempDir()
	seed = filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "init", "-q", "-b", "main")
	writeSource(t, seed, "A.lean", "def a := 0\n")
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "base")
	remote = filepath.Join(root, "remote.git")
	gitIn(t, root, "clone", "-q", "--bare", seed, remote)
	tree = filepath.Join(root, "tree")
	gitIn(t, root, "clone", "-q", remote, tree)
	return seed, remote, tree
}

// The landing program's clone holds a landed commit under a ref that is not a branch. A refresh with LAND_COMMIT
// fetches that commit by its id, checks it out, and leaves no ref behind.
func TestSyncFetchesALandedCommitThatNoBranchHolds(t *testing.T) {
	seed, remote, tree := syncRemote(t)
	gitIn(t, seed, "checkout", "-q", "-b", "landing")
	writeSource(t, seed, "A.lean", "def a := 1\n")
	gitIn(t, seed, "commit", "-q", "-am", "landed")
	commit := gitIn(t, seed, "rev-parse", "HEAD")
	gitIn(t, seed, "push", "-q", remote, "landing:refs/landed/1")
	if heads := gitIn(t, remote, "branch", "--contains", commit); heads != "" {
		t.Fatalf("a branch of the remote holds the landed commit: %s", heads)
	}
	cfg := Config{Tree: tree, Remote: remote, Build: []string{"true"}, BuildTimeout: time.Minute}
	if err := Sync(cfg, commit, io.Discard)(context.Background()); err != nil {
		t.Fatalf("sync of a commit that no branch holds: %v", err)
	}
	if got := gitIn(t, tree, "rev-parse", "HEAD"); got != commit {
		t.Errorf("the tree is at %s, want the landed commit %s", got, commit)
	}
	if refs := gitIn(t, tree, "for-each-ref", "--format=%(refname)", "refs/declindex/"); refs != "" {
		t.Errorf("the sync left refs:\n%s", strings.TrimSpace(refs))
	}
}

// A remote that refuses a fetch by id (protocol version 0, which only allows the tips that it advertises) still
// gives the landed commit through its branches.
func TestSyncFetchesTheBranchesWhenTheRemoteRefusesAFetchByID(t *testing.T) {
	seed, remote, tree := syncRemote(t)
	writeSource(t, seed, "A.lean", "def a := 1\n")
	gitIn(t, seed, "commit", "-q", "-am", "landed")
	commit := gitIn(t, seed, "rev-parse", "HEAD")
	writeSource(t, seed, "A.lean", "def a := 2\n")
	gitIn(t, seed, "commit", "-q", "-am", "after")
	gitIn(t, seed, "push", "-q", remote, "main")
	gitIn(t, remote, "config", "uploadpack.allowReachableSHA1InWant", "false")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.version")
	t.Setenv("GIT_CONFIG_VALUE_0", "0")
	var log strings.Builder
	cfg := Config{Tree: tree, Remote: remote, Build: []string{"true"}, BuildTimeout: time.Minute}
	if err := Sync(cfg, commit, &log)(context.Background()); err != nil {
		t.Fatalf("sync: %v\n%s", err, log.String())
	}
	if got := gitIn(t, tree, "rev-parse", "HEAD"); got != commit {
		t.Errorf("the tree is at %s, want the landed commit %s", got, commit)
	}
	if !strings.Contains(log.String(), "unadvertised object") {
		t.Errorf("the fetch by id was not refused, so the test did not exercise the branches:\n%s", log.String())
	}
	if refs := gitIn(t, tree, "for-each-ref", "--format=%(refname)", "refs/declindex/"); refs != "" {
		t.Errorf("the sync left refs:\n%s", strings.TrimSpace(refs))
	}
}
