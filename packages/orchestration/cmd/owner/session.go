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
	deployPath string // --deploy
	owner      string // --owner: the owner (or the person) the command acts for
	dsnFile    string // --ledger-dsn-file, which replaces the deployment's ledger_dsn_file

	loaded bool
	repo   string // the ledger repository: skeleton/owner.yaml's repo
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

// load reads the deployment and the copy's skeleton/owner.yaml at main, whose repo is the ledger repository.
func (s *session) load() error {
	if s.loaded {
		return nil
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
	s.config, s.repo, s.loaded = config, config.Repo, true
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

// ledger opens the ledger whose DSN is in --ledger-dsn-file, else in the deployment's ledger_dsn_file.
func (s *session) ledger() (*ledger.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	source, file := "--ledger-dsn-file", s.dsnFile
	if file == "" {
		source, file = "the deployment's ledger_dsn_file", s.deploy.LedgerDSNFile
	}
	store, err := ownerpkg.OpenLedgerFile(s.ctx, source, file)
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
