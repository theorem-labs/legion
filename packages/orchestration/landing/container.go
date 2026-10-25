package landing

import (
	"context"
	"time"
)

// MountMode is how a named mount is mounted in a container.
type MountMode string

const (
	ReadWrite MountMode = "read-write"
	ReadOnly  MountMode = "read-only"
)

// Mount is a path of the build VM mounted in a container at the same path.
// HostPath may name a file or a directory.
type Mount struct {
	Name     string
	HostPath string
	Mode     MountMode
}

// Network is a container's network: NetworkDefault is docker's bridge, and
// NetworkNone (also the empty value) gives it no network at all.
type Network string

const (
	NetworkNone    Network = "none"
	NetworkDefault Network = "default"
)

// ContainerRun is one command run in a container of the pinned image on the
// build VM. The container gets exactly Env (plus the git safe.directory
// variables for SafeDirectory, and HOME and PATH when a mount is named elan),
// the Mounts, a fresh read-write /land/scratch and a read-only /land/inputs
// holding Inputs.
type ContainerRun struct {
	// Name is unique for each run; it names the container and the run's
	// directory under the run root.
	Name           string
	Image, User    string
	Argv           []string
	Env            map[string]string
	SafeDirectory  string
	Mounts         []Mount
	Network        Network
	Workdir        string
	Inputs         map[string][]byte
	Outputs        []string
	MaxOutputBytes int64
	Timeout        time.Duration
}

// ContainerResult is how a run ended. Exit is the container's exit status
// (137 after SIGKILL). An output that is absent, not a regular file, or a
// symbolic link is in Missing; one over MaxOutputBytes is in TooLarge. Stdout
// and Stderr keep at most MaxOutputBytes each.
type ContainerResult struct {
	Exit           int
	TimedOut       bool
	Stdout, Stderr []byte
	Outputs        map[string][]byte
	Missing        []string
	TooLarge       []string
}

// ContainerRunner runs a container on the build VM. An error means the run
// could not be started or its result could not be read (SSH or docker failed),
// never that the command failed.
type ContainerRunner interface {
	Run(context.Context, ContainerRun) (ContainerResult, error)
}

// LandedTrees gives a landed commit's detached worktree in the program's
// clone, the hooks' LAND_TREE.
type LandedTrees interface {
	LandedTree(ctx context.Context, commit string) (string, error)
	RemoveLandedTree(ctx context.Context, commit string) error
}
