package messagetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// alive reports whether pid is a running process that still carries the run's ledger repository: a process that
// exited, or a zombie nobody has reaped, carries no environment.
func alive(pid int, run string) bool {
	return processEnv(pid)["LEGION_LEDGER_REPOSITORY"] == run
}

// An interrupted run tears down with the run's context already cancelled (acceptance run 7: cancelling the driver
// left its daemon, both owner panes and the bearer directory behind). The teardown stops the daemon by its own PID,
// the panes by the run's own tmux socket and every owner process it saw by its own PID, removes the bearer files, and
// reports what it stopped. A PID that now names a process of another run is left alone.
func TestTeardownStopsWhatTheRunStartedAfterTheRunContextEnded(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the teardown stops a real tmux server")
	}
	const run = "message-test-20261024t050000z-0a1b2c3d"
	socket := fmt.Sprintf("messagetest-teardown-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	pane := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "owners",
		"-e", "LEGION_OWNER="+st, "-e", "LEGION_LEDGER_REPOSITORY="+run, "sleep 300")
	if out, err := pane.CombinedOutput(); err != nil {
		t.Fatalf("start the owner pane: %v: %s", err, out)
	}

	// An owner process outside the pane that ignores the hangup kill-server sends, and a process of another run.
	start := func(repo string) *exec.Cmd {
		command := exec.Command("sh", "-c", `trap "" HUP; exec sleep 300`)
		command.Env = append(os.Environ(), "LEGION_LEDGER_REPOSITORY="+repo)
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
		return command
	}
	stray, other := start(run), start("message-test-another-run")
	daemon := exec.Command("sleep", "300")
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	tokens := filepath.Join(t.TempDir(), "message-test", run)
	if err := privateDir(tokens); err != nil {
		t.Fatal(err)
	}
	if _, err := operatorTokenFile(tokens); err != nil {
		t.Fatal(err)
	}

	d := &driver{
		opts: Options{Timeouts: Timeouts{DaemonStop: 5 * time.Second, Poll: 10 * time.Millisecond}},
		run:  run, socket: socket, tokenDir: tokens, report: &Report{},
		daemon: daemon, daemonDone: done,
		ownerPIDs: map[int]string{stray.Process.Pid: st, other.Process.Pid: st},
	}
	d.report.Teardown.DaemonPIDs = []int{daemon.Process.Pid}
	// The pane's processes as the run last saw them.
	deadline := time.Now().Add(10 * time.Second)
	for len(d.ownerPIDs) < 3 {
		if _, err := d.panes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane's process never showed: %v", d.ownerPIDs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var panePIDs []int
	for pid := range d.ownerPIDs {
		if pid != stray.Process.Pid && pid != other.Process.Pid {
			panePIDs = append(panePIDs, pid)
		}
	}

	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.teardown(interrupted); err != nil {
		t.Fatalf("teardown after the run's context ended: %v", err)
	}

	if !d.exited {
		t.Fatal("the daemon still runs")
	}
	if err := exec.Command("tmux", "-L", socket, "list-sessions").Run(); err == nil {
		t.Fatalf("the tmux server on %s still runs", socket)
	}
	for _, pid := range append(panePIDs, stray.Process.Pid) {
		if alive(pid, run) {
			t.Fatalf("the owner process %d still runs", pid)
		}
	}
	if !alive(other.Process.Pid, "message-test-another-run") {
		t.Fatal("the teardown stopped a process of another run")
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Fatalf("the bearer directory %s is left: %v", tokens, err)
	}
	report := d.report.Teardown
	if !slices.Contains(report.Stopped, stray.Process.Pid) || slices.Contains(report.Stopped, other.Process.Pid) || len(report.Left) != 0 ||
		!slices.Contains(report.OwnerPIDs, stray.Process.Pid) || report.TokenDir != tokens {
		t.Fatalf("the teardown's report = %+v", report)
	}
}
