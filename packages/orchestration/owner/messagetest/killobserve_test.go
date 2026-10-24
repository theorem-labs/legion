package messagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// A supervisor frees a dead owner's leases as soon as it sees its pane's process gone (the daemon on "supervise:
// process died", the harness supervisor's probe every 200 ms), so the item's holder at a kill cannot be read after
// the pane's processes are gone: a supervisor that freed the lease first leaves no holder, an after-answer kill then
// shows no holder, and an after-claim kill reads as before the claim. The kill reads the item while the pane's
// processes are stopped and still alive, so no supervisor has freed anything yet, and only then kills them. Here a
// stand-in supervisor frees the lease within a millisecond of the pane's process exiting; each kill must still read
// the holder its moment had.
func TestKillReadsTheItemBeforeTheSupervisorFreesTheDeadOwnersLease(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ctx := t.Context()
	store, err := ledger.Open(ctx, testpg.Database(t, "message_test_kill_observe"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	const repo = "r"
	if _, err := store.EnsureRepo(ctx, repo, 3); err != nil {
		t.Fatal(err)
	}
	socket := "mt-kill-observe-" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	d := &driver{
		opts: Options{Progress: io.Discard, Timeouts: Timeouts{DaemonStop: 10 * time.Second, Poll: 50 * time.Millisecond}},
		run:  repo, source: fn, target: st, store: store, tmux: tmuxServer{socket: socket}, bare: filepath.Join(t.TempDir(), "no-repo.git"), report: &Report{},
	}
	for index, test := range []struct {
		moment string
		claim  bool
	}{{MomentAfterClaim, true}, {MomentAfterClaim, true}, {MomentBeforeClaim, false}, {MomentAfterClaim, true}} {
		item := d.need("r" + strconv.Itoa(4+index))
		if err := store.Enqueue(ctx, repo, item, st); err != nil {
			t.Fatal(err)
		}
		if test.claim {
			if _, err := store.Claim(ctx, repo, item, st, 0); err != nil {
				t.Fatal(err)
			}
		}
		p := ownerPane(t, d, socket)
		freed := make(chan error, 1)
		go func() { freed <- freeWhenGone(store, repo, p.PID) }()

		holder, record, done, err := d.killObserving(ctx, p, item)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-freed; err != nil {
			t.Fatal(err)
		}
		if observed := observedMoment(st, holder, record, done); observed != test.moment || (test.claim && holder != st) {
			t.Fatalf("kill %d of %s: holder %q, record %q, done %t: landed %s, want %s (the supervisor freed the lease after the read)",
				index+1, item, holder, record, done, observed, test.moment)
		}
		if current, err := store.Item(ctx, repo, item); err != nil || current.Holder != "" {
			t.Fatalf("the stand-in supervisor did not free %s after the kill: %+v, %v", item, current, err)
		}
	}
}

// ownerPane starts a pane on socket whose process names the run, as an owner pane's does, and returns it.
func ownerPane(t *testing.T, d *driver, socket string) pane {
	t.Helper()
	args := []string{"-L", socket, "new-window", "-d", "-P", "-F", "#{pane_id}"}
	if exec.Command("tmux", "-L", socket, "has-session").Run() != nil {
		args = []string{"-L", socket, "new-session", "-d", "-P", "-F", "#{pane_id}"}
	}
	args = append(args, "env", "LEGION_OWNER="+st, "LEGION_LEDGER_REPOSITORY="+d.run, "sleep", "600")
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		panes, err := d.tmux.ownerPanes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := panes[st]; ok {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatal("the owner pane never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// freeWhenGone is a supervisor's reaction to a dead owner: it frees the owner's leases as soon as pid is gone.
func freeWhenGone(store *ledger.Store, repo string, pid int) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && zombie(stat) {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the pane's process never exited")
		}
		time.Sleep(time.Millisecond)
	}
	_, err := store.ReleaseHolder(context.Background(), repo, st, "owner pane stopped")
	return err
}

// zombie reports whether a /proc/<pid>/stat line is a process that exited and waits to be reaped.
func zombie(stat []byte) bool {
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && len(stat) > end+2 && stat[end+2] == 'Z'
}
