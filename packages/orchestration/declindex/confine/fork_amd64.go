//go:build linux

package confine

import "golang.org/x/sys/unix"

// forkSyscalls are the architecture's own process-creating calls besides clone.
var forkSyscalls = []uint32{unix.SYS_FORK, unix.SYS_VFORK}
