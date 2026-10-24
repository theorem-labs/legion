package declindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrAlreadyPast means the index already covers the requested landed commit.
var ErrAlreadyPast = errors.New("the index already covers the landed commit")

// TempError is a retryable failure, such as a remote fetch or a timed-out build.
type TempError struct {
	Err error
}

func (e *TempError) Error() string { return e.Err.Error() }

func (e *TempError) Unwrap() error { return e.Err }

// Sync returns the synchronisation that RefreshAt runs while it holds the index writer lock: it fetches an
// otherwise absent commit, checks it out, and runs the configured build before the index reads it. Pass it with
// CheckLandedCoverage: RefreshAt then refuses a tree with tracked changes before it runs it.
func Sync(cfg Config, commit string, log io.Writer) func(context.Context) error {
	return func(ctx context.Context) error {
		has, err := hasCommit(ctx, cfg.Tree, commit)
		if err != nil {
			return err
		}
		if !has && cfg.Remote != "" {
			if err := runGit(ctx, cfg.Tree, log, "fetch", "--no-tags", cfg.Remote, "+refs/heads/*:refs/declindex/remote/*"); err != nil {
				return &TempError{Err: fmt.Errorf("fetch %q: %w", cfg.Remote, err)}
			}
			has, err = hasCommit(ctx, cfg.Tree, commit)
			if err != nil {
				return err
			}
		}
		if !has {
			return fmt.Errorf("the tree lacks %s", commit)
		}
		if err := runGit(ctx, cfg.Tree, log, "checkout", "--detach", "--quiet", commit); err != nil {
			return fmt.Errorf("checkout %s: %w", commit, err)
		}
		if len(cfg.Build) == 0 {
			return nil
		}
		return runBuild(ctx, cfg.Tree, cfg.Build, cfg.BuildTimeout, log)
	}
}

// indexCoversLandedCommit reports whether the complete persisted index has already read commit or a descendant,
// with its compiled part too when compiled is set. RefreshAt calls it while holding its writer lock, so a retry
// never mistakes a tree left at a failed build, or an index whose compiled step failed, for an advanced index.
func (s *Store) indexCoversLandedCommit(ctx context.Context, tree, commit string, compiled bool) (string, error) {
	indexed, err := s.Meta(ctx, "commit")
	if err != nil {
		return "", err
	}
	if indexed == "" {
		return "", nil
	}
	indexErrors, err := s.Meta(ctx, "errors")
	if err != nil {
		return "", err
	}
	if indexErrors != "0" {
		return "", nil
	}
	graph, err := s.Meta(ctx, "graph")
	if err != nil {
		return "", err
	}
	if graph == "" || graphToken(s.path+".graph") != graph {
		return "", nil
	}
	if compiled {
		at, err := s.Meta(ctx, "compiled_commit")
		if err != nil {
			return "", err
		}
		if at != indexed {
			return "", nil
		}
	}
	if indexed == commit {
		return indexed, nil
	}
	has, err := hasCommit(ctx, tree, commit)
	if err != nil {
		return "", err
	}
	if !has {
		return "", nil
	}
	covers, err := CommitIsAncestor(ctx, tree, commit, indexed)
	if err != nil || !covers {
		return "", err
	}
	return indexed, nil
}

// requireCleanTree refuses a tree with tracked changes, where a landed sync would check out another commit.
func requireCleanTree(ctx context.Context, tree string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", tree, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return fmt.Errorf("check tracked changes: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return errors.New("the tree has tracked changes")
	}
	return nil
}

// IsObjectID reports whether id is a full Git object id as Git prints it: 40 lowercase hexadecimal characters.
// A commit that the index records, or that is compared with the commits it records (LAND_COMMIT), must be one,
// since the comparison is of text. Git itself also reads the uppercase form.
func IsObjectID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// TreeHead is the commit that the Git work tree at tree has checked out. A tree that is not a Git work tree, or
// whose branch has no commit yet, is an error: it has no commit to compare an index's commit with.
func TreeHead(ctx context.Context, tree string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", tree, "rev-parse", "HEAD").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) != 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("read the commit of the tree %s: %w", tree, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// CommitIsAncestor reports whether ancestor is descendant or one of its ancestors, in the Git repository of tree.
func CommitIsAncestor(ctx context.Context, tree, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "merge-base", "--is-ancestor", ancestor, descendant)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("compare commits %s and %s: %w", ancestor, descendant, err)
}

func hasCommit(ctx context.Context, tree, commit string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "cat-file", "-e", commit+"^{commit}")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, fmt.Errorf("find %s: %w", commit, err)
}

func runGit(ctx context.Context, tree string, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", tree}, args...)...)
	cmd.Stdout, cmd.Stderr = log, log
	return cmd.Run()
}

// runBuild runs the configured build in tree, in a process group of its own (InGroup) that the end of ctx, or of
// timeout after the build started, kills. Both ends, and a build that a signal ended, are a TempError: the landing
// program runs it again.
func runBuild(ctx context.Context, tree string, build []string, timeout time.Duration, log io.Writer) error {
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(runCtx, build[0], build[1:]...)
	cmd.Dir = tree
	cmd.Stdout, cmd.Stderr = log, log
	killed := InGroup(cmd)
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return &TempError{Err: ctx.Err()}
		}
		return fmt.Errorf("start build %q: %w", build[0], err)
	}
	timer := time.AfterFunc(timeout, stop)
	defer timer.Stop()
	err := cmd.Wait()
	switch {
	case err == nil:
		return nil
	case killed() && ctx.Err() != nil:
		return &TempError{Err: ctx.Err()}
	case killed():
		return &TempError{Err: fmt.Errorf("build %q timed out after %s", build[0], timeout)}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return &TempError{Err: fmt.Errorf("build %q ended with signal %s", build[0], status.Signal())}
		}
	}
	return fmt.Errorf("build %q: %w", build[0], err)
}
