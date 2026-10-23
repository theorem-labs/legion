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

// Sync returns the synchronisation that RefreshAt runs while it holds the index writer lock. It refuses a dirty
// tree, fetches an otherwise absent commit, checks it out, and runs the configured build before the index reads it.
func Sync(cfg Config, commit string, log io.Writer) func(context.Context) error {
	return func(ctx context.Context) error {
		dirty, err := trackedChanges(ctx, cfg.Tree)
		if err != nil {
			return err
		}
		if dirty {
			return errors.New("the tree has tracked changes")
		}
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
		past, err := commitIsAncestor(ctx, cfg.Tree, commit, "HEAD")
		if err != nil {
			return err
		}
		if past {
			if log != nil {
				fmt.Fprintf(log, "declindex: index already covers %s\n", commit)
			}
			return ErrAlreadyPast
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

func trackedChanges(ctx context.Context, tree string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", tree, "status", "--porcelain", "--untracked-files=no")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("check tracked changes: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func commitIsAncestor(ctx context.Context, tree, ancestor, descendant string) (bool, error) {
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
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func runBuild(ctx context.Context, tree string, build []string, timeout time.Duration, log io.Writer) error {
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	cmd := exec.Command(build[0], build[1:]...)
	cmd.Dir = tree
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start build %q: %w", build[0], err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-wait:
		if err == nil {
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return &TempError{Err: fmt.Errorf("build %q ended with signal %s", build[0], status.Signal())}
			}
		}
		return fmt.Errorf("build %q: %w", build[0], err)
	case <-ctx.Done():
		killBuildGroup(cmd)
		<-wait
		return &TempError{Err: ctx.Err()}
	case <-timer.C:
		killBuildGroup(cmd)
		<-wait
		return &TempError{Err: fmt.Errorf("build %q timed out after %s", build[0], timeout)}
	}
}

func killBuildGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
