package messagetest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// alive reports whether pid is a running process that still carries the run's ledger repository: a process that
// exited, or a zombie nobody has reaped, carries no environment.
func alive(pid int, run string) bool {
	return processEnv(pid)["LEGION_LEDGER_REPOSITORY"] == run
}

// tmuxServerRuns reports whether a process runs whose command line names the tmux socket: a tmux server keeps the
// command line of the client that started it, which names its socket with -L, whatever directory the socket is in.
func tmuxServerRuns(socket string) bool {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && slices.ContainsFunc(bytes.Split(cmdline, []byte{0}), func(arg []byte) bool { return string(arg) == socket }) {
			return true
		}
	}
	return false
}

// An interrupted run tears down with the run's context already cancelled (acceptance run 7: cancelling the driver
// left its daemon, both owner panes and the bearer directory behind). The teardown stops the daemon by its own PID,
// the panes by the run's own tmux socket and every owner process it saw by its own PID, removes the bearer files and
// the run's tmux directory, which holds the socket (tmux never removes a socket file itself), and reports what it
// stopped. A PID that now names a process of another run is left alone.
func TestTeardownStopsWhatTheRunStartedAfterTheRunContextEnded(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the teardown stops a real tmux server")
	}
	const run = "message-test-20261024t050000z-0a1b2c3d"
	socket := fmt.Sprintf("messagetest-teardown-%d", os.Getpid())
	tmuxDir := filepath.Join(shortTempDir(t), "run", "tmux")
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server := tmuxServer{socket: socket, tmpDir: tmuxDir}
	t.Cleanup(func() { _ = server.killServer(context.Background()) })
	pane := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "owners",
		"-e", "LEGION_OWNER="+st, "-e", "LEGION_LEDGER_REPOSITORY="+run, "sleep 300")
	pane.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxDir)
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
	// The daemon's own secret files: each pane's boot token, which it writes under its state_dir in the run's
	// directory, <state_dir>/secrets/<claim token>.
	daemonState := filepath.Join(t.TempDir(), "run", "daemon", "state")
	secrets := filepath.Join(daemonState, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "legion-run-owner-struct_cmountpoint"), []byte("boot token"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := &driver{
		opts: Options{Timeouts: Timeouts{DaemonStop: 5 * time.Second, Poll: 10 * time.Millisecond}},
		run:  run, tmux: server, tokenDir: tokens, daemonState: daemonState, report: &Report{},
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
	if tmuxServerRuns(socket) {
		t.Fatalf("the tmux server on %s still runs", socket)
	}
	if _, err := os.Stat(tmuxDir); !os.IsNotExist(err) {
		t.Fatalf("the run's tmux directory %s, which holds its socket, is left: %v", tmuxDir, err)
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
	if _, err := os.Stat(secrets); !os.IsNotExist(err) {
		t.Fatalf("the daemon's secret files in %s are left in the run's directory: %v", secrets, err)
	}
	report := d.report.Teardown
	if !slices.Contains(report.Stopped, stray.Process.Pid) || slices.Contains(report.Stopped, other.Process.Pid) || len(report.Left) != 0 ||
		!slices.Contains(report.OwnerPIDs, stray.Process.Pid) || report.TokenDir != tokens || report.DaemonSecrets != secrets || report.TmuxDir != tmuxDir {
		t.Fatalf("the teardown's report = %+v", report)
	}
}
