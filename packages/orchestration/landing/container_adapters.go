package landing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// PackagesChangedError says the package store has no entry for a build
// tree's lake-manifest.json and the packages source holds another manifest,
// so the build cannot get its packages without a network (the package step's
// exit 3). Nothing was built.
type PackagesChangedError struct{ Hash string }

func (e PackagesChangedError) Error() string {
	return "packages_changed: the package store has no entry for lake-manifest.json " + e.Hash
}

// gitPackage is one git package of a lake-manifest.json.
type gitPackage struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Rev    string `json:"rev"`
	SubDir string `json:"subDir"`
}

// packageStep fills the store entry from the packages source when it is
// absent. It exits 0 when the entry exists, 3 when the source's manifest is
// another, and 1 on any other failure; a fill copies into a new directory and
// renames it, so no failure or second fill leaves a partial entry.
const packageStep = `set -eu
store=$1 hash=$2 src=$3 tmp=$4; shift 4
entry=$store/$hash
[ -d "$entry" ] && exit 0
[ "$(sha256sum < "$src/lake-manifest.json" | cut -d' ' -f1)" = "$hash" ] || exit 3
mkdir -p "$store" "$tmp"
copy=$(mktemp -d "$tmp/packages.XXXXXX")
trap 'rm -rf "$copy"' EXIT
while [ $# -gt 0 ]; do
  name=$1 rev=$2 sub=$3; shift 3
  mkdir "$copy/$name"
  if [ -n "$sub" ]; then
    (cd "$src/.lake/packages/$name" && tar -c -f - --anchored --exclude=./.lake --exclude="./$sub/.lake" .) | tar -x -f - -C "$copy/$name"
  else
    (cd "$src/.lake/packages/$name" && tar -c -f - --anchored --exclude=./.lake .) | tar -x -f - -C "$copy/$name"
  fi
  head=$(git -C "$copy/$name" rev-parse HEAD)
  [ "$head" = "$rev" ] || { echo "package $name is at $head, not the manifest's $rev" >&2; exit 1; }
  [ -z "$(git -C "$copy/$name" status --porcelain --untracked-files=all)" ] || { echo "package $name has changes" >&2; exit 1; }
done
mv -T "$copy" "$entry" 2>/dev/null || [ -d "$entry" ] || exit 1`

// buildDirectoryStep makes a build directory: the tree from the clone, with
// every path's blob bytes, and .lake/packages hard-linked from the entry. The
// archive goes through a file, so a failed git archive fails the step.
const buildDirectoryStep = `set -eu
mkdir -p "$(dirname "$1")"
mkdir "$1"
git --git-dir="$2" archive -o "$1.tar" "$3"
tar -x -f "$1.tar" -C "$1"
rm -f "$1.tar"
if [ -n "$4" ]; then mkdir -p "$1/.lake"; cp -al "$4" "$1/.lake/packages"; fi`

// Packages runs the package step for tree's lake-manifest.json and returns
// the store entry, or "" when the tree has no manifest or no git package.
func (d Docker) Packages(ctx context.Context, tree Tree) (string, error) {
	manifest, err := exec.CommandContext(ctx, "git", "-C", d.RepositoryPath, "show", string(tree)+":lake-manifest.json").Output()
	if err != nil {
		return "", nil
	}
	var wire struct {
		Packages []gitPackage `json:"packages"`
	}
	if err := json.Unmarshal(manifest, &wire); err != nil {
		return "", fmt.Errorf("read lake-manifest.json of %s: %w", tree, err)
	}
	sum := sha256.Sum256(manifest)
	hash := hex.EncodeToString(sum[:])
	args := []string{path.Join(d.BuildRoot, ".land-packages"), hash, d.PackagesSource, d.buildsDirectory()}
	for _, pkg := range wire.Packages {
		if pkg.Type != "git" {
			continue
		}
		if !plainName.MatchString(pkg.Name) || !fullCommit.MatchString(pkg.Rev) || strings.Contains(pkg.SubDir, "..") {
			return "", fmt.Errorf("lake-manifest.json of %s: package %q is not a plain name at a full commit", tree, pkg.Name)
		}
		args = append(args, pkg.Name, pkg.Rev, pkg.SubDir)
	}
	if len(args) == 4 {
		return "", nil
	}
	if d.PackagesSource == "" {
		return "", fmt.Errorf("the tree has git packages and container.packages_source is not set")
	}
	mounts := []Mount{
		{Name: "build_root", HostPath: d.BuildRoot, Mode: ReadWrite},
		{Name: "packages_source", HostPath: d.PackagesSource, Mode: ReadOnly},
	}
	_, code, err := d.helper(ctx, mounts, nil, packageStep, args...)
	switch {
	case err == nil:
		return path.Join(d.BuildRoot, ".land-packages", hash), nil
	case code == 3:
		return "", PackagesChangedError{Hash: hash}
	}
	return "", fmt.Errorf("package step: %w", err)
}

// ContainerBuilder builds a tree in a container: in a directory of its own
// under the build root, which holds the tree from the program's clone and the
// packages linked from the store. The build gets Mounts (container.build_mounts,
// which include the build root's mount) and no network.
type ContainerBuilder struct {
	Docker  Docker
	Command []string
	Mounts  []Mount
}

// Build builds a tree and removes its directory when it returns. A package
// step exit 3 is an infrastructure failure of class packages_changed: a blame
// or confirmation build has no member to return for it.
func (b ContainerBuilder) Build(ctx context.Context, tree Tree, modules []string) (blame.BuildResult, error) {
	build, err := b.build(ctx, tree, modules)
	build.Release()
	var changed PackagesChangedError
	if errors.As(err, &changed) {
		return blame.BuildResult{InfraClass: "packages_changed", Output: changed.Error()}, nil
	}
	return build.Result, err
}

// BuildKept builds the candidate and keeps its directory until Release. A
// package step exit 3 returns PackagesChangedError.
func (b ContainerBuilder) BuildKept(ctx context.Context, tree Tree) (KeptBuild, error) {
	return b.build(ctx, tree, nil)
}

func (b ContainerBuilder) build(ctx context.Context, tree Tree, modules []string) (KeptBuild, error) {
	kept := KeptBuild{Release: noRelease}
	if len(b.Command) == 0 {
		return kept, fmt.Errorf("build command is empty")
	}
	d := b.Docker
	if err := d.Push(ctx, string(tree)); err != nil {
		return kept, err
	}
	entry, err := d.Packages(ctx, tree)
	if err != nil {
		return kept, err
	}
	directory := path.Join(d.buildsDirectory(), uniqueName("build"))
	root := []Mount{{Name: "build_root", HostPath: d.BuildRoot, Mode: ReadWrite}}
	var once sync.Once
	release := func() {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_, _, _ = d.helper(cleanup, root, nil, `rm -rf "$1"`, directory)
		})
	}
	if _, _, err := d.helper(ctx, append(root, d.cloneMount(ReadOnly)), nil, buildDirectoryStep, directory, d.GitDir, string(tree), entry); err != nil {
		release()
		return kept, fmt.Errorf("make the build directory: %w", err)
	}
	kept.Directory, kept.Release = directory, release
	result, err := d.Run(ctx, ContainerRun{
		Name:    uniqueName("build"),
		Argv:    b.Command,
		Env:     map[string]string{"LAND_TREE": string(tree), "LAND_MODULES": strings.Join(modules, " "), "LAND_RESULT_FILE": scratchPath + "/result.json"},
		Mounts:  b.Mounts,
		Network: NetworkNone,
		Workdir: directory,
		Outputs: []string{"result.json"},
		// The build's log is its verdict's evidence, so it keeps more
		// than a hook's output.
		MaxOutputBytes: 64 << 20,
	})
	if err != nil {
		return kept, err
	}
	kept.Result = buildVerdict(result)
	return kept, nil
}

// buildVerdict reads a build's result: the verdict file when the command wrote
// one ({"green", "absent_targets"}), else its exit status and the
// LAND_TARGET_ABSENT marker. 137 and any other exit above 128 are an
// infrastructure failure.
func buildVerdict(result ContainerResult) blame.BuildResult {
	output := string(result.Stdout) + string(result.Stderr)
	verdict := blame.BuildResult{Green: result.Exit == 0, Output: output}
	if body, found := result.Outputs["result.json"]; found {
		var wire struct {
			Green         *bool    `json:"green"`
			AbsentTargets []string `json:"absent_targets"`
		}
		if err := json.Unmarshal(body, &wire); err == nil && wire.Green != nil {
			verdict.Green = *wire.Green && result.Exit == 0
			verdict.Absent = !verdict.Green && len(wire.AbsentTargets) != 0
			return verdict
		}
	}
	switch {
	case result.Exit == 0:
	case strings.Contains(output, "LAND_TARGET_ABSENT"):
		verdict.Absent = true
	case result.Exit == 137:
		verdict.InfraClass = "resource-kill"
	case result.Exit > 128:
		verdict.InfraClass = "signal"
	case result.Exit == 126 || result.Exit == 127:
		verdict.InfraClass = "launch"
	}
	return verdict
}

// ContainerHasher runs the statement hasher in a container, with the program's
// clone read-only as its working directory and LAND_GIT_DIR, the names in
// /land/inputs/names.json, and Mounts (container.hasher_mounts) on Network.
type ContainerHasher struct {
	Docker  Docker
	Command []string
	Mounts  []Mount
	Network Network
}

func (h ContainerHasher) Hashes(ctx context.Context, input HashInput) (map[string]StatementHash, error) {
	if len(h.Command) == 0 {
		return nil, fmt.Errorf("hasher command is empty")
	}
	namesJSON, err := json.Marshal(input.Names)
	if err != nil {
		return nil, fmt.Errorf("encode declaration names: %w", err)
	}
	d := h.Docker
	if err := d.Push(ctx, string(input.Tree)); err != nil {
		return nil, err
	}
	environment := map[string]string{
		"LAND_TREE":       string(input.Tree),
		"LAND_GIT_DIR":    d.GitDir,
		"LAND_BUILD_DIR":  input.BuildDirectory,
		"LAND_NAMES_FILE": inputsPath + "/names.json",
	}
	if len(namesJSON) <= maxNamesEnvironment {
		environment["LAND_NAMES"] = string(namesJSON)
	}
	result, err := d.Run(ctx, ContainerRun{
		Name:           uniqueName("hash"),
		Argv:           h.Command,
		Env:            environment,
		SafeDirectory:  d.GitDir,
		Mounts:         append([]Mount{d.cloneMount(ReadOnly)}, h.Mounts...),
		Network:        h.Network,
		Workdir:        d.GitDir,
		Inputs:         map[string][]byte{"names.json": namesJSON},
		MaxOutputBytes: 64 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("run statement hasher: %w", err)
	}
	stderr := strings.TrimSpace(string(result.Stderr))
	switch result.Exit {
	case 0:
		return parseStatementHashes(string(result.Stdout), input.Names)
	case 3:
		return nil, HashUnableError{Output: stderr}
	case 75:
		return nil, HashRetryError{Output: stderr}
	}
	return nil, fmt.Errorf("run statement hasher: exit %d: %s", result.Exit, stderr)
}

// ContainerChecker runs one configured check in a container: the program's
// clone read-only as its working directory, the candidate tree in LAND_TREE
// and the base commit in LAND_BASE, Mounts (container.check_mounts) and
// Network (container.check_network, none when empty).
type ContainerChecker struct {
	Docker    Docker
	CheckName string
	Command   []string
	Format    string
	Mounts    []Mount
	Network   Network
}

func (c ContainerChecker) Name() string { return c.CheckName }

func (c ContainerChecker) Check(ctx context.Context, in CheckInput) ([]Finding, error) {
	if len(c.Command) == 0 {
		return nil, fmt.Errorf("check %q has no command", c.CheckName)
	}
	d := c.Docker
	if err := d.Push(ctx, string(in.Tree), in.Base); err != nil {
		return nil, err
	}
	result, err := d.Run(ctx, ContainerRun{
		Name:           uniqueName("check"),
		Argv:           c.Command,
		Env:            map[string]string{"LAND_TREE": string(in.Tree), "LAND_BASE": in.Base, "LAND_GIT_DIR": d.GitDir},
		SafeDirectory:  d.GitDir,
		Mounts:         append([]Mount{d.cloneMount(ReadOnly)}, c.Mounts...),
		Network:        c.Network,
		Workdir:        d.GitDir,
		MaxOutputBytes: 64 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("run check %q: %w", c.CheckName, err)
	}
	return checkOutcome(c.CheckName, c.Format, result.Exit, string(result.Stdout), string(bytes.TrimSpace(result.Stderr)))
}
