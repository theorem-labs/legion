package owner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Repo is a bare Git repository.
type Repo struct {
	Path string
}

// Worktree is one branch checkout of a Repo.
type Worktree struct {
	Repo   Repo
	Path   string
	Branch string
}

var autoGCConfigMu sync.Mutex

// Worktree creates a worktree at dir on branch from base, or reuses an existing directory.
func (r Repo) Worktree(ctx context.Context, dir, branch, base string) (Worktree, error) {
	if err := r.disableAutoGC(ctx); err != nil {
		return Worktree{}, err
	}

	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return Worktree{}, fmt.Errorf("worktree path %s is not a directory", dir)
		}
		return Worktree{Repo: r, Path: dir, Branch: branch}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Worktree{}, fmt.Errorf("stat worktree %s: %w", dir, err)
	}

	exists, err := r.branchExists(ctx, branch)
	if err != nil {
		return Worktree{}, err
	}
	args := []string{"worktree", "add"}
	if !exists {
		args = append(args, "-b", branch)
	}
	args = append(args, dir)
	if exists {
		args = append(args, branch)
	} else if base != "" {
		args = append(args, base)
	}
	if _, err := r.git(ctx, args...); err != nil {
		return Worktree{}, err
	}
	return Worktree{Repo: r, Path: dir, Branch: branch}, nil
}

// BlobAtHead reads path from the worktree's HEAD. It reports found false when the path is absent.
func (w Worktree) BlobAtHead(path string) ([]byte, bool, error) {
	data, err := w.git(context.Background(), "show", "HEAD:"+path)
	if err == nil {
		return []byte(data), true, nil
	}
	if _, headErr := w.git(context.Background(), "rev-parse", "--verify", "HEAD^{commit}"); headErr != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// CommitFile writes path and commits it unless HEAD already has the same bytes. Each value of a trailer key is its
// own trailer line, so a record that answers several items carries one Owner-Item trailer for each.
func (w Worktree) CommitFile(ctx context.Context, path string, data []byte, subject string, trailers map[string][]string) (string, error) {
	existing, found, err := w.BlobAtHead(path)
	if err != nil {
		return "", err
	}
	if found && bytes.Equal(existing, data) {
		return w.head(ctx)
	}

	file := filepath.Join(w.Path, path)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", fmt.Errorf("make parent directory for %s: %w", path, err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := w.git(ctx, "add", "--", path); err != nil {
		return "", err
	}

	args := []string{"commit", "--only", "-m", subject}
	for _, key := range slices.Sorted(maps.Keys(trailers)) {
		for _, value := range trailers[key] {
			args = append(args, "--trailer", key+": "+value)
		}
	}
	args = append(args, "--", path)
	if _, err := w.git(ctx, args...); err != nil {
		return "", err
	}
	return w.head(ctx)
}

// MergeMain merges main into the worktree branch and returns the resulting HEAD.
func (w Worktree) MergeMain(ctx context.Context) (string, error) {
	if _, err := w.git(ctx, "merge", "--no-edit", "main"); err != nil {
		if _, abortErr := w.git(ctx, "merge", "--abort"); abortErr != nil {
			return "", fmt.Errorf("merge main: %w; abort: %v", err, abortErr)
		}
		return "", fmt.Errorf("merge main: %w", err)
	}
	return w.head(ctx)
}

// LastCommitTouching returns the newest commit that changed path on this worktree's branch.
func (w Worktree) LastCommitTouching(path string) (string, error) {
	sha, err := w.git(context.Background(), "log", "-1", "--format=%H", "--", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// Show reads path from rev, which can be either a commit or a tree object.
func (r Repo) Show(ctx context.Context, rev, path string) ([]byte, error) {
	data, err := r.git(ctx, "show", rev+":"+path)
	if err != nil {
		return nil, err
	}
	return []byte(data), nil
}

// CommitTrailers returns the trailers recorded by sha, each key with its values in commit order.
func (r Repo) CommitTrailers(sha string) (map[string][]string, error) {
	output, err := r.git(context.Background(), "show", "-s", "--format=%(trailers:only,unfold)", sha)
	if err != nil {
		return nil, err
	}
	trailers := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		trailers[key] = append(trailers[key], strings.TrimSpace(value))
	}
	return trailers, nil
}

// MergeTree writes and returns the merge tree for m and u, with paths that conflict.
func (r Repo) MergeTree(ctx context.Context, m, u string) (tree string, conflicts []string, err error) {
	output, err := r.git(ctx, "merge-tree", "--write-tree", "--no-messages", "--name-only", m, u)
	tree, conflicts = parseMergeTree(output)
	if err == nil {
		return tree, conflicts, nil
	}
	if gitExitCode(err) == 1 && tree != "" {
		return tree, conflicts, nil
	}
	return "", nil, err
}

// CommitTree creates a commit with tree, parents in order, and msg.
func (r Repo) CommitTree(ctx context.Context, tree string, parents []string, msg string) (string, error) {
	args := make([]string, 0, 2+len(parents)*2)
	args = append(args, "commit-tree", tree)
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", msg)
	sha, err := r.git(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

func (r Repo) disableAutoGC(ctx context.Context) error {
	autoGCConfigMu.Lock()
	defer autoGCConfigMu.Unlock()

	value, err := r.git(ctx, "config", "--get", "gc.auto")
	if err == nil && strings.TrimSpace(value) == "0" {
		return nil
	}
	if err != nil && gitExitCode(err) != 1 {
		return fmt.Errorf("read automatic Git garbage collection setting: %w", err)
	}
	if _, err := r.git(ctx, "config", "gc.auto", "0"); err != nil {
		return fmt.Errorf("disable automatic Git garbage collection: %w", err)
	}
	return nil
}

func (r Repo) branchExists(ctx context.Context, branch string) (bool, error) {
	_, err := r.git(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if gitExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

func (r Repo) git(ctx context.Context, args ...string) (string, error) {
	return runGit(ctx, r.Path, args...)
}

func (w Worktree) git(ctx context.Context, args ...string) (string, error) {
	return runGit(ctx, w.Path, args...)
}

func (w Worktree) head(ctx context.Context) (string, error) {
	sha, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return stdout.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

func gitExitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func parseMergeTree(output string) (string, []string) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", nil
	}
	conflicts := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		if line = strings.TrimSpace(line); line != "" {
			conflicts = append(conflicts, line)
		}
	}
	return lines[0], conflicts
}
