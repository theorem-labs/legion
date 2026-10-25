package declindex

import (
	"os/exec"
	"sync/atomic"
	"syscall"
)

// InGroup makes cmd, made with exec.CommandContext, the leader of a new process group, and has the end of its
// context kill that whole group, so that the programs cmd starts (Lake's `lean` and compiler processes, a build's
// jobs, a librarian's helpers) do not outlive it. It is the one way this module runs a process group. killed
// reports, once cmd.Wait has returned, whether the end of the context killed the group, so that a caller tells its
// own timeout or cancellation from the program's own failure and keeps its own errors.
func InGroup(cmd *exec.Cmd) (killed func() bool) {
	var k atomic.Bool
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		k.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return k.Load
}
