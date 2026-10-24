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

	panes, err := ownerPanes(context.Background(), socket)
	if err != nil || len(panes) != 0 {
		t.Fatalf("ownerPanes on a server with no session = %v, %v, want no pane and no error", panes, err)
	}
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
	list := func(context.Context, string) (map[string]pane, error) {
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
	list := func(context.Context, string) (map[string]pane, error) {
		return map[string]pane{st: {ID: "%1", Owner: st, Dir: "/srv/elsewhere"}}, nil
	}
	panes, seen, err := d.awaitPanes(context.Background(), list, time.Now())
	if err != nil || panes[st].Dir != "/srv/elsewhere" || seen[st] == 0 {
		t.Fatalf("awaitPanes = %v, %v, %v; want the pane as last seen and no error", panes, seen, err)
	}
}
