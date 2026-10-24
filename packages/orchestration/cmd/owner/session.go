package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// session holds what one command run has read: the deployment, the copy's configuration at main, the owner's
// identity and, once opened, the ledger.
type session struct {
	ctx        context.Context
	getenv     func(string) string
	cwd        string
	deployPath string

	loaded bool
	owner  string
	repo   string
	deploy ownerpkg.Deploy
	bare   ownerpkg.Repo
	config ownerpkg.Config
	store  *ledger.Store
}

func (s *session) close() {
	if s.store != nil {
		s.store.Close()
	}
}

func (s *session) load() error {
	if s.loaded {
		return nil
	}
	s.owner = strings.TrimSpace(s.getenv("LEGION_OWNER"))
	if s.owner == "" {
		return usagef("LEGION_OWNER is unset: it names the owner (or the person) this command acts for")
	}
	s.repo = strings.TrimSpace(s.getenv("LEGION_LEDGER_REPOSITORY"))
	if s.repo == "" {
		return usagef("LEGION_LEDGER_REPOSITORY is unset")
	}
	deploy, err := s.loadDeploy()
	if err != nil {
		return err
	}
	s.deploy, s.bare = deploy, ownerpkg.Repo{Path: deploy.BareRepo}
	config, err := ownerpkg.LoadConfig(&s.bare)
	if err != nil {
		return err
	}
	if config.Repo != s.repo {
		return fmt.Errorf("skeleton/owner.yaml names ledger repository %q, but LEGION_LEDGER_REPOSITORY is %q", config.Repo, s.repo)
	}
	s.config, s.loaded = config, true
	return nil
}

// loadDeploy reads the deployment file: --deploy, else $HOME/.config/owner/deploy.yaml.
func (s *session) loadDeploy() (ownerpkg.Deploy, error) {
	path := s.deployPath
	if path == "" {
		home := strings.TrimSpace(s.getenv("HOME"))
		if home == "" {
			return ownerpkg.Deploy{}, usagef("--deploy is not given and HOME is unset")
		}
		path = filepath.Join(home, ".config", "owner", "deploy.yaml")
	}
	return ownerpkg.LoadDeploy(path)
}

// ledger opens the ledger the way every owner process does: from LEGION_LEDGER_DSN_FILE (C8).
func (s *session) ledger() (*ledger.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	store, err := ownerpkg.OpenLedger(s.ctx, s.getenv)
	if err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}

func (s *session) git(args ...string) (string, error) {
	command := exec.CommandContext(s.ctx, "git", append([]string{"--git-dir=" + s.deploy.BareRepo}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// blob reads path at rev. found is false when rev or path does not exist.
func (s *session) blob(rev, file string) ([]byte, bool, error) {
	object, err := s.git("rev-parse", "--verify", "--quiet", rev+":"+file)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := s.git("cat-file", "blob", strings.TrimSpace(object))
	if err != nil {
		return nil, false, err
	}
	return []byte(data), true, nil
}

func (s *session) mainFile(file string) ([]byte, error) {
	data, found, err := s.blob("main", file)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("main has no %s", file)
	}
	return data, nil
}

func branch(owner string) string { return ownerpkg.Branch(owner) }

// lastCommit is the newest commit of rev that touched file, or "" when rev does not exist.
func (s *session) lastCommit(rev, file string) (string, error) {
	if _, err := s.git("rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	out, err := s.git("log", "-1", "--format=%H", rev, "--", file)
	return strings.TrimSpace(out), err
}

// worktree creates or reuses ./wt on the owner's branch. The path is absolute because git runs each worktree
// command from the bare repository.
func (s *session) worktree() (ownerpkg.Worktree, error) {
	cwd, err := filepath.Abs(s.cwd)
	if err != nil {
		return ownerpkg.Worktree{}, err
	}
	dir := filepath.Join(cwd, "wt")
	want := branch(s.owner)
	if _, err := os.Stat(dir); err == nil {
		command := exec.CommandContext(s.ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD", "--git-common-dir")
		out, err := command.Output()
		if err != nil {
			return ownerpkg.Worktree{}, refusef("%s exists and is not a worktree: %v", dir, err)
		}
		lines := strings.Fields(string(out))
		if len(lines) != 2 || lines[0] != want {
			return ownerpkg.Worktree{}, refusef("%s is a checkout of %v, not of %s", dir, lines, want)
		}
		common, bare := lines[1], s.deploy.BareRepo
		if !filepath.IsAbs(common) {
			common = filepath.Join(dir, common)
		}
		if !samePath(common, bare) {
			return ownerpkg.Worktree{}, refusef("%s belongs to %s, not to %s", dir, common, bare)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ownerpkg.Worktree{}, err
	}
	return s.bare.Worktree(s.ctx, dir, want, "main")
}

func samePath(left, right string) bool {
	l, errL := filepath.EvalSymlinks(left)
	r, errR := filepath.EvalSymlinks(right)
	return errL == nil && errR == nil && filepath.Clean(l) == filepath.Clean(r)
}
