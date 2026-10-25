package landing

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Docker runs every container of the program with the pinned image. With SSH
// set (an argv whose last word is the build VM's host) each docker command runs
// on the VM through it, and pushes reach the VM's clone over the same SSH
// command; with SSH empty they run on this host, which the tests use. On the VM
// the program runs only `docker run`.
type Docker struct {
	SSH   []string
	Image string
	User  string // uid:gid of the VM user that owns the mounted paths
	// GitDir is the program's bare clone on the VM, and RepositoryPath the
	// local repository that every pushed tree and commit comes from.
	GitDir         string
	RepositoryPath string
	// Repository is the ledger's repository name; its build directories are
	// under BuildRoot/.land-builds/<Repository with each / as _>/.
	Repository string
	// RunRoot holds each run's inputs and scratch directory, RunRoot/<name>.
	RunRoot string
	// BuildRoot is the host path of the container.build_root mount, which
	// holds the build directories and the package store. PackagesSource is
	// the host path of container.packages_source, empty for no store.
	BuildRoot      string
	PackagesSource string
}

// The paths of a container's own directories.
const (
	scratchPath = "/land/scratch"
	inputsPath  = "/land/inputs"
)

// cloneAttributes makes git archive and a checkout give each path its blob's
// bytes, whatever .gitattributes the tree holds.
const cloneAttributes = "* -text -eol -ident -filter -working-tree-encoding -export-ignore -export-subst"

var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// docker returns the docker command with args, on the VM when SSH is set.
func (d Docker) docker(ctx context.Context, args ...string) *exec.Cmd {
	argv := append([]string{"docker"}, args...)
	if len(d.SSH) == 0 {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	return exec.CommandContext(ctx, d.SSH[0], append(append([]string(nil), d.SSH[1:]...), shellJoin(argv))...)
}

// transportError is a failure of SSH (exit 255) or of docker itself (exit
// 125), so the command in the container never ran or its result is unknown.
type transportError struct {
	code   int
	stderr string
}

func (e transportError) Error() string {
	return fmt.Sprintf("docker on the build VM exited %d: %s", e.code, strings.TrimSpace(e.stderr))
}

func exitCode(err error) (int, bool) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return -1, false
}

// helper runs one of the program's own steps: a shell script in a container
// of the image with no network, as the configured user, with only mounts. It
// returns the script's stdout and exit status.
func (d Docker) helper(ctx context.Context, mounts []Mount, stdin io.Reader, script string, args ...string) ([]byte, int, error) {
	argv := []string{"run", "--rm", "--network=none", "--user", d.User}
	if stdin != nil {
		argv = append(argv, "-i")
	}
	for _, mount := range mounts {
		argument, err := mountArgument(mount)
		if err != nil {
			return nil, -1, err
		}
		argv = append(argv, "--mount", argument)
	}
	argv = append(argv, "--entrypoint", "/bin/sh", d.Image, "-c", script, "land-step")
	argv = append(argv, args...)
	command := d.docker(ctx, argv...)
	command.Stdin = stdin
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	if ctx.Err() != nil {
		return nil, -1, ctx.Err()
	}
	code, exited := exitCode(err)
	if !exited || code == 255 || code == 125 {
		return nil, code, transportError{code: code, stderr: stderr.String() + err.Error()}
	}
	return stdout.Bytes(), code, fmt.Errorf("step exited %d: %s", code, strings.TrimSpace(stderr.String()))
}

// mountArgument is docker's --mount value for a mount at its host path.
func mountArgument(mount Mount) (string, error) {
	if !path.IsAbs(mount.HostPath) || strings.ContainsAny(mount.HostPath, ",\n") {
		return "", fmt.Errorf("mount %q: host path %q must be absolute and hold no comma", mount.Name, mount.HostPath)
	}
	argument := "type=bind,src=" + mount.HostPath + ",dst=" + mount.HostPath
	switch mount.Mode {
	case ReadOnly:
		return argument + ",readonly", nil
	case ReadWrite:
		return argument, nil
	}
	return "", fmt.Errorf("mount %q: mode %q is not %q or %q", mount.Name, mount.Mode, ReadWrite, ReadOnly)
}

// Run runs one container. Its run directory RunRoot/<name> is made fresh with
// the inputs, mounted read-only at /land/inputs, and an empty scratch directory
// at /land/scratch; the outputs are read back from scratch and the directory is
// removed.
func (d Docker) Run(ctx context.Context, run ContainerRun) (ContainerResult, error) {
	name := containerName(run.Name)
	image, user := run.Image, run.User
	if image == "" {
		image = d.Image
	}
	if user == "" {
		user = d.User
	}
	if len(run.Argv) == 0 {
		return ContainerResult{}, fmt.Errorf("run %s has no command", name)
	}
	for _, output := range run.Outputs {
		if !plainName.MatchString(output) {
			return ContainerResult{}, fmt.Errorf("output %q is not a plain file name", output)
		}
	}
	inputs, err := inputArchive(run.Inputs)
	if err != nil {
		return ContainerResult{}, err
	}
	directory := path.Join(d.RunRoot, name)
	root := []Mount{{Name: "run-root", HostPath: d.RunRoot, Mode: ReadWrite}}
	if _, _, err := d.helper(ctx, root, bytes.NewReader(inputs),
		`set -eu; rm -rf "$1"; mkdir -p "$1/inputs" "$1/scratch"; tar -x -C "$1/inputs"; chmod -R a-w "$1/inputs"`, directory); err != nil {
		return ContainerResult{}, fmt.Errorf("write the inputs of %s: %w", name, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _, _ = d.helper(cleanup, root, nil, `chmod -R u+w "$1" 2>/dev/null; rm -rf "$1"`, directory)
	}()
	// A container of a run with this name that an earlier process left
	// behind is removed first, so the name is free.
	_ = d.docker(ctx, "rm", "-f", name).Run()

	argv := []string{"run", "--rm", "--name", name, "--user", user, "--network=" + dockerNetwork(run.Network),
		"--mount", "type=bind,src=" + directory + "/scratch,dst=" + scratchPath,
		"--mount", "type=bind,src=" + directory + "/inputs,dst=" + inputsPath + ",readonly"}
	mounts := slices.Clone(run.Mounts)
	// A mount inside another (a read-write out/ under a read-only root) must
	// come after it, so docker mounts the parent first.
	slices.SortStableFunc(mounts, func(a, b Mount) int { return len(a.HostPath) - len(b.HostPath) })
	for _, mount := range mounts {
		argument, err := mountArgument(mount)
		if err != nil {
			return ContainerResult{}, err
		}
		argv = append(argv, "--mount", argument)
	}
	environment, err := d.environment(ctx, run, image)
	if err != nil {
		return ContainerResult{}, err
	}
	for _, variable := range environment {
		argv = append(argv, "--env", variable)
	}
	if run.Workdir != "" {
		argv = append(argv, "--workdir", run.Workdir)
	}
	argv = append(argv, "--entrypoint", run.Argv[0], image)
	argv = append(argv, run.Argv[1:]...)

	limit := run.MaxOutputBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if run.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, run.Timeout)
	}
	defer cancel()
	command := d.docker(runCtx, argv...)
	stdout, stderr := &cappedBuffer{limit: limit}, &cappedBuffer{limit: limit}
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	result := ContainerResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runCtx.Err() != nil {
		// The container goes on when the docker client or its SSH dies,
		// so it is removed by name.
		removal, cancelRemoval := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		_ = d.docker(removal, "rm", "-f", name).Run()
		cancelRemoval()
		if ctx.Err() != nil {
			return ContainerResult{}, ctx.Err()
		}
		result.TimedOut, result.Exit = true, -1
		return result, nil
	}
	if runErr != nil {
		code, exited := exitCode(runErr)
		if !exited || code == 255 || code == 125 {
			return ContainerResult{}, transportError{code: code, stderr: string(stderr.Bytes())}
		}
		result.Exit = code
	}
	if err := d.readOutputs(ctx, root, directory, run.Outputs, limit, &result); err != nil {
		return ContainerResult{}, fmt.Errorf("read the outputs of %s: %w", name, err)
	}
	return result, nil
}

// dockerNetwork maps the configured network to docker's: "host" is the VM's
// own network stack, and anything else is no network.
func dockerNetwork(network Network) string {
	if network == NetworkHost {
		return "host"
	}
	return "none"
}

// environment is the run's whole environment: its own variables, the git
// safe.directory for SafeDirectory, and HOME and PATH for a run that mounts
// elan (HOME is that mount's parent, and its bin directory comes first on the
// image's PATH).
func (d Docker) environment(ctx context.Context, run ContainerRun, image string) ([]string, error) {
	variables := make(map[string]string, len(run.Env)+5)
	for key, value := range run.Env {
		variables[key] = value
	}
	if run.SafeDirectory != "" {
		variables["GIT_CONFIG_COUNT"] = "1"
		variables["GIT_CONFIG_KEY_0"] = "safe.directory"
		variables["GIT_CONFIG_VALUE_0"] = run.SafeDirectory
	}
	for _, mount := range run.Mounts {
		if mount.Name != "elan" {
			continue
		}
		imagePath, err := d.imagePath(ctx, image)
		if err != nil {
			return nil, err
		}
		variables["HOME"] = path.Dir(mount.HostPath)
		variables["PATH"] = path.Join(mount.HostPath, "bin") + ":" + imagePath
	}
	keys := make([]string, 0, len(variables))
	for key := range variables {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+variables[key])
	}
	return environment, nil
}

// imagePath is the PATH that the image itself sets.
func (d Docker) imagePath(ctx context.Context, image string) (string, error) {
	output, err := d.docker(ctx, "image", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", image).Output()
	if err != nil {
		return "", fmt.Errorf("read the PATH of image %s: %w", image, err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if value, found := strings.CutPrefix(line, "PATH="); found {
			return value, nil
		}
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", nil
}

// readOutputs reads each declared output that is a regular file, not a
// symbolic link, of at most limit bytes; the others go in Missing or TooLarge.
func (d Docker) readOutputs(ctx context.Context, root []Mount, directory string, outputs []string, limit int64, result *ContainerResult) error {
	result.Outputs = map[string][]byte{}
	if len(outputs) == 0 {
		return nil
	}
	script := `set -eu
dir=$1 max=$2; shift 2
cd "$dir/scratch"
: > "$dir/report"; : > "$dir/ok"
for n do
  if [ -L "$n" ] || [ ! -f "$n" ]; then echo "missing $n" >> "$dir/report"
  elif [ "$(stat -c %s -- "$n")" -gt "$max" ]; then echo "large $n" >> "$dir/report"
  else echo "$n" >> "$dir/ok"; fi
done
tar -c -f - -C "$dir" report -C "$dir/scratch" --no-recursion -T "$dir/ok"`
	args := append([]string{directory, strconv.FormatInt(limit, 10)}, outputs...)
	archive, _, err := d.helper(ctx, root, nil, script, args...)
	if err != nil {
		return err
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the outputs archive: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return err
		}
		if header.Name == "report" {
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				if name, found := strings.CutPrefix(line, "missing "); found {
					result.Missing = append(result.Missing, name)
				} else if name, found := strings.CutPrefix(line, "large "); found {
					result.TooLarge = append(result.TooLarge, name)
				}
			}
			continue
		}
		if int64(len(body)) > limit {
			result.TooLarge = append(result.TooLarge, header.Name)
			continue
		}
		result.Outputs[header.Name] = body
	}
	return nil
}

// inputArchive is a tar of the inputs, each a plain file name.
func inputArchive(inputs map[string][]byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !plainName.MatchString(name) {
			return nil, fmt.Errorf("input %q is not a plain file name", name)
		}
		body := inputs[name]
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := writer.Write(body); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

var unsafeNameCharacters = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// containerName is the run's name with every other character as "-", after
// "land-".
func containerName(name string) string {
	return "land-" + strings.Trim(unsafeNameCharacters.ReplaceAllString(name, "-"), "-.")
}

// uniqueName is prefix followed by a random suffix.
func uniqueName(prefix string) string {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	return prefix + "-" + hex.EncodeToString(suffix[:])
}

// cappedBuffer keeps the first limit bytes written to it.
type cappedBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - int64(b.buffer.Len()); room > 0 {
		if int64(len(p)) > room {
			b.buffer.Write(p[:room])
		} else {
			b.buffer.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return bytes.Clone(b.buffer.Bytes()) }

// shellJoin quotes each word for a POSIX shell.
func shellJoin(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// cloneMount is the program's clone, mounted in mode.
func (d Docker) cloneMount(mode MountMode) Mount {
	return Mount{Name: "git_dir", HostPath: d.GitDir, Mode: mode}
}

// InitClone makes the program's clone, a bare repository, in its existing
// directory, and writes its info/attributes. A second call changes nothing.
func (d Docker) InitClone(ctx context.Context) error {
	_, _, err := d.helper(ctx, []Mount{d.cloneMount(ReadWrite)}, nil,
		`set -eu; git init --bare -q "$1"; printf '%s\n' "$2" > "$1/info/attributes"`, d.GitDir, cloneAttributes)
	if err != nil {
		return fmt.Errorf("make the program's clone %s: %w", d.GitDir, err)
	}
	return nil
}

// Push pushes each object (a commit or a tree) from the local repository to
// refs/land/<object> in the program's clone. The receiving git runs in a
// container that mounts only the clone. A second push changes nothing.
func (d Docker) Push(ctx context.Context, objects ...string) error {
	if len(objects) == 0 {
		return nil
	}
	receive := shellJoin([]string{"docker", "run", "--rm", "-i", "--network=none", "--user", d.User,
		"--mount", "type=bind,src=" + d.GitDir + ",dst=" + d.GitDir, "--entrypoint", "git", d.Image, "receive-pack"})
	url := d.GitDir
	environment := os.Environ()
	if len(d.SSH) != 0 {
		url = d.SSH[len(d.SSH)-1] + ":" + d.GitDir
		environment = append(environment, "GIT_SSH_COMMAND="+shellJoin(d.SSH[:len(d.SSH)-1]), "GIT_SSH_VARIANT=ssh")
	}
	args := []string{"-C", d.RepositoryPath, "push", "--quiet", "--receive-pack=" + receive, url}
	for _, object := range objects {
		args = append(args, object+":refs/land/"+object)
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("push %s to the program's clone: %w: %s", strings.Join(objects, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// at is this executor on the clone gitDir, the one a batch was admitted with;
// an empty gitDir keeps the configured one.
func (d Docker) at(gitDir string) Docker {
	if gitDir != "" {
		d.GitDir = gitDir
	}
	return d
}

// LandedTree pushes the landed commit to the clone gitDir and adds its
// detached worktree <gitDir>/trees/<commit> when it is absent; the hooks'
// LAND_TREE, in the clone their containers mount.
func (d Docker) LandedTree(ctx context.Context, gitDir, commit string) (string, error) {
	d = d.at(gitDir)
	if !fullCommit.MatchString(commit) {
		return "", fmt.Errorf("landed commit %q is not 40 hex characters", commit)
	}
	if err := d.Push(ctx, commit); err != nil {
		return "", err
	}
	tree := path.Join(d.GitDir, "trees", commit)
	if _, _, err := d.helper(ctx, []Mount{d.cloneMount(ReadWrite)}, nil,
		`set -eu; [ -e "$2/.git" ] || git --git-dir="$1" worktree add -q --detach "$2" "$3"`, d.GitDir, tree, commit); err != nil {
		return "", fmt.Errorf("add the landed tree of %s: %w", commit, err)
	}
	return tree, nil
}

// RemoveLandedTree removes the landed commit's worktree from the clone gitDir.
func (d Docker) RemoveLandedTree(ctx context.Context, gitDir, commit string) error {
	d = d.at(gitDir)
	if !fullCommit.MatchString(commit) {
		return fmt.Errorf("landed commit %q is not 40 hex characters", commit)
	}
	if _, _, err := d.helper(ctx, []Mount{d.cloneMount(ReadWrite)}, nil,
		`set -eu; rm -rf "$1/trees/$2"; git --git-dir="$1" worktree prune`, d.GitDir, commit); err != nil {
		return fmt.Errorf("remove the landed tree of %s: %w", commit, err)
	}
	return nil
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CheckMounts refuses a mount whose host path does not exist on the VM. A path
// may be a file or a directory.
func (d Docker) CheckMounts(ctx context.Context, mounts []Mount) error {
	for _, mount := range mounts {
		probe := Mount{Name: mount.Name, HostPath: mount.HostPath, Mode: ReadOnly}
		if _, _, err := d.helper(ctx, []Mount{probe}, nil, `test -e "$1"`, mount.HostPath); err != nil {
			return fmt.Errorf("mount %q: host path %s does not exist on the build VM: %w", mount.Name, mount.HostPath, err)
		}
	}
	return nil
}

// buildsDirectory is where this repository's build directories and package
// copies are made.
func (d Docker) buildsDirectory() string {
	return path.Join(d.BuildRoot, ".land-builds", strings.ReplaceAll(d.Repository, "/", "_"))
}

// Start prepares the VM before the first command: it refuses a configuration
// whose mount, clone, run root or build root does not exist, and makes the
// clone. With clean set (land serve) it then removes every landed worktree and
// every build directory that an earlier process left behind.
func (d Docker) Start(ctx context.Context, mounts []Mount, clean bool) error {
	required := append([]Mount{
		{Name: "repository.git_dir", HostPath: d.GitDir},
		{Name: "container.run_root", HostPath: d.RunRoot},
	}, mounts...)
	if err := d.CheckMounts(ctx, required); err != nil {
		return err
	}
	if err := d.InitClone(ctx); err != nil {
		return err
	}
	if !clean {
		return nil
	}
	if _, _, err := d.helper(ctx, []Mount{d.cloneMount(ReadWrite)}, nil,
		`set -eu; rm -rf "$1/trees"; git --git-dir="$1" worktree prune`, d.GitDir); err != nil {
		return fmt.Errorf("remove the landed trees: %w", err)
	}
	if d.BuildRoot != "" {
		if _, _, err := d.helper(ctx, []Mount{{Name: "build_root", HostPath: d.BuildRoot, Mode: ReadWrite}}, nil,
			`rm -rf "$1"`, d.buildsDirectory()); err != nil {
			return fmt.Errorf("remove the build directories: %w", err)
		}
	}
	return nil
}
