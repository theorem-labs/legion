package messagetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The supervisor's private tmux server can be up with no session: tmux then refuses `list-panes -a` with "no
// current target". A run that polls while the supervisor starts its first owner sees exactly that, and must read it
// as no owner pane yet, as it reads a socket with no server.
func TestOwnerPanesOfAServerWithNoSessionIsNone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the pane listing reads a real tmux server")
	}
	socket := fmt.Sprintf("messagetest-empty-%d", os.Getpid())
	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "start-server", ";", "set-option", "-g", "exit-empty", "off")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start an empty tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	panes, err := tmuxServer{socket: socket}.ownerPanes(context.Background())
	if err != nil || len(panes) != 0 {
		t.Fatalf("ownerPanes on a server with no session = %v, %v, want no pane and no error", panes, err)
	}
}

// The run's tmux server is under the run's own TMUX_TMPDIR, which the driver gives every tmux command of its own, as it
// gives it the daemon: the driver finds the panes of a server started there, the same socket name in tmux's default
// directory is no server of the run's, and killing the run's server stops it.
func TestTheDriversTmuxCommandsUseTheRunsTmuxDirectory(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the pane listing reads a real tmux server")
	}
	ctx := context.Background()
	dir := shortTempDir(t)
	run := tmuxServer{socket: fmt.Sprintf("mt-dir-%d", os.Getpid()), tmpDir: dir}
	start := exec.Command("tmux", "-L", run.socket, "-f", "/dev/null", "new-session", "-d", "-e", "LEGION_OWNER="+st, "sleep 300")
	start.Env = append(os.Environ(), "TMUX_TMPDIR="+dir)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start a server under %s: %v: %s", dir, err, out)
	}
	t.Cleanup(func() { _ = run.killServer(ctx) })
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), run.socket)); err != nil {
		t.Fatalf("the server's socket is not under %s: %v", dir, err)
	}
	if panes, err := run.ownerPanes(ctx); err != nil || len(panes) != 1 || panes[st].ID == "" {
		t.Fatalf("the run's server lists %v, %v; want the owner pane of %s", panes, err, st)
	}
	if panes, err := (tmuxServer{socket: run.socket}).ownerPanes(ctx); err != nil || len(panes) != 0 {
		t.Fatalf("tmux's default directory lists %v, %v for %s; want no server of that name there", panes, err, run.socket)
	}
	if err := run.killServer(ctx); err != nil {
		t.Fatal(err)
	}
	if panes, err := run.ownerPanes(ctx); err != nil || len(panes) != 0 {
		t.Fatalf("after killServer the run's server lists %v, %v", panes, err)
	}
}

// shortTempDir is a temporary directory with a short path: a tmux socket under it must fit a Unix socket's 107-byte
// path, which a directory named for the test, as t.TempDir's is, can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ownerDirFixture is a supervisor state directory with its owners directory, the working directory the supervisor
// gives node there (links resolved, as /proc shows a working directory), and a driver of node over it.
func ownerDirFixture(t *testing.T, node string, paneStart time.Duration) (*driver, string) {
	t.Helper()
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "owners"), 0o755); err != nil {
		t.Fatal(err)
	}
	owners, err := filepath.EvalSymlinks(filepath.Join(state, "owners"))
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{nodes: []string{node}, daemonState: state, opts: Options{Timeouts: Timeouts{PaneStart: paneStart, Poll: time.Millisecond}}}
	return d, filepath.Join(owners, "legion-x-owner-"+EncodeOwnerName(node))
}

// The supervisor's pane runs `cd <owner directory> && legion worker-shim …` under a shell that starts in the tmux
// server's directory, so a pane first seen there is still starting. The wake step judges the launch (C8, C9) once
// the pane works in its owner directory.
func TestAwaitPanesWaitsForEachPaneToReachItsOwnerDirectory(t *testing.T) {
	d, want := ownerDirFixture(t, st, 10*time.Second)
	listings := 0
	list := func(context.Context) (map[string]pane, error) {
		listings++
		dir := "/srv/where-the-server-started"
		if listings >= 3 {
			dir = want
		}
		return map[string]pane{st: {ID: "%1", Owner: st, Dir: dir}}, nil
	}
	panes, seen, err := d.awaitPanes(context.Background(), list, time.Now())
	if err != nil || panes[st].Dir != want || seen[st] == 0 || listings != 3 {
		t.Fatalf("awaitPanes = %v, %v, %v after %d listings; want the pane in %s, first seen at the first listing", panes, seen, err, listings, want)
	}
}

// A pane that is there but never reaches its owner directory within the pane timeout is a launch to judge, not a
// pane that never started: the wait returns the pane as last seen, so the launch check names its directory.
func TestAwaitPanesReturnsAPaneThatNeverReachesItsOwnerDirectory(t *testing.T) {
	d, _ := ownerDirFixture(t, st, 50*time.Millisecond)
	list := func(context.Context) (map[string]pane, error) {
		return map[string]pane{st: {ID: "%1", Owner: st, Dir: "/srv/elsewhere"}}, nil
	}
	panes, seen, err := d.awaitPanes(context.Background(), list, time.Now())
	if err != nil || panes[st].Dir != "/srv/elsewhere" || seen[st] == 0 {
		t.Fatalf("awaitPanes = %v, %v, %v; want the pane as last seen and no error", panes, seen, err)
	}
}
