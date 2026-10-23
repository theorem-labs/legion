//go:build linux

package confine

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// systemReads are what a dynamically linked program needs to start: the shared libraries and the loader's cache,
// and the random device. None holds anything of the user's.
var systemReads = []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64", "/usr/local/lib", "/etc/ld.so.cache",
	"/dev/urandom"}

// minABI is the first Landlock ABI that governs truncation (Linux 6.2). Before it, a confined process could empty
// any file it may read.
const minABI = 3

const (
	fsV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	// The rights that apply to a file (the others apply to directories only).
	fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	readRights = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	execRights = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_EXECUTE
	udpNet     = unix.LANDLOCK_ACCESS_NET_BIND_UDP | unix.LANDLOCK_ACCESS_NET_CONNECT_SEND_UDP
	tcpNet     = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	allScopes  = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
)

// landlockVersion asks the kernel for its Landlock ABI; a test replaces it.
var landlockVersion = func() (int, error) {
	abi, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, e
	}
	return int(abi), nil
}

func landlockABI() (int, error) {
	abi, err := landlockVersion()
	if err != nil {
		return 0, fmt.Errorf("%w: Landlock: %v", ErrUnavailable, err)
	}
	if abi < minABI {
		return 0, fmt.Errorf("%w: Landlock ABI %d does not govern truncation; ABI %d (Linux 6.2) is needed", ErrUnavailable, abi, minABI)
	}
	return abi, nil
}

func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	}
	return 0, fmt.Errorf("%w: no seccomp filter for %s", ErrUnavailable, runtime.GOARCH)
}

func available() error {
	if _, err := auditArch(); err != nil {
		return err
	}
	_, err := landlockABI()
	return err
}

func handledFS(abi int) uint64 {
	fs := uint64(fsV1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	if abi >= 5 {
		fs |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return fs
}

// createRuleset handles every right the kernel knows: whatever no rule grants is denied.
func createRuleset(abi int) (int, uint64, error) {
	attr := unix.LandlockRulesetAttr{Handled_access_fs: handledFS(abi)}
	size := uintptr(8)
	if abi >= 4 {
		attr.Handled_access_net, size = tcpNet|udpNet, 16
	}
	if abi >= 6 {
		attr.Scoped, size = allScopes, 24
	}
	for {
		fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
		if e == 0 {
			return int(fd), attr.Handled_access_fs, nil
		}
		// A kernel whose Landlock has TCP but not yet UDP rules.
		if e == unix.EINVAL && attr.Handled_access_net&udpNet != 0 {
			attr.Handled_access_net &^= udpNet
			continue
		}
		return -1, 0, fmt.Errorf("landlock_create_ruleset: %v", e)
	}
}

func addRule(ruleset int, path string, access, handled uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fileRights
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access & handled, Parent_fd: int32(fd)}
	if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); e != 0 {
		return fmt.Errorf("landlock_add_rule %s: %v", path, e)
	}
	return nil
}

// interpreter is the ELF interpreter (the dynamic loader) of program, empty for a static one.
func interpreter(program string) (string, error) {
	f, err := elf.Open(program)
	if err != nil {
		return "", nil // not ELF (a script): its interpreter line names a program it cannot execute
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b := make([]byte, p.Filesz)
			if _, err := p.ReadAt(b, 0); err != nil {
				return "", err
			}
			for len(b) > 0 && b[len(b)-1] == 0 {
				b = b[:len(b)-1]
			}
			return filepath.EvalSymlinks(string(b))
		}
	}
	return "", nil
}

func landlock(s Spec) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	ruleset, handled, err := createRuleset(abi)
	if err != nil {
		return err
	}
	defer unix.Close(ruleset)
	add := func(paths []string, access uint64, optional bool) error {
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil && optional {
				continue
			}
			if err := addRule(ruleset, p, access, handled); err != nil {
				return err
			}
		}
		return nil
	}
	interp, err := interpreter(s.Argv[0])
	if err != nil {
		return err
	}
	exec := []string{s.Argv[0]}
	if interp != "" {
		exec = append(exec, interp)
	}
	// The scratch directory: everything but executing, and linking or renaming across directories (REFER), which
	// would bring a file in from outside or take one out.
	writeRights := handled &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_REFER)
	for _, step := range []struct {
		paths    []string
		access   uint64
		optional bool
	}{
		{systemReads, readRights, true},
		{[]string{"/dev/null"}, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, true},
		{s.Read, readRights, false},
		{[]string{s.Scratch}, writeRights, false},
		{exec, execRights, false},
	} {
		if err := add(step.paths, step.access, step.optional); err != nil {
			return err
		}
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); e != 0 {
		return fmt.Errorf("landlock_restrict_self: %v", e)
	}
	return nil
}

// deniedSyscalls fail with EPERM: they reach the network, other processes, the kernel's keyrings, or new
// namespaces and mounts. forkSyscalls (per architecture) are denied too.
var deniedSyscalls = []uint32{unix.SYS_SOCKET, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER,
	unix.SYS_IO_URING_REGISTER, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_MOUNT,
	unix.SYS_BPF}

// insn is one classic BPF instruction whose jumps name labels; "" is the next instruction.
type insn struct {
	label  string
	code   uint16
	k      uint32
	jt, jf string
}

func assemble(prog []insn) ([]unix.SockFilter, error) {
	at := map[string]int{}
	for i, in := range prog {
		if in.label != "" {
			at[in.label] = i
		}
	}
	out := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		out[i] = unix.SockFilter{Code: in.code, K: in.k}
		for _, j := range []struct {
			label string
			dst   *uint8
		}{{in.jt, &out[i].Jt}, {in.jf, &out[i].Jf}} {
			if j.label == "" {
				continue
			}
			t, ok := at[j.label]
			off := t - (i + 1)
			if !ok || off < 0 || off > 255 {
				return nil, fmt.Errorf("seccomp filter: no forward jump to %q from %d", j.label, i)
			}
			*j.dst = uint8(off)
		}
	}
	return out, nil
}

// filter is the seccomp program: the denied calls fail; execve and execveat go to the parent (which lets the
// launcher's one exec through, then closes the channel so that the kernel fails every later one with ENOSYS);
// clone3 reports ENOSYS, so that the C library falls back to clone; and clone succeeds only for a thread.
func filter() ([]unix.SockFilter, error) {
	arch, err := auditArch()
	if err != nil {
		return nil, err
	}
	const (
		ldAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset  = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret   = unix.BPF_RET | unix.BPF_K
		// Offsets in struct seccomp_data.
		offNr, offArch, offArg0 = 0, 4, 16
	)
	p := []insn{
		{code: ldAbs, k: offArch},
		{code: jeq, k: arch, jf: "kill"},
		{code: ldAbs, k: offNr},
	}
	if arch == unix.AUDIT_ARCH_X86_64 {
		p = append(p, insn{code: jge, k: 0x40000000, jt: "kill"}) // the x32 system calls
	}
	for _, nr := range append(append([]uint32(nil), deniedSyscalls...), forkSyscalls...) {
		p = append(p, insn{code: jeq, k: nr, jt: "eperm"})
	}
	p = append(p,
		insn{code: jeq, k: unix.SYS_CLONE3, jt: "enosys"},
		insn{code: jeq, k: unix.SYS_EXECVE, jt: "notify"},
		insn{code: jeq, k: unix.SYS_EXECVEAT, jt: "notify"},
		insn{code: jeq, k: unix.SYS_CLONE, jt: "clone"},
		insn{code: ret, k: unix.SECCOMP_RET_ALLOW},
		insn{label: "eperm", code: ret, k: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		insn{label: "enosys", code: ret, k: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		insn{label: "notify", code: ret, k: unix.SECCOMP_RET_USER_NOTIF},
		insn{label: "kill", code: ret, k: unix.SECCOMP_RET_KILL_PROCESS},
		insn{label: "clone", code: ldAbs, k: offArg0}, // the flags' low word
		insn{code: jset, k: unix.CLONE_THREAD, jf: "clone_deny"},
		insn{code: ret, k: unix.SECCOMP_RET_ALLOW},
		insn{label: "clone_deny", code: ret, k: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
	)
	return assemble(p)
}

// seccomp installs the filter on this thread and sends its notification channel to the parent over notifyFD.
func seccomp(notifyFD int) error {
	prog, err := filter()
	if err != nil {
		return err
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	listener, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_NEW_LISTENER,
		uintptr(unsafe.Pointer(&fprog)))
	if e != 0 {
		return fmt.Errorf("seccomp: %v", e)
	}
	defer unix.Close(int(listener))
	defer unix.Close(notifyFD)
	if err := unix.Sendmsg(notifyFD, []byte{0}, unix.UnixRights(int(listener)), nil, 0); err != nil {
		return fmt.Errorf("send the seccomp channel: %w", err)
	}
	return nil
}

func apply(s Spec, notifyFD int) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := landlock(s); err != nil {
		return err
	}
	return seccomp(notifyFD)
}

// The kernel's seccomp notification structures (include/uapi/linux/seccomp.h).
type seccompData struct {
	Nr   int32
	Arch uint32
	IP   uint64
	Args [6]uint64
}

type seccompNotif struct {
	ID    uint64
	Pid   uint32
	Flags uint32
	Data  seccompData
}

type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// startTimeout bounds the launcher's way to its exec.
const startTimeout = 30 * time.Second

// start starts the launcher with one end of a socket pair as its descriptor 3, receives its seccomp channel, lets
// its exec of the program through, and closes the channel. A failure kills the launcher; nothing then runs.
func start(cmd *exec.Cmd) (err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	parent := fds[0]
	defer unix.Close(parent)
	child := os.NewFile(uintptr(fds[1]), "confine-channel")
	cmd.ExtraFiles = append([]*os.File{child}, cmd.ExtraFiles...)
	if err := cmd.Start(); err != nil {
		child.Close()
		return err
	}
	child.Close()
	defer func() {
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(startTimeout)
	if err := waitReadable(parent, deadline); err != nil {
		return fmt.Errorf("confine: the launcher sent no seccomp channel: %w", err)
	}
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(parent, make([]byte, 1), oob, 0)
	if err != nil {
		return fmt.Errorf("confine: receive the seccomp channel: %w", err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return fmt.Errorf("confine: the launcher exited before it sent its seccomp channel")
	}
	got, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(got) != 1 {
		return fmt.Errorf("confine: no seccomp channel in the launcher's message")
	}
	listener := got[0]
	// Closing the channel makes the kernel fail every later exec with ENOSYS.
	defer unix.Close(listener)
	if err := waitReadable(listener, deadline); err != nil {
		return fmt.Errorf("confine: the launcher did not reach its exec: %w", err)
	}
	var n seccompNotif
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), unix.SECCOMP_IOCTL_NOTIF_RECV, uintptr(unsafe.Pointer(&n))); e != 0 {
		return fmt.Errorf("confine: seccomp notification: %v", e)
	}
	resp := seccompNotifResp{ID: n.ID}
	ok := int(n.Pid) == cmd.Process.Pid && uint32(n.Data.Nr) == unix.SYS_EXECVE
	if ok {
		resp.Flags = unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE
	} else {
		resp.Error = -int32(unix.EPERM)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), unix.SECCOMP_IOCTL_NOTIF_SEND, uintptr(unsafe.Pointer(&resp))); e != 0 {
		return fmt.Errorf("confine: seccomp response: %v", e)
	}
	if !ok {
		return fmt.Errorf("confine: an unexpected first exec (pid %d, call %d)", n.Pid, n.Data.Nr)
	}
	return nil
}

// waitReadable waits until fd is readable or hung up, or the deadline passes.
func waitReadable(fd int, deadline time.Time) error {
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return errors.New("timed out")
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		return errors.New("the launcher exited")
	}
}
