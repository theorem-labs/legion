package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/declindex"
)

type landingFixture struct {
	config, tree, first, second string
}

func newLandingFixture(t *testing.T) landingFixture {
	t.Helper()
	config := fixture(t)
	root := filepath.Dir(config)
	seed := filepath.Join(root, "tree")
	git(t, seed, "init", "-q")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "first")
	first := git(t, seed, "rev-parse", "HEAD")
	remote := filepath.Join(root, "remote.git")
	git(t, root, "clone", "-q", "--bare", seed, remote)
	tree := filepath.Join(root, "landing-tree")
	git(t, root, "clone", "-q", remote, tree)
	if err := os.WriteFile(filepath.Join(seed, "landing.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "landing.txt")
	git(t, seed, "commit", "-q", "-m", "second")
	second := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "push", "-q", remote, "HEAD:refs/heads/master")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "tree: tree\n", "tree: landing-tree\n", 1) + "remote: " + remote + "\nbuild: [\"true\"]\nbuild_timeout: 1s\n")
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("initial refresh: %+v", r)
	}
	return landingFixture{config: config, tree: tree, first: first, second: second}
}

func (f landingFixture) refreshAt(t *testing.T, commit string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", f.config, "refresh"}, func(key string) string {
		if key == "LAND_COMMIT" {
			return commit
		}
		return ""
	}, &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func assertIndexCommit(t *testing.T, config, want string) {
	t.Helper()
	cfg, err := declindex.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := declindex.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Meta(context.Background(), "commit")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("index commit %q, want %q", got, want)
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err == nil && zombieProcess(raw) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed-out build process %d still exists", pid)
		case <-tick.C:
		}
	}
}

func TestAssertProcessGoneTreatsZombieAsGone(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("/proc is unavailable")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	path := filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "stat")
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err == nil && zombieProcess(raw) {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("child process did not become a zombie")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	assertProcessGone(t, cmd.Process.Pid)
}

func zombieProcess(raw []byte) bool {
	end := bytes.LastIndexByte(raw, ')')
	return end >= 0 && len(raw) > end+2 && raw[end+2] == 'Z'
}

func TestRefreshAtLandedCommit(t *testing.T) {
	t.Run("fetches builds and records the previous index commit", func(t *testing.T) {
		f := newLandingFixture(t)
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("refresh: %+v", r)
		}
		if got := git(t, f.tree, "rev-parse", "HEAD"); got != f.second {
			t.Fatalf("tree HEAD %s, want landed commit %s", got, f.second)
		}
		cfg, err := declindex.LoadConfig(f.config)
		if err != nil {
			t.Fatal(err)
		}
		s, err := declindex.Open(cfg.DB)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		commit, err := s.Meta(context.Background(), "commit")
		if err != nil {
			t.Fatal(err)
		}
		previous, err := s.Meta(context.Background(), "prev_commit")
		if err != nil {
			t.Fatal(err)
		}
		if commit != f.second || previous != f.first {
			t.Fatalf("index commit %q, prev_commit %q; want %q, %q", commit, previous, f.second, f.first)
		}
	})
	t.Run("refuses a dirty tracked tree", func(t *testing.T) {
		f := newLandingFixture(t)
		if err := os.WriteFile(filepath.Join(f.tree, "Golden.lean"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 3 {
			t.Fatalf("dirty refresh: %+v", r)
		}
		if got := git(t, f.tree, "rev-parse", "HEAD"); got != f.first {
			t.Fatalf("tree moved to %s after dirty refresh", got)
		}
	})
	t.Run("classifies a missing remote as temporary", func(t *testing.T) {
		f := newLandingFixture(t)
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "remote: "+filepath.Join(filepath.Dir(f.config), "remote.git"), "remote: /does-not-exist", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 75 {
			t.Fatalf("missing-remote refresh: %+v", r)
		}
	})
	t.Run("refuses a missing commit without a remote", func(t *testing.T) {
		f := newLandingFixture(t)
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "remote: "+filepath.Join(filepath.Dir(f.config), "remote.git")+"\n", "", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 3 {
			t.Fatalf("missing-commit refresh: %+v", r)
		}
	})
	t.Run("retries a failed build from the stale index", func(t *testing.T) {
		f := newLandingFixture(t)
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", "build: [\"false\"]", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 3 {
			t.Fatalf("failed-build refresh: %+v", r)
		}
		retryBuild := "build:\n  - sh\n  - -c\n  - 'echo retry-build > retry-build'"
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", retryBuild, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("failed-build retry: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.tree, "retry-build")); err != nil {
			t.Fatal(err)
		}
		assertIndexCommit(t, f.config, f.second)
	})
	t.Run("retries a timed-out build from the stale index", func(t *testing.T) {
		f := newLandingFixture(t)
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		build := "build:\n  - sh\n  - -c\n  - 'sleep 30 & echo $! > pid; wait'"
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", build, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 75 {
			t.Fatalf("timed-out build: %+v", r)
		}
		raw, err := os.ReadFile(filepath.Join(f.tree, "pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		assertProcessGone(t, pid)
		retryBuild := "build:\n  - sh\n  - -c\n  - 'echo retry-timeout > retry-timeout'"
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", retryBuild, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("timed-out build retry: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.tree, "retry-timeout")); err != nil {
			t.Fatal(err)
		}
		assertIndexCommit(t, f.config, f.second)
	})
	t.Run("retries a failed index refresh from the stale index", func(t *testing.T) {
		f := newLandingFixture(t)
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "lib: .\n", "lib: missing\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 3 {
			t.Fatalf("failed index refresh: %+v", r)
		}
		retryBuild := "build:\n  - sh\n  - -c\n  - 'echo retry-refresh > retry-refresh'"
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", retryBuild, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("failed index refresh retry: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.tree, "retry-refresh")); err != nil {
			t.Fatal(err)
		}
		assertIndexCommit(t, f.config, f.second)
	})
	t.Run("repairs a graph write that failed after the index commit", func(t *testing.T) {
		f := newLandingFixture(t)
		cfg, err := declindex.LoadConfig(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(cfg.DB + ".graph"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cfg.DB+".graph.tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 3 {
			t.Fatalf("graph write failure: %+v", r)
		}
		if err := os.Remove(cfg.DB + ".graph.tmp"); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		retryBuild := "build:\n  - sh\n  - -c\n  - 'echo retry-graph > retry-graph'"
		if err := os.WriteFile(f.config, []byte(strings.Replace(string(body), "build: [\"true\"]", retryBuild, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("graph write retry: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.tree, "retry-graph")); err != nil {
			t.Fatal(err)
		}
		assertIndexCommit(t, f.config, f.second)
		if r := invoke(t, "--config", f.config, "users", "Frame"); r.code != 0 {
			t.Fatalf("query after graph repair: %+v", r)
		}
	})
	t.Run("leaves a clean older tree untouched when the index is already current", func(t *testing.T) {
		f := newLandingFixture(t)
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("refresh to second commit: %+v", r)
		}
		git(t, f.tree, "checkout", "-q", "--detach", f.first)
		if r := f.refreshAt(t, f.second); r.code != 0 {
			t.Fatalf("covered refresh: %+v", r)
		}
		if r := f.refreshAt(t, f.first); r.code != 0 || !strings.Contains(r.errOut, f.second) || !strings.Contains(r.errOut, f.first) {
			t.Fatalf("ancestor covered refresh: %+v", r)
		}
		if got := git(t, f.tree, "rev-parse", "HEAD"); got != f.first {
			t.Fatalf("covered refresh moved the tree to %s", got)
		}
	})
}
