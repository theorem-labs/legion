// Package confine runs one program in an operating-system sandbox that an unprivileged process can build, with
// no setuid helper and no user namespace: Landlock confines its files, and a seccomp filter its system calls.
//
//   - Files: it reads only beneath Spec.Read, its scratch directory and the system files that a dynamically linked
//     program needs to start (the shared libraries, the loader's cache and /dev/urandom); reads and writes
//     /dev/null; executes only the program and its ELF interpreter; writes (creates, removes, truncates) only in
//     its scratch directory; and links or renames nothing across directories (Landlock REFER is never granted).
//     The scratch directory must be fresh: this user's, mode 0700 and empty; the inputs are copied into it, never
//     linked.
//   - Processes: the program is the only process. Its one exec is the start of the program itself; every later
//     execve and execveat fails, and so does every clone that is not a thread (fork, vfork, clone3, posix_spawn).
//   - Network and the rest: every socket(2) fails, and so do io_uring, ptrace, process_vm_readv/writev, the
//     key-management calls, bpf, unshare, setns and mount. Landlock also denies TCP and UDP, abstract Unix
//     sockets and signals to processes outside the sandbox, on a kernel whose Landlock has them.
//   - Environment: exactly Spec.Env.
//
// Landlock ABI 3 (Linux 6.2) is required, the first that governs truncation; on an older kernel the sandbox is
// unavailable and nothing runs.
//
// A confined program is started through the current executable: Command re-runs it with the spec in the
// DECLINDEX_CONFINE variable, and this package's init function, which runs before anything else in that process,
// applies the sandbox to its own thread and executes the program in its place. The seccomp filter sends that one
// exec to the parent, which lets it through and then closes the notification channel, after which the kernel
// fails every exec. Every binary that imports the package (the command, and the test binaries of the packages that
// use it) is therefore its own launcher.
package confine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const envVar = "DECLINDEX_CONFINE"

// Spec is what a confined program may touch.
type Spec struct {
	// Argv is the program and its arguments; Argv[0] is a path.
	Argv []string `json:"argv"`
	// Env is the program's whole environment.
	Env []string `json:"env"`
	// Scratch is the one directory the program may write, and its working directory. It must be fresh for the
	// run (NewScratch makes one): this user's, closed to others, and empty.
	Scratch string `json:"scratch"`
	// Inputs are copied into Scratch before the program starts.
	Inputs []Input `json:"-"`
	// Read are the files and directories it may read (a directory: everything beneath it).
	Read []string `json:"read"`
}

// Input is a file the program receives in its scratch directory.
type Input struct {
	// Name is its path below the scratch directory.
	Name string
	Data []byte
	// Mode is its permission bits; zero is 0600.
	Mode os.FileMode
}

// childFD is the file descriptor on which the launcher sends its seccomp notification channel to the parent.
const childFD = 3

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
	if err := os.Chdir(s.Scratch); err != nil {
		fmt.Fprintf(os.Stderr, "confine: %v\n", err)
		os.Exit(126)
	}
	if err := apply(s, childFD); err != nil {
		fmt.Fprintf(os.Stderr, "confine: %v\n", err)
		os.Exit(126)
	}
	err := syscall.Exec(s.Argv[0], s.Argv, s.Env)
	fmt.Fprintf(os.Stderr, "confine: exec %s: %v\n", s.Argv[0], err)
	os.Exit(126)
}

// ErrUnavailable says that this machine cannot build the sandbox.
var ErrUnavailable = errors.New("the sandbox is unavailable")

// Available reports whether this machine can build the sandbox: a kernel with Landlock ABI 3 or newer, seccomp
// notification, and an architecture whose seccomp filter this package writes.
func Available() error { return available() }

// NewScratch makes a fresh scratch directory below base (the system's temporary directory when empty).
func NewScratch(base string) (string, error) {
	d, err := os.MkdirTemp(base, "confine-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// checkScratch refuses a scratch directory that is not fresh: a symlink, another user's, open to others, or not
// empty. An entry placed before the run, such as a hard link to a file outside, would reach that file.
func checkScratch(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("confine: the scratch directory %q is not an absolute path", dir)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("confine: scratch: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("confine: the scratch %s is not a directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("confine: the scratch %s is not this user's", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("confine: the scratch %s is open to others (%v)", dir, st.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("confine: the scratch %s is not empty (%s, …): a scratch directory is fresh for each run", dir, entries[0].Name())
	}
	return nil
}

// writeInputs copies the inputs into the (checked, empty) scratch directory, creating each file new.
func writeInputs(dir string, inputs []Input) error {
	for _, in := range inputs {
		// IsLocal refuses an empty, absolute or escaping name, and accepts "." (the scratch directory itself).
		if !filepath.IsLocal(in.Name) || filepath.Clean(in.Name) == "." {
			return fmt.Errorf("confine: the input name %q is not a path below the scratch directory", in.Name)
		}
		p := filepath.Join(dir, filepath.Clean(in.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		mode := in.Mode
		if mode == 0 {
			mode = 0o600
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
		if err != nil {
			return err
		}
		if _, err := f.Write(in.Data); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Command is the confined program of spec, run through the current executable. It fails when the sandbox is
// unavailable or the scratch directory is not fresh: nothing then runs.
func Command(ctx context.Context, spec Spec) (*Cmd, error) {
	if err := available(); err != nil {
		return nil, err
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("confine: no program")
	}
	if err := checkScratch(spec.Scratch); err != nil {
		return nil, err
	}
	if err := writeInputs(spec.Scratch, spec.Inputs); err != nil {
		return nil, err
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
	cmd.Dir = spec.Scratch
	return &Cmd{Cmd: cmd}, nil
}

// Cmd is a confined program. Start (and Run, CombinedOutput) also lets through the program's one exec; do not call
// the embedded exec.Cmd's Start or Output, which would not.
type Cmd struct {
	*exec.Cmd
}

// Start starts the launcher, lets its exec of the program through, and closes the channel that would let another.
func (c *Cmd) Start() error { return start(c.Cmd) }

// Run starts the program and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// CombinedOutput runs the program and returns its standard output and error together.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil || c.Stderr != nil {
		return nil, errors.New("confine: Stdout or Stderr already set")
	}
	var out strings.Builder
	// exec.Cmd writes one comparable writer from one goroutine at a time.
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return []byte(out.String()), err
}
