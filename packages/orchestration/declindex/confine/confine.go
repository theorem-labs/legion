// Package confine runs a program in an operating-system sandbox that an unprivileged process can build, with no
// setuid helper and no user namespace: Landlock confines its files, and a seccomp filter denies it the system
// calls that reach beyond them.
//
//   - Files: it reads only beneath Spec.Read, writes (creates, removes, renames) only beneath Spec.Write, and
//     executes only its own program and that program's ELF interpreter.
//   - Network and other processes: every socket(2) fails, and so do io_uring, ptrace, process_vm_readv/writev,
//     the key-management calls, unshare, setns and mount. Landlock also denies TCP and UDP, abstract Unix
//     sockets and signals to processes outside the sandbox, on a kernel whose Landlock has them.
//   - Environment: exactly Spec.Env.
//
// A confined program is started through the current executable: Command re-runs it with the spec in the
// DECLINDEX_CONFINE variable, and this package's init function, which runs before anything else in that process,
// applies the sandbox to its own thread and executes the program in its place. Every binary that imports the
// package (the command, and the test binaries of the packages that use it) is therefore its own launcher.
package confine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

const envVar = "DECLINDEX_CONFINE"

// Spec is what a confined program may touch.
type Spec struct {
	// Argv is the program and its arguments; Argv[0] is a path.
	Argv []string `json:"argv"`
	// Env is the program's whole environment.
	Env []string `json:"env"`
	// Dir is the program's working directory.
	Dir string `json:"dir"`
	// Read are the files and directories it may read (a directory: everything beneath it).
	Read []string `json:"read"`
	// Write are the directories (or files) it may read and write.
	Write []string `json:"write"`
}

func init() {
	raw, ok := os.LookupEnv(envVar)
	if !ok {
		return
	}
	var s Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil || len(s.Argv) == 0 {
		fmt.Fprintf(os.Stderr, "confine: bad %s: %v\n", envVar, err)
		os.Exit(126)
	}
	// Landlock and seccomp apply to the calling thread; execve keeps only that thread, with both.
	runtime.LockOSThread()
	if s.Dir != "" {
		if err := os.Chdir(s.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "confine: %v\n", err)
			os.Exit(126)
		}
	}
	if err := apply(s); err != nil {
		fmt.Fprintf(os.Stderr, "confine: %v\n", err)
		os.Exit(126)
	}
	err := syscall.Exec(s.Argv[0], s.Argv, s.Env)
	fmt.Fprintf(os.Stderr, "confine: exec %s: %v\n", s.Argv[0], err)
	os.Exit(126)
}

// ErrUnavailable says that this machine cannot build the sandbox.
var ErrUnavailable = errors.New("the sandbox is unavailable")

// Available reports whether this machine can build the sandbox: a kernel with Landlock enabled, and an
// architecture whose seccomp filter this package writes.
func Available() error { return available() }

// Command is the confined program of spec, run through the current executable. It fails when the sandbox is
// unavailable: nothing then runs unconfined.
func Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if err := available(); err != nil {
		return nil, err
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("confine: no program")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self)
	cmd.Env = []string{envVar + "=" + string(raw)}
	cmd.Dir = spec.Dir
	return cmd, nil
}
