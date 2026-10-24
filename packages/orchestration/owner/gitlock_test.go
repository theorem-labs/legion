package owner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitPath is the path git uses for name in the worktree: index.lock and HEAD.lock in its own git directory, a branch's
// ref lock in the bare repository.
func gitPath(t *testing.T, w Worktree, name string) string {
	t.Helper()
	path := gitOutput(t, w.Path, nil, "rev-parse", "--git-path", name)
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.Path, path)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A commit killed while git held its locks leaves the lock files behind with no process that holds them. The next
// commit in the worktree clears them and commits, so a killed step cannot wedge its owner.
func TestCommitFileClearsTheLocksAKilledCommitLeft(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/stale")
	if _, err := worktree.CommitFile(context.Background(), "records/first.txt", []byte("first\n"), "owner: first", nil); err != nil {
		t.Fatal(err)
	}
	locks := []string{gitPath(t, worktree, "index.lock"), gitPath(t, worktree, "HEAD.lock"), gitPath(t, worktree, "refs/heads/owner/stale.lock")}
	for _, lock := range locks {
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := worktree.CommitFile(context.Background(), "records/second.txt", []byte("second\n"), "owner: second", map[string][]string{"Owner-Item": {"need:a->b@r1"}})
	if err != nil {
		t.Fatalf("commit after a killed commit: %v", err)
	}
	if got := gitOutput(t, worktree.Path, nil, "show", commit+":records/second.txt"); got != "second" {
		t.Fatalf("the commit holds %q", got)
	}
	for _, lock := range locks {
		if exists(lock) {
			t.Errorf("the stale lock %s is left", lock)
		}
	}
}

// The helper process of TestCommitFileWaitsForAKilledCommitsGitThenRunsAgain: it runs one CommitFile and exits.
func TestCommitHelperProcess(t *testing.T) {
	spec := os.Getenv("OWNER_COMMIT_HELPER")
	if spec == "" {
		return
	}
	fields := strings.Split(spec, "\n")
	worktree := Worktree{Repo: Repo{Path: fields[0]}, Path: fields[1], Branch: fields[2]}
	if _, err := worktree.CommitFile(context.Background(), "records/killed.txt", []byte("killed\n"), "owner: killed", nil); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// A step killed in the middle of its commit: the owner process dies while its git still runs and holds the index
// lock. The re-run of the step waits for that git instead of taking its lock away; once the git is gone too, the re-run
// clears the lock it left and commits the record.
func TestCommitFileWaitsForAKilledCommitsGitThenRunsAgain(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/killed")
	marker := filepath.Join(t.TempDir(), "in-commit")
	hook := filepath.Join(repo.Path, "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	// git runs the pre-commit hook while it holds the index lock; this one stops the commit there.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > '"+marker+"'\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestCommitHelperProcess$")
	helper.Env = append(os.Environ(), "OWNER_COMMIT_HELPER="+repo.Path+"\n"+worktree.Path+"\n"+worktree.Branch)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	group := helper.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	waitFor(t, 30*time.Second, "the helper's commit to reach its hook", func() bool { return exists(marker) })
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	// The owner process dies; its git and the hook run on, as a killed command's children do.
	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	indexLock := gitPath(t, worktree, "index.lock")
	if !exists(indexLock) {
		t.Fatalf("the killed commit's git holds no %s", indexLock)
	}
	done := make(chan error, 1)
	go func() {
		_, err := worktree.CommitFile(context.Background(), "records/killed.txt", []byte("killed\n"), "owner: killed", nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the re-run returned while the killed commit's git still ran: %v", err)
	case <-time.After(time.Second):
	}
	if !exists(indexLock) {
		t.Fatal("the re-run removed the index lock of a git that still runs")
	}

	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the re-run after the killed commit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the re-run did not finish after the killed commit's git was gone")
	}
	if got := gitOutput(t, worktree.Path, nil, "show", "HEAD:records/killed.txt"); got != "killed" {
		t.Fatalf("HEAD holds %q", got)
	}
	if exists(indexLock) {
		t.Fatalf("the stale %s is left", indexLock)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
