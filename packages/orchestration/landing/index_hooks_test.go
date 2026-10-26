package landing

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/indextest"
	"github.com/sjawhar/legion/orchestration/internal/testdocker"
)

var (
	declindexOnce   sync.Once
	declindexBinary string
	declindexErr    error
)

// buildDeclindex builds the declaration index's command once per test binary, static, so it runs in the image.
func buildDeclindex(t *testing.T) string {
	t.Helper()
	declindexOnce.Do(func() {
		dir, err := os.MkdirTemp("", "land-declindex-")
		if err != nil {
			declindexErr = err
			return
		}
		declindexBinary = filepath.Join(dir, "declindex")
		command := exec.Command("go", "build", "-o", declindexBinary, "../cmd/declindex")
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			declindexErr = errors.New(err.Error() + ": " + string(output))
		}
	})
	if declindexErr != nil {
		t.Fatalf("build declindex: %v", declindexErr)
	}
	return declindexBinary
}

// indexFixture is the post-landing index of one repository: a seed repository with two commits, a bare remote that
// holds both, an index tree cloned at the first, an index (a schema of a test database whose connection string is
// in the file dsn) refreshed at the first, a state directory, and a declindex configuration with absolute paths, as
// the landing configuration passes it.
type indexFixture struct {
	seed, remote, tree, state, dsn, readerDSN, config, bin string
	first, second                                          string
}

func newIndexFixture(t *testing.T) indexFixture {
	t.Helper()
	bin := buildDeclindex(t)
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	golden := filepath.Join("..", "declindex", "testdata", "golden")
	if err := filepath.WalkDir(golden, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(golden, path)
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(seed, relative), 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(seed, relative), body, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(seed, "owners.json"), `{"format":"cgraph/owners/v1","owners":[{"id":"function:frame_at","contract":"Sub"}]}`)
	gitIn(t, seed, "init", "-q")
	gitIn(t, seed, "config", "user.email", "test@example.invalid")
	gitIn(t, seed, "config", "user.name", "Test")
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-qm", "first")
	first := gitIn(t, seed, "rev-parse", "HEAD")
	remote := filepath.Join(root, "remote.git")
	gitIn(t, root, "clone", "-q", "--bare", seed, remote)
	tree := filepath.Join(root, "index-tree")
	gitIn(t, root, "clone", "-q", remote, tree)
	writeFixture(t, filepath.Join(seed, "landing.txt"), "second\n")
	gitIn(t, seed, "add", "landing.txt")
	gitIn(t, seed, "commit", "-qm", "second")
	second := gitIn(t, seed, "rev-parse", "HEAD")
	gitIn(t, seed, "push", "-q", remote, "HEAD:refs/heads/master")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// The refresh writes the index with the login that owns its schema; the ledger reads it with the reader login
	// that the refresh grants the schema's reads to, as the landing configuration's index-dsn and index-reader-dsn do.
	owner := indextest.DSN(t)
	role, reader := indextest.ReaderRole(t, owner)
	dsn := filepath.Join(root, "secrets", "index-dsn")
	writeFixture(t, dsn, owner+"\n")
	readerDSN := filepath.Join(root, "secrets", "index-reader-dsn")
	writeFixture(t, readerDSN, reader+"\n")
	config := filepath.Join(root, "conf", "declindex.yaml")
	writeFixture(t, config, "tree: "+tree+"\nlib: .\nindex: landing\nstate: "+state+"\nreader: "+role+"\nroots: [Golden, Sub]\nfile_kinds: [Spec, Link, Proof]\n"+
		"remote: "+remote+"\nbuild: [\"true\"]\nbuild_timeout: 30s\n"+
		"repo: r\nowners: owners.json\nowners_format: cgraph/owners/v1\ncontracts:\n  paths: [\"Sub/**\"]\n  kinds: [def]\n  name: \"^[A-Z]\"\n")
	if output, err := exec.Command(bin, "--config", config, "--index-dsn-file", dsn, "refresh").CombinedOutput(); err != nil {
		t.Fatalf("initial refresh: %v: %s", err, output)
	}
	return indexFixture{seed: seed, remote: remote, tree: tree, state: state, dsn: dsn, readerDSN: readerDSN, config: config, bin: bin, first: first, second: second}
}

// indexHooks is a landed batch of f.second whose refresh and ledger hooks run in real containers, with this
// repository's modes: the refresh gets the state directory and the index tree read-write, the remote read-only and
// the owner login's connection string; the ledger hook gets the state directory and the tree read-only and the reader
// login's connection string, so it cannot write the index. Each reads its connection string's file, read-only, and
// reaches the database on the host network, as the index-dsn and index-reader-dsn file mounts and network "host" give
// them in the landing configuration. withRemote false leaves the remote out of the refresh's mounts.
func indexHooks(t *testing.T, f indexFixture, withRemote bool) (Runner, *effectLogFake, Batch) {
	t.Helper()
	user := testdocker.Require(t)
	root := t.TempDir()
	d := Docker{Image: testdocker.Image, User: user, GitDir: filepath.Join(root, "clone.git"), RepositoryPath: f.seed,
		Repository: "r", RunRoot: filepath.Join(root, "runs"), BuildRoot: filepath.Join(root, "lakecache")}
	for _, dir := range []string{d.GitDir, d.RunRoot, d.BuildRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Start(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	declared := map[string]Mount{
		"tools":            {Name: "tools", HostPath: filepath.Dir(f.bin)},
		"conf":             {Name: "conf", HostPath: filepath.Dir(f.config)},
		"index-dsn":        {Name: "index-dsn", HostPath: f.dsn},
		"index-reader-dsn": {Name: "index-reader-dsn", HostPath: f.readerDSN},
		"state":            {Name: "state", HostPath: f.state},
		"index-tree":       {Name: "index-tree", HostPath: f.tree},
		"index-remote":     {Name: "index-remote", HostPath: f.remote},
	}
	refreshMounts := map[string]MountMode{"tools": ReadOnly, "conf": ReadOnly, "index-dsn": ReadOnly, "state": ReadWrite, "index-tree": ReadWrite}
	if withRemote {
		refreshMounts["index-remote"] = ReadOnly
	}
	hooks := []HookConfig{
		{Name: "refresh", Command: []string{f.bin, "--config", f.config, "--index-dsn-file", f.dsn, "refresh"},
			Apply: ApplyNone, Mounts: refreshMounts, Network: NetworkHost},
		{Name: "ledger", Command: []string{f.bin, "--config", f.config, "--index-dsn-file", f.readerDSN,
			"ledger", "--contracts-file", inputsPath + "/" + ContractsInput, "--emit", scratchPath + "/ops.jsonl"},
			Apply: ApplyContracts, Outputs: []string{"ops.jsonl"}, Network: NetworkHost,
			Mounts: map[string]MountMode{"tools": ReadOnly, "conf": ReadOnly, "index-reader-dsn": ReadOnly, "state": ReadOnly, "index-tree": ReadOnly}},
	}
	runner := Runner{
		PostLanding:   PostLanding{Parts: []string{"slice"}, Hooks: hooks, TimeoutSeconds: 300, MaxOutputBytes: 1 << 20},
		Container:     Container{Image: d.Image, User: d.User, GitDir: d.GitDir, Mounts: declared},
		EscalateTo:    "escalations",
		EscalateAfter: 3,
		RetryBackoff:  time.Nanosecond,
		Containers:    d,
		Trees:         d,
	}
	if err := runner.PostLanding.Validate(runner.Container); err != nil {
		t.Fatal(err)
	}
	effects := newEffectLog()
	runner.Effects = effects
	batch := Batch{ID: "batch", Repository: "r", State: Landed, PullRequest: PullRequest{Number: 7, MergedCommit: f.second, State: PullRequestMerged}}
	batch.PostLandEffects = runner.postLandEffects(batch)
	for _, effect := range batch.PostLandEffects {
		effects.create(effect.Key, effect.Op, "", effect.Member, map[string]any{"commit": f.second})
		batch.PendingEffects = append(batch.PendingEffects, effect.Key)
	}
	slices.Sort(batch.PendingEffects)
	return runner, effects, batch
}

// The index moves, then the ledger hook applies. The refresh fetches the landed commit from the read-only remote,
// checks it out in the read-write index tree and moves the index to it, writing only as the configured user; then
// the ledger hook, with the tree read-only and the index through its reader login, decides, and its operations apply
// once.
func TestTheIndexMovesThenTheLedgerHookApplies(t *testing.T) {
	f := newIndexFixture(t)
	runner, effects, batch := indexHooks(t, f, true)
	if err := runner.finishEffects(context.Background(), &batch); err != nil {
		t.Fatalf("finishEffects() error = %v (escalations %q)", err, effects.escalations)
	}
	if head := gitIn(t, f.tree, "rev-parse", "HEAD"); head != f.second {
		t.Fatalf("the index tree is at %s, want the landed commit %s", head, f.second)
	}
	status, err := exec.Command(f.bin, "--config", f.config, "--index-dsn-file", f.dsn, "--json", "status").CombinedOutput()
	if err != nil || !strings.Contains(string(status), f.second) {
		t.Fatalf("declindex status: %v: %s; want the landed commit", err, status)
	}
	// The refresh's container writes the fetched objects and the checkout (landing.txt) into the index tree; the
	// state directory holds what the host made and anything the container added.
	if _, err := os.Stat(filepath.Join(f.tree, "landing.txt")); err != nil {
		t.Fatalf("the refresh did not check out the landed commit's file: %v", err)
	}
	for _, written := range []string{f.tree, f.state} {
		if err := filepath.WalkDir(written, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if owner := info.Sys().(*syscall.Stat_t).Uid; strconv.Itoa(int(owner)) != strconv.Itoa(os.Getuid()) {
				t.Errorf("%s is owned by %d, not the configured user", path, owner)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(effects.applied, []string{"apply/batch/1/slice"}) {
		t.Fatalf("the applies are %q, want the ledger hook's once", effects.applied)
	}
	if ops := string(effects.outputs["hook/batch/1/slice"]["ops.jsonl"]); !strings.Contains(ops, "Gold2.FrameAt") {
		t.Fatalf("the ledger hook's operations are %q", ops)
	}
}

// Negative control: without the remote in its mounts the refresh cannot fetch the landed commit and exits 75; it
// is retried and escalated at the threshold, and the ledger hook never runs.
func TestARefreshWithoutItsRemoteIsRetriedAndTheLedgerHookWaits(t *testing.T) {
	f := newIndexFixture(t)
	runner, effects, batch := indexHooks(t, f, false)
	for round := 1; round <= 3; round++ {
		if err := runner.finishEffects(context.Background(), &batch); !errors.Is(err, ErrEffectsPending) {
			t.Fatalf("round %d: finishEffects() error = %v, want the effects pending", round, err)
		}
	}
	if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice@3 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want %q", got, want)
	}
	if effect := effects.effects["hook/batch/1/slice"]; effect.State != EffectIntent || effect.Attempts != 0 || len(effects.applied) != 0 {
		t.Fatalf("the ledger hook ran: %+v, applies %q", effect, effects.applied)
	}
	if head := gitIn(t, f.tree, "rev-parse", "HEAD"); head != f.first {
		t.Fatalf("the index tree moved to %s", head)
	}
}
