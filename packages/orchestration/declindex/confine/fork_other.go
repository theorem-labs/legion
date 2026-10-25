//go:build linux && !amd64

package confine

// forkSyscalls are the architecture's own process-creating calls besides clone: arm64 has none.
var forkSyscalls []uint32
