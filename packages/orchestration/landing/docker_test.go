package landing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testdocker"
)

// dockerFixture is a repository, the program's clone, a run root and a build root, all on this host, with a
// Docker that runs its containers here.
type dockerFixture struct {
	docker Docker
	repo   string
	commit string
	tree   string
}

func newDockerFixture(t *testing.T, files map[string]string) dockerFixture {
	t.Helper()
	user := testdocker.Require(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "config", "user.email", "test@example.invalid")
	gitIn(t, repo, "config", "user.name", "Test")
	if files == nil {
		files = map[string]string{"Alpha.lean": "def alpha := 0\n"}
	}
	for name, body := range files {
		writeFixture(t, filepath.Join(repo, name), body)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "base")
	d := Docker{
		Image:          testdocker.Image,
		User:           user,
		GitDir:         filepath.Join(root, "clone.git"),
		RepositoryPath: repo,
		Repository:     "owner/repo",
		RunRoot:        filepath.Join(root, "runs"),
		BuildRoot:      filepath.Join(root, "lakecache"),
	}
	for _, dir := range []string{d.GitDir, d.RunRoot, d.BuildRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Start(context.Background(), nil, true); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return dockerFixture{docker: d, repo: repo, commit: gitIn(t, repo, "rev-parse", "HEAD"), tree: gitIn(t, repo, "rev-parse", "HEAD^{tree}")}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// shell is a run of a shell script in the fixture's image.
func shell(name, script string) ContainerRun {
	return ContainerRun{Name: name, Argv: []string{"/bin/sh", "-c", script}, MaxOutputBytes: 1 << 20, Timeout: 2 * time.Minute}
}

// A run gets its inputs read-only at /land/inputs and its declared outputs back from /land/scratch; an output that
// is absent or a symbolic link is missing, and one over the cap is too large.
func TestDockerRunPassesInputsAndReadsOutputs(t *testing.T) {
	f := newDockerFixture(t, nil)
	run := shell("io", `cat /land/inputs/in.json > /land/scratch/out.json
ln -s /etc/passwd /land/scratch/link
head -c 2048 /dev/zero > /land/scratch/big
touch /land/inputs/x 2>/dev/null && echo inputs-writable
echo done`)
	run.Inputs = map[string][]byte{"in.json": []byte(`{"a":1}`)}
	run.Outputs = []string{"out.json", "link", "big", "absent"}
	run.MaxOutputBytes = 1024
	result, err := f.docker.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Exit != 0 || strings.Contains(string(result.Stdout), "inputs-writable") || !strings.Contains(string(result.Stdout), "done") {
		t.Fatalf("run = exit %d, stdout %q, stderr %q", result.Exit, result.Stdout, result.Stderr)
	}
	if string(result.Outputs["out.json"]) != `{"a":1}` {
		t.Fatalf("outputs = %q", result.Outputs)
	}
	if strings.Join(result.Missing, ",") != "link,absent" || strings.Join(result.TooLarge, ",") != "big" {
		t.Fatalf("missing %q, too large %q", result.Missing, result.TooLarge)
	}
	if entries, _ := os.ReadDir(f.docker.RunRoot); len(entries) != 0 {
		t.Fatalf("the run root still holds %d entries", len(entries))
	}
}

// A run that outlives its timeout is removed and reported as timed out.
func TestDockerRunTimesOut(t *testing.T) {
	f := newDockerFixture(t, nil)
	run := shell("slow", "sleep 60")
	run.Timeout = 3 * time.Second
	result, err := f.docker.Run(context.Background(), run)
	if err != nil || !result.TimedOut {
		t.Fatalf("Run() = %+v, %v; want timed out", result, err)
	}
	if output, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+containerName("slow")).Output(); len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("the container is still there: %s", output)
	}
}

// A hook with network "default" reaches a fixture server on docker's bridge; the same hook with no network (the
// value when the key is absent) cannot.
func TestDockerNetworkHostReachesAServerAndNoneCannot(t *testing.T) {
	f := newDockerFixture(t, nil)
	url, _ := fixtureServer(t)
	script := `for i in 1 2 3 4 5 6 7 8 9 10; do curl -fsS -m 3 -o /dev/null "$1" && exit 0; sleep 1; done; exit 7`
	for _, test := range []struct {
		network Network
		want    int
	}{{NetworkHost, 0}, {NetworkNone, 7}, {"", 7}} {
		run := ContainerRun{Name: "net-" + string(test.network), Argv: []string{"/bin/sh", "-c", script, "probe", url}, Network: test.network, Timeout: time.Minute}
		result, err := f.docker.Run(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if result.Exit != test.want {
			t.Fatalf("network %q: exit %d, want %d (stderr %q)", test.network, result.Exit, test.want, result.Stderr)
		}
	}
}

// The negative controls. A container sees no $HOME of the host, no SSH agent, no GH_* or *_DSN variable and no
// network; a mount it does not name is not there; a read-only mount cannot be written; and a write outside
// /land/scratch and its read-write mount reaches nothing the program reads.
func TestDockerContainerSeesOnlyWhatItIsGiven(t *testing.T) {
	f := newDockerFixture(t, nil)
	readOnly, writable, hidden := t.TempDir(), t.TempDir(), t.TempDir()
	writeFixture(t, filepath.Join(readOnly, "keep"), "keep\n")
	writeFixture(t, filepath.Join(hidden, "secret"), "secret\n")
	t.Setenv("GH_TOKEN", "leak")
	t.Setenv("LEDGER_DSN", "leak")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	home, _ := os.UserHomeDir()
	run := shell("negative", `fail=0
env | grep -E '^(GH_|SSH_AUTH_SOCK=)|_DSN=' && fail=1
[ -e "$1" ] && { echo "host home $1 visible"; fail=1; }
[ -e "$2/secret" ] && { echo "unnamed mount visible"; fail=1; }
echo x > "$3/keep" 2>/dev/null && { echo "read-only mount written"; fail=1; }
echo x > "$4/made" || { echo "read-write mount not writable"; fail=1; }
echo x > /outside 2>/dev/null
curl -fsS -m 3 -o /dev/null http://1.1.1.1/ 2>/dev/null && { echo "network reached"; fail=1; }
exit $fail`)
	run.Argv = append(run.Argv, "check", filepath.Join(home, ".ssh"), hidden, readOnly, writable)
	run.Mounts = []Mount{{Name: "ro", HostPath: readOnly, Mode: ReadOnly}, {Name: "rw", HostPath: writable, Mode: ReadWrite}}
	result, err := f.docker.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 {
		t.Fatalf("exit %d: %s%s", result.Exit, result.Stdout, result.Stderr)
	}
	if body, _ := os.ReadFile(filepath.Join(readOnly, "keep")); string(body) != "keep\n" {
		t.Fatalf("the read-only mount changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(writable, "made")); err != nil {
		t.Fatalf("the read-write mount was not written: %v", err)
	}
}

// A mount inside a read-only mount is writable when it is named read-write, whatever order the run lists them in:
// the graph hook's root and config/ are read-only, and only the directories it writes are writable.
func TestDockerMountsAWritableChildInsideAReadOnlyRoot(t *testing.T) {
	f := newDockerFixture(t, nil)
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "config", "inputs.sha256"), "pinned\n")
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := shell("graph", `echo x > "$1/out/graph.json" || exit 1
echo x > "$1/config/inputs.sha256" 2>/dev/null && exit 2
echo x > "$1/new" 2>/dev/null && exit 3
exit 0`)
	run.Argv = append(run.Argv, "graph", root)
	run.Mounts = []Mount{{Name: "graph-out", HostPath: filepath.Join(root, "out"), Mode: ReadWrite}, {Name: "graph", HostPath: root, Mode: ReadOnly}}
	result, err := f.docker.Run(context.Background(), run)
	if err != nil || result.Exit != 0 {
		t.Fatalf("Run() = exit %d, %v: %s", result.Exit, err, result.Stderr)
	}
}

// A container that mounts elan gets HOME as the mount's parent and the mount's bin first on the image's PATH; the
// safe.directory variables name the tree; nothing else is set.
func TestDockerEnvironmentIsTheExplicitList(t *testing.T) {
	f := newDockerFixture(t, nil)
	elan := filepath.Join(t.TempDir(), "home", ".elan")
	if err := os.MkdirAll(filepath.Join(elan, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := shell("env", "env | sort")
	run.Env = map[string]string{"LAND_COMMIT": "c"}
	run.SafeDirectory = "/srv/tree"
	run.Mounts = []Mount{{Name: "elan", HostPath: elan, Mode: ReadOnly}}
	result, err := f.docker.Run(context.Background(), run)
	if err != nil || result.Exit != 0 {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		got[key] = value
	}
	want := map[string]string{
		"LAND_COMMIT": "c", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "/srv/tree",
		"HOME": filepath.Dir(elan),
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %q, want %q (environment %v)", key, got[key], value, got)
		}
	}
	if !strings.HasPrefix(got["PATH"], filepath.Join(elan, "bin")+":") {
		t.Fatalf("PATH = %q, want the elan bin first", got["PATH"])
	}
	for key := range got {
		if _, expected := want[key]; !expected && key != "PATH" && key != "HOSTNAME" && key != "PWD" && key != "SHLVL" && key != "_" {
			t.Fatalf("unexpected variable %s=%q", key, got[key])
		}
	}
}

// A push repeated after a crash changes nothing, and a tree can be pushed as a ref.
func TestDockerPushIsIdempotent(t *testing.T) {
	f := newDockerFixture(t, nil)
	ctx := context.Background()
	for range 2 {
		if err := f.docker.Push(ctx, f.commit, f.tree); err != nil {
			t.Fatalf("Push() error = %v", err)
		}
	}
	refs := gitIn(t, f.docker.GitDir, "for-each-ref", "--format=%(objectname) %(refname)")
	if refs != f.commit+" refs/land/"+f.commit+"\n"+f.tree+" refs/land/"+f.tree && refs != f.tree+" refs/land/"+f.tree+"\n"+f.commit+" refs/land/"+f.commit {
		t.Fatalf("refs = %q", refs)
	}
}

// A hook finds LAND_TREE clean at LAND_COMMIT and cannot write the clone, which it gets read-only.
func TestDockerLandedTreeIsCleanAndTheCloneReadOnly(t *testing.T) {
	f := newDockerFixture(t, nil)
	ctx := context.Background()
	tree, err := f.docker.LandedTree(ctx, "", f.commit)
	if err != nil {
		t.Fatalf("LandedTree() error = %v", err)
	}
	if again, err := f.docker.LandedTree(ctx, "", f.commit); err != nil || again != tree {
		t.Fatalf("second LandedTree() = %q, %v", again, err)
	}
	run := shell("tree", `cd "$LAND_TREE" || exit 9
[ "$(git rev-parse HEAD)" = "$LAND_COMMIT" ] || exit 1
[ -z "$(git status --porcelain --untracked-files=all)" ] || exit 2
touch "$LAND_GIT_DIR/hook-was-here" 2>/dev/null && exit 3
git -C "$LAND_TREE" show HEAD:Alpha.lean > /dev/null || exit 4
exit 0`)
	run.Env = map[string]string{"LAND_TREE": tree, "LAND_COMMIT": f.commit, "LAND_GIT_DIR": f.docker.GitDir}
	run.SafeDirectory = tree
	run.Mounts = []Mount{{Name: "git_dir", HostPath: f.docker.GitDir, Mode: ReadOnly}}
	result, err := f.docker.Run(ctx, run)
	if err != nil || result.Exit != 0 {
		t.Fatalf("hook = exit %d, %v: %s", result.Exit, err, result.Stderr)
	}
	if err := f.docker.RemoveLandedTree(ctx, "", f.commit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the landed tree is still there: %v", err)
	}
}

// Start refuses a configuration whose clone, run root or mount path does not exist; a mount may be a file (a DSN
// file) or a directory.
func TestDockerStartRefusesAMissingPathAndAcceptsAFile(t *testing.T) {
	f := newDockerFixture(t, nil)
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "index-dsn")
	writeFixture(t, dsn, "postgres://x\n")
	directory := t.TempDir()
	if err := f.docker.Start(ctx, []Mount{{Name: "index-dsn", HostPath: dsn}, {Name: "index", HostPath: directory}}, false); err != nil {
		t.Fatalf("Start() with a file and a directory mount = %v", err)
	}
	missing := filepath.Join(directory, "absent")
	if err := f.docker.Start(ctx, []Mount{{Name: "index", HostPath: missing}}, false); err == nil || !strings.Contains(err.Error(), `mount "index"`) {
		t.Fatalf("Start() with a missing mount = %v", err)
	}
	gone := f.docker
	gone.GitDir = filepath.Join(directory, "no-clone.git")
	if err := gone.Start(ctx, nil, false); err == nil || !strings.Contains(err.Error(), "repository.git_dir") {
		t.Fatalf("Start() with a missing clone = %v", err)
	}
}

// A member whose .gitattributes marks a .lean file export-ignore is built with that file present, and with
// export-subst and ident paths at their blob bytes.
func TestContainerBuildHasEveryPathOfTheTree(t *testing.T) {
	f := newDockerFixture(t, map[string]string{
		".gitattributes": "Hidden.lean export-ignore\nSubst.lean export-subst ident\n",
		"Hidden.lean":    "def hidden := 0\n",
		"Subst.lean":     "-- $Format:%H$ $Id$\n",
	})
	builder := ContainerBuilder{Docker: f.docker, Mounts: []Mount{{Name: "lake-cache", HostPath: f.docker.BuildRoot, Mode: ReadWrite}},
		Command: []string{"/bin/sh", "-c", `test -f Hidden.lean && grep -qF '$Format:%H$ $Id$' Subst.lean || exit 1`}}
	result, err := builder.Build(context.Background(), Tree(f.tree), nil)
	if err != nil || !result.Green {
		t.Fatalf("Build() = %+v, %v", result, err)
	}
}

// A build reads its verdict from the result file; without one, an exit above 128 is an infrastructure failure.
func TestContainerBuildVerdict(t *testing.T) {
	f := newDockerFixture(t, nil)
	mounts := []Mount{{Name: "lake-cache", HostPath: f.docker.BuildRoot, Mode: ReadWrite}}
	for _, test := range []struct {
		script string
		green  bool
		absent bool
		infra  string
	}{
		{`printf '{"green":true,"absent_targets":[]}' > "$LAND_RESULT_FILE"`, true, false, ""},
		{`printf '{"green":false,"absent_targets":["Linux.NoSuchModule"]}' > "$LAND_RESULT_FILE"; exit 1`, false, true, ""},
		{`exit 1`, false, false, ""},
		// The build's shell is PID 1 of its container, which ignores its own SIGKILL; a killed child is the case.
		{`sh -c 'kill -9 $$'`, false, false, "resource-kill"},
	} {
		builder := ContainerBuilder{Docker: f.docker, Command: []string{"/bin/sh", "-c", test.script}, Mounts: mounts}
		result, err := builder.Build(context.Background(), Tree(f.tree), []string{"Linux.NoSuchModule"})
		if err != nil || result.Green != test.green || result.Absent != test.absent || result.InfraClass != test.infra {
			t.Fatalf("%s: Build() = %+v, %v", test.script, result, err)
		}
	}
}

// The hasher runs in the clone with the names in LAND_NAMES_FILE, and its exits 3 and 75 are unable and retry.
func TestContainerHasher(t *testing.T) {
	f := newDockerFixture(t, nil)
	hasher := ContainerHasher{Docker: f.docker, Command: []string{"/bin/sh", "-c",
		`test "$PWD" = "$LAND_GIT_DIR" && git cat-file -e "$LAND_TREE" && grep -q Alpha "$LAND_NAMES_FILE" || exit 3
printf 'Alpha\th\tAlpha.lean\n'`}}
	hashes, err := hasher.Hashes(context.Background(), HashInput{Tree: Tree(f.tree), Names: []string{"Alpha"}})
	if err != nil || hashes["Alpha"].Hash != "h" {
		t.Fatalf("Hashes() = %v, %v", hashes, err)
	}
	for code, check := range map[string]func(error) bool{
		"3":  func(err error) bool { var e HashUnableError; return errors.As(err, &e) },
		"75": func(err error) bool { var e HashRetryError; return errors.As(err, &e) },
	} {
		hasher.Command = []string{"/bin/sh", "-c", "exit " + code}
		if _, err := hasher.Hashes(context.Background(), HashInput{Tree: Tree(f.tree), Names: []string{"Alpha"}}); !check(err) {
			t.Fatalf("exit %s: error = %v", code, err)
		}
	}
}

// A check runs in the clone with the candidate tree and the base, and a graph check's exit 3 is unable.
func TestContainerChecker(t *testing.T) {
	f := newDockerFixture(t, nil)
	checker := ContainerChecker{Docker: f.docker, CheckName: "files", Command: []string{"/bin/sh", "-c",
		`git cat-file -e "$LAND_TREE" && git cat-file -e "$LAND_BASE" || exit 9; printf 'Alpha.lean\tno\n'`}}
	findings, err := checker.Check(context.Background(), CheckInput{Tree: Tree(f.tree), Base: f.commit})
	if err != nil || len(findings) != 1 || findings[0].Path != "Alpha.lean" {
		t.Fatalf("Check() = %v, %v", findings, err)
	}
	checker.Format, checker.Command = FormatGraph, []string{"/bin/sh", "-c", "echo index of another tree >&2; exit 3"}
	var unable *CheckUnableError
	if _, err := checker.Check(context.Background(), CheckInput{Tree: Tree(f.tree), Base: f.commit}); !errors.As(err, &unable) {
		t.Fatalf("graph exit 3 = %v", err)
	}
}

// packageFixture is a packages source (an index tree) whose lake-manifest.json names two git packages, one with a
// subDir as iris has, each with outputs in its own .lake directory, and a tree whose manifest is that one.
type packageFixture struct {
	dockerFixture
	source string
	hash   string
}

func newPackageFixture(t *testing.T) packageFixture {
	t.Helper()
	source := t.TempDir()
	revs := map[string]string{}
	for _, name := range []string{"batteries", "iris"} {
		dir := filepath.Join(source, ".lake", "packages", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "init", "-q")
		gitIn(t, dir, "config", "user.email", "test@example.invalid")
		gitIn(t, dir, "config", "user.name", "Test")
		writeFixture(t, filepath.Join(dir, "Source.lean"), "def "+name+" := 0\n")
		writeFixture(t, filepath.Join(dir, ".gitignore"), ".lake\n")
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-qm", name)
		revs[name] = gitIn(t, dir, "rev-parse", "HEAD")
		writeFixture(t, filepath.Join(dir, ".lake", "build", "out.olean"), "outputs")
	}
	writeFixture(t, filepath.Join(source, ".lake", "packages", "iris", "Iris", ".lake", "build", "out.olean"), "outputs")
	writeFixture(t, filepath.Join(source, ".lake", "packages", "iris", ".gitignore"), ".lake\nIris/.lake\n")
	gitIn(t, filepath.Join(source, ".lake", "packages", "iris"), "commit", "-qam", "ignore")
	revs["iris"] = gitIn(t, filepath.Join(source, ".lake", "packages", "iris"), "rev-parse", "HEAD")
	manifest := `{"version": "1.1.0", "packagesDir": ".lake/packages", "packages": [
 {"type": "path", "name": "Local", "dir": "local"},
 {"type": "git", "name": "batteries", "rev": "` + revs["batteries"] + `", "url": "https://example.invalid/b"},
 {"type": "git", "name": "iris", "rev": "` + revs["iris"] + `", "subDir": "Iris", "url": "https://example.invalid/i"}]}
`
	writeFixture(t, filepath.Join(source, "lake-manifest.json"), manifest)
	f := newDockerFixture(t, map[string]string{"lake-manifest.json": manifest, "Alpha.lean": "def alpha := 0\n"})
	f.docker.PackagesSource = source
	sum := sha256.Sum256([]byte(manifest))
	return packageFixture{dockerFixture: f, source: source, hash: hex.EncodeToString(sum[:])}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	output, err := exec.Command("stat", "-c", "%i", path).Output()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	var value uint64
	for _, c := range strings.TrimSpace(string(output)) {
		value = value*10 + uint64(c-'0')
	}
	return value
}

// The fill copies each git package from the index tree without its .lake, and without <subDir>/.lake for a
// package with a subDir; a build's .lake/packages files are hard links of the entry's, and a build leaves every
// file of the entry as it was.
func TestPackageStoreFillsFromTheIndexTreeAndLinksIntoABuild(t *testing.T) {
	f := newPackageFixture(t)
	ctx := context.Background()
	entry, err := f.docker.Packages(ctx, Tree(f.tree))
	if err != nil {
		t.Fatalf("Packages() error = %v", err)
	}
	if want := filepath.Join(f.docker.BuildRoot, ".land-packages", f.hash); entry != want {
		t.Fatalf("entry = %q, want %q", entry, want)
	}
	for _, absent := range []string{"batteries/.lake", "iris/.lake", "iris/Iris/.lake", "Local"} {
		if _, err := os.Stat(filepath.Join(entry, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the entry holds %s: %v", absent, err)
		}
	}
	for _, present := range []string{"batteries/Source.lean", "batteries/.git", "iris/Source.lean", "iris/.git"} {
		if _, err := os.Stat(filepath.Join(entry, present)); err != nil {
			t.Fatalf("the entry lacks %s: %v", present, err)
		}
	}
	before := snapshot(t, entry)
	builder := ContainerBuilder{Docker: f.docker, Mounts: []Mount{{Name: "lake-cache", HostPath: f.docker.BuildRoot, Mode: ReadWrite}},
		Command: []string{"/bin/sh", "-c", `stat -c %i .lake/packages/batteries/Source.lean > /land/scratch/inode
git -C .lake/packages/batteries diff --exit-code HEAD >/dev/null || exit 1
mkdir -p .lake/packages/batteries/.lake/build && echo built > .lake/packages/batteries/.lake/build/new.olean`}}
	kept, err := builder.BuildKept(ctx, Tree(f.tree))
	if err != nil || !kept.Result.Green {
		t.Fatalf("BuildKept() = %+v, %v", kept.Result, err)
	}
	linked := filepath.Join(kept.Directory, ".lake", "packages", "batteries", "Source.lean")
	if inode(t, linked) != inode(t, filepath.Join(entry, "batteries", "Source.lean")) {
		t.Fatal("the build's package file is not a hard link of the entry's")
	}
	kept.Release()
	if _, err := os.Stat(kept.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the build directory is still there: %v", err)
	}
	if after := snapshot(t, entry); after != before {
		t.Fatalf("the entry changed across a build:\n%s\n---\n%s", before, after)
	}
}

// snapshot is each file of dir with its inode, size, modification time and SHA-256.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	output, err := exec.Command("sh", "-c", `cd "$1" && find . -type f -print0 | sort -z | xargs -0 stat -c '%n %i %s %Y' && find . -type f -print0 | sort -z | xargs -0 sha256sum`, "snapshot", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// A fill is refused, and leaves no entry, when a package is not at its manifest's rev; a tree whose manifest is
// not the index tree's is packages_changed.
func TestPackageStoreRefusesAMovedPackageAndAnotherManifest(t *testing.T) {
	f := newPackageFixture(t)
	ctx := context.Background()
	dir := filepath.Join(f.source, ".lake", "packages", "batteries")
	writeFixture(t, filepath.Join(dir, "More.lean"), "def more := 0\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "moved")
	if _, err := f.docker.Packages(ctx, Tree(f.tree)); err == nil || !strings.Contains(err.Error(), "not the manifest's") {
		t.Fatalf("Packages() with a moved package = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.docker.BuildRoot, ".land-packages")); len(entries) != 0 {
		t.Fatalf("a refused fill left %d entries", len(entries))
	}
	writeFixture(t, filepath.Join(f.repo, "lake-manifest.json"), `{"packages": [{"type": "git", "name": "batteries", "rev": "`+strings.Repeat("a", 40)+`"}]}`)
	gitIn(t, f.repo, "commit", "-qam", "new manifest")
	var changed PackagesChangedError
	if _, err := f.docker.Packages(ctx, Tree(gitIn(t, f.repo, "rev-parse", "HEAD^{tree}"))); !errors.As(err, &changed) {
		t.Fatalf("Packages() of another manifest = %v, want PackagesChangedError", err)
	}
	builder := ContainerBuilder{Docker: f.docker, Command: []string{"true"}, Mounts: []Mount{{Name: "lake-cache", HostPath: f.docker.BuildRoot, Mode: ReadWrite}}}
	result, err := builder.Build(ctx, Tree(gitIn(t, f.repo, "rev-parse", "HEAD^{tree}")), nil)
	if err != nil || result.InfraClass != "packages_changed" {
		t.Fatalf("a blame build of another manifest = %+v, %v; want infrastructure class packages_changed", result, err)
	}
}

// Two fills of one entry at once leave one complete entry.
func TestPackageStoreConcurrentFillsLeaveOneEntry(t *testing.T) {
	f := newPackageFixture(t)
	var wait sync.WaitGroup
	errs := make([]error, 2)
	for index := range errs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, errs[index] = f.docker.Packages(context.Background(), Tree(f.tree))
		}()
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("a fill failed: %v", err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(f.docker.BuildRoot, ".land-packages"))
	if len(entries) != 1 {
		t.Fatalf("the store holds %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(f.docker.BuildRoot, ".land-packages", f.hash, "iris", "Source.lean")); err != nil {
		t.Fatalf("the entry is incomplete: %v", err)
	}
	if leftovers, _ := os.ReadDir(f.docker.buildsDirectory()); len(leftovers) != 0 {
		t.Fatalf("the fills left %d copies", len(leftovers))
	}
}

// A check with network "default" reaches a fixture server, and the same check with none cannot: graphcheck reads
// the index over the network once it is in Postgres.
func TestContainerCheckerNetwork(t *testing.T) {
	f := newDockerFixture(t, nil)
	url, _ := fixtureServer(t)
	script := `for i in 1 2 3 4 5; do curl -fsS -m 2 -o /dev/null "$1" && exit 0; sleep 1; done; exit 3`
	for _, test := range []struct {
		network Network
		unable  bool
	}{{NetworkHost, false}, {NetworkNone, true}, {"", true}} {
		checker := ContainerChecker{Docker: f.docker, CheckName: "names", Format: FormatGraph, Network: test.network,
			Command: []string{"/bin/sh", "-c", script, "probe", url}}
		_, err := checker.Check(context.Background(), CheckInput{Tree: Tree(f.tree), Base: f.commit})
		var unable *CheckUnableError
		if errors.As(err, &unable) != test.unable {
			t.Fatalf("network %q: Check() error = %v", test.network, err)
		}
	}
}

// fixtureServer is an HTTP server on docker's bridge, which only a container with a network can reach. It returns
// its URL and its container's name.
func fixtureServer(t *testing.T) (string, string) {
	t.Helper()
	name := "land-test-server-" + strings.ToLower(uniqueName("s"))
	if output, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--entrypoint", "python3", testdocker.Image,
		"-m", "http.server", "8080").CombinedOutput(); err != nil {
		t.Fatalf("start the fixture server: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	address, err := exec.Command("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil || len(bytes.TrimSpace(address)) == 0 {
		t.Fatalf("read the server's address: %v %q", err, address)
	}
	return "http://" + strings.TrimSpace(string(address)) + ":8080/", name
}

// The worktree is added in the clone the caller names, not the executor's own: a batch's hooks mount the clone
// they were frozen with, so its tree must be there.
func TestDockerLandedTreeUsesTheCloneItIsGiven(t *testing.T) {
	f := newDockerFixture(t, nil)
	other := f.docker
	other.GitDir = filepath.Join(t.TempDir(), "frozen.git")
	if err := os.MkdirAll(other.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := other.InitClone(context.Background()); err != nil {
		t.Fatal(err)
	}
	tree, err := f.docker.LandedTree(context.Background(), other.GitDir, f.commit)
	if err != nil {
		t.Fatalf("LandedTree() error = %v", err)
	}
	if want := filepath.Join(other.GitDir, "trees", f.commit); tree != want {
		t.Fatalf("tree = %q, want %q", tree, want)
	}
	if _, err := os.Stat(filepath.Join(tree, "Alpha.lean")); err != nil {
		t.Fatalf("the named clone has no worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.docker.GitDir, "trees", f.commit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the executor's own clone got the worktree: %v", err)
	}
	if err := f.docker.RemoveLandedTree(context.Background(), other.GitDir, f.commit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree is still there: %v", err)
	}
}
