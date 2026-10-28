package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets a test run this binary as declindex itself (DECLINDEX_TEST_MAIN=1), to signal a real process.
func TestMain(m *testing.M) {
	if os.Getenv("DECLINDEX_TEST_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

// A hook timeout or retry sends SIGTERM to `declindex refresh`; the build it started, and the build's children,
// must not outlive it. The run says that a signal stopped it, not that the index's database is unreachable, and
// exits 75 so that it is run again.
func TestSIGTERMDuringRefreshKillsTheBuild(t *testing.T) {
	f := newLandingFixture(t)
	body, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	build := "build:\n  - sh\n  - -c\n  - 'sleep 300 & echo $! > pid; wait'\nbuild_timeout: 10m\n"
	text := strings.Replace(string(body), "build: [\"true\"]\nbuild_timeout: 1s\n", build, 1)
	if text == string(body) {
		t.Fatal("the landing fixture no longer sets the build")
	}
	writeIndexConfig(t, f.config, text)
	cmd := exec.Command(os.Args[0], "--index-dsn-file", indexDSNFile(f.config), "--config", f.config, "refresh")
	cmd.Env = append(os.Environ(), "DECLINDEX_TEST_MAIN=1", "LAND_COMMIT="+f.second)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(f.tree, "pid")
	var pid int
	for deadline := time.Now().Add(30 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the build did not start")
		}
		if raw, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(raw), "\n") {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 75 {
		t.Fatalf("refresh after SIGTERM: %v, want exit 75", err)
	}
	assertProcessGone(t, pid)
	if got := stderr.String(); !strings.Contains(got, "declindex: the run was stopped by a signal (terminated)") ||
		strings.Contains(got, "unreachable") {
		t.Errorf("refresh after SIGTERM says %q; want that a signal stopped it, and nothing of an unreachable database", got)
	}
}
