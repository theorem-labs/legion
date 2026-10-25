package declindex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startsGrandchild writes a program that starts a long sleep, records its pid in pidFile, and waits for it: the
// shape of Lake running `lean`, or of an exporter that outlives a cancelled refresh.
func startsGrandchild(t *testing.T, dir, pidFile string) string {
	t.Helper()
	path := filepath.Join(dir, "starts-grandchild")
	script := "#!/bin/sh\nsleep 300 &\necho $! > '" + pidFile + "'\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the program did not start its child")
	return 0
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if end := bytes.LastIndexByte(raw, ')'); err == nil && end >= 0 && len(raw) > end+2 && raw[end+2] == 'Z' {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d outlived the cancelled command", pid)
}

// cancelledResult waits for a cancelled command. Without the group kill, the orphaned child keeps the command's
// output pipe open, and the command does not return until that child ends.
func cancelledResult(t *testing.T, done <-chan error, pid int) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the cancelled command did not return while its child %d ran", pid)
		return nil
	}
}

func TestCancelledToolchainCommandKillsItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	program := startsGrandchild(t, dir, pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, dir, []string{"PATH=/usr/bin:/bin"}, program) }()
	pid := waitForPid(t, pidFile)
	cancel()
	if err := cancelledResult(t, done, pid); err == nil {
		t.Fatal("the cancelled command succeeded")
	}
	requireGone(t, pid)
}

func TestCancelledExporterKillsItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	x := &Exporter{Lean: Lean{Prefix: dir}, Bin: startsGrandchild(t, dir, pidFile), Jobs: 1, ScratchDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := x.Run(ctx, Manifest{Modules: []string{"M"}})
		done <- err
	}()
	pid := waitForPid(t, pidFile)
	cancel()
	if err := cancelledResult(t, done, pid); err == nil {
		t.Fatal("the cancelled export succeeded")
	}
	requireGone(t, pid)
}
