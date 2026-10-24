package messagetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
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
