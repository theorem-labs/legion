//go:build linux

package confine

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// systemReads are what a dynamically linked program needs to start: the shared libraries and the loader's cache,
// and the null and random devices. None holds anything of the user's.
var systemReads = []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64", "/usr/local/lib", "/etc/ld.so.cache",
	"/dev/urandom"}

const (
	fsV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	// The rights that apply to a file (the others apply to directories only).
	fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	readRights  = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	execRights  = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_EXECUTE
	udpNet      = unix.LANDLOCK_ACCESS_NET_BIND_UDP | unix.LANDLOCK_ACCESS_NET_CONNECT_SEND_UDP
	tcpNet      = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	allScopes   = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	seccompArgs = 16 // offset of args[0] in struct seccomp_data
)

func landlockABI() (int, error) {
	abi, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, fmt.Errorf("%w: Landlock: %v", ErrUnavailable, e)
	}
	return int(abi), nil
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
	fs := uint64(fsV1)
	if abi >= 2 {
		fs |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		fs |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
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
	writeRights := handled &^ unix.LANDLOCK_ACCESS_FS_EXECUTE
	for _, step := range []struct {
		paths    []string
		access   uint64
		optional bool
	}{
		{systemReads, readRights, true},
		{[]string{"/dev/null"}, unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, true},
		{s.Read, readRights, false},
		{s.Write, writeRights, false},
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
// namespaces and mounts.
var deniedSyscalls = []uint32{unix.SYS_SOCKET, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER,
	unix.SYS_IO_URING_REGISTER, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_MOUNT,
	unix.SYS_BPF}

func seccomp() error {
	arch, err := auditArch()
	if err != nil {
		return err
	}
	const (
		ldAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		ret   = unix.BPF_RET | unix.BPF_K
	)
	type jump struct {
		at     int
		target string // allow, deny or kill, for the true branch (tt) or false branch (tf)
		onTrue bool
	}
	var prog []unix.SockFilter
	var jumps []jump
	prog = append(prog, unix.SockFilter{Code: ldAbs, K: 4}) // arch
	jumps = append(jumps, jump{len(prog), "kill", false})
	prog = append(prog, unix.SockFilter{Code: jeq, K: arch})
	prog = append(prog, unix.SockFilter{Code: ldAbs, K: 0}) // nr
	if arch == unix.AUDIT_ARCH_X86_64 {
		jumps = append(jumps, jump{len(prog), "kill", true}) // the x32 system calls
		prog = append(prog, unix.SockFilter{Code: jge, K: 0x40000000})
	}
	for _, nr := range deniedSyscalls {
		jumps = append(jumps, jump{len(prog), "deny", true})
		prog = append(prog, unix.SockFilter{Code: jeq, K: nr})
	}
	at := map[string]int{"allow": len(prog), "deny": len(prog) + 1, "kill": len(prog) + 2}
	prog = append(prog,
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS})
	for _, j := range jumps {
		off := at[j.target] - (j.at + 1)
		if off > 255 {
			return errors.New("seccomp filter too long")
		}
		if j.onTrue {
			prog[j.at].Jt = uint8(off)
		} else {
			prog[j.at].Jf = uint8(off)
		}
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		return fmt.Errorf("seccomp: %v", e)
	}
	return nil
}

func apply(s Spec) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := landlock(s); err != nil {
		return err
	}
	return seccomp()
}
