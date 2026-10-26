package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

func gateFixture(t *testing.T) (string, string, string) {
	t.Helper()
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	registry := filepath.Join(tree, "registry")
	if err := os.MkdirAll(filepath.Join(registry, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	writeIndexConfig(t, config, string(body)+"registry: registry\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(filepath.Dir(config), "proposal.json")
	if err := os.WriteFile(proposal, []byte(`{"id":"p","concept":{"name":"Fresh","home":"New.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return config, tree, proposal
}

func TestGateRequiresAValidProposer(t *testing.T) {
	config, _, proposal := gateFixture(t)
	for _, args := range [][]string{
		{"--config", config, "gate", proposal},
		{"--config", config, "gate", "--proposer", "not an owner", proposal},
		{"--config", config, "gate", "--proposer", "role", "--accepted-by", "no!", proposal},
	} {
		r := invoke(t, args...)
		if r.code != 2 || !strings.Contains(r.errOut, "owner id") {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

func TestGateRequiresIndexCommitAndCleanRegistry(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		path := filepath.Join(tree, "Golden.lean")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(body, []byte("\n-- changed\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, tree, "commit", "-qam", "advance tree")
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "index commit") {
			t.Fatalf("head mismatch: %+v", r)
		}
	})
	t.Run("registry", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		if err := os.WriteFile(filepath.Join(tree, "registry", "untracked"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry") {
			t.Fatalf("dirty registry: %+v", r)
		}
	})
}

func TestGateRejectsInTreeStateAndEscapedRegistry(t *testing.T) {
	t.Run("state", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		writeIndexConfig(t, config, strings.Replace(string(body), "state: state", "state: tree/.declindex", 1))
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "outside the tree") {
			t.Fatalf("in-tree state: %+v", r)
		}
		if status := git(t, tree, "status", "--porcelain"); status != "" {
			t.Fatalf("gate dirtied the tree: %q", status)
		}
	})
	t.Run("registry", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		outside := filepath.Join(filepath.Dir(tree), "outside-registry")
		if err := os.MkdirAll(filepath.Join(outside, "concepts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		writeIndexConfig(t, config, strings.Replace(string(body), "registry: registry", "registry: ../outside-registry", 1))
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry") {
			t.Fatalf("escaped registry: %+v", r)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		outside := filepath.Join(filepath.Dir(tree), "symlink-registry")
		if err := os.MkdirAll(filepath.Join(outside, "concepts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(tree, "registry")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(tree, "registry")); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "inside the tree") {
			t.Fatalf("symlink registry: %+v", r)
		}
	})
}

// A tree that is not a Git work tree has no commit to compare the index's commit with: the gate is unable to
// decide, rather than comparing two empty commits.
func TestGateIsUnableOnATreeThatIsNotAGitWorkTree(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	writeIndexConfig(t, config, string(body)+"registry: registry\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(filepath.Dir(config), "proposal.json")
	writeTestFile(t, proposal, `{"id":"p","concept":{"name":"Fresh","home":"New.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "commit of the tree") {
		t.Fatalf("gate on a tree that is not a Git work tree: want exit 3, no stdout and the tree's commit named, got %+v", r)
	}
}

// An addition is the repository file registry/concepts/<compiled hash>.yaml, which its proposer writes in its own
// worktree and opens with `ledger open --write <addition.path>` (declindex/README.md, "Concept registry"). The
// ledger refuses an absolute write path ("path ... is absolute; give it relative to the repository root",
// ledger/paths.go), and an absolute path would name the gate's tree, not the proposer's worktree, so two exact
// duplicates proposed from different trees would not overlap. The gate prints the repository-relative path.
func TestGatePrintsCanonicalAdditionPathAfterSnapshotCleanup(t *testing.T) {
	requireLake(t)
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "registry", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "Fixture.lean"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	lean, err := declindex.FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "registry", "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(dir, "proposal.json")
	if err := os.WriteFile(proposal, []byte(`{"id":"p","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	var verdict struct {
		CompiledHash string `json:"compiled_hash"`
		Addition     struct {
			Path string `json:"path"`
		} `json:"addition"`
	}
	if err := json.Unmarshal([]byte(r.out), &verdict); err != nil {
		t.Fatal(err)
	}
	want := "registry/concepts/" + verdict.CompiledHash + ".yaml"
	if verdict.CompiledHash == "" || verdict.Addition.Path != want {
		t.Fatalf("addition path %q, want %q", verdict.Addition.Path, want)
	}
	if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(verdict.Addition.Path))); !os.IsNotExist(err) {
		t.Fatalf("gate wrote canonical addition path: %v", err)
	}
	if status := git(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("gate dirtied the tree: %q", status)
	}
}

// A tree below its repository's root: the addition path is still repository-relative, the tree's path in the
// repository (which refresh records in the index) followed by the registry's path in the tree.
func TestGatePrintsTheAdditionPathOfATreeBelowTheRepositoryRoot(t *testing.T) {
	requireLake(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	tree := filepath.Join(repo, "proj")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "Fixture.lean"), string(fixture))
	lean, err := declindex.FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(repo, "README"), "a repository whose Lean project is below its root\n")
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: repo/proj\nlib: .\nindex: idx\nstate: state\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(dir, "proposal.json")
	writeTestFile(t, proposal, `{"id":"p","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	var verdict struct {
		CompiledHash string `json:"compiled_hash"`
		Addition     struct {
			Path string `json:"path"`
		} `json:"addition"`
	}
	if err := json.Unmarshal([]byte(r.out), &verdict); err != nil {
		t.Fatal(err)
	}
	want := "proj/registry/concepts/" + verdict.CompiledHash + ".yaml"
	if verdict.CompiledHash == "" || verdict.Addition.Path != want {
		t.Fatalf("addition path %q, want %q", verdict.Addition.Path, want)
	}
}

// leanGateFixture is the Lean fixture tree, compiled, with a format-2 registry holding concepts (file name to
// content), committed, and a configuration that reads the compiled part; the index is refreshed. It needs Lean.
func leanGateFixture(t *testing.T, concepts map[string]string) (dir, tree, config string) {
	t.Helper()
	requireLake(t)
	dir = t.TempDir()
	tree = filepath.Join(dir, "tree")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "Fixture.lean"), string(fixture))
	lean, err := declindex.FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	for name, body := range concepts {
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", name), body)
	}
	config = filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	return dir, tree, config
}

// A concept file that the loader refuses stops neither a refresh that reads the compiled part nor the gate: both
// note the file as a registry defect and go on without it.
func TestAMalformedConceptFileStopsNeitherRefreshNorTheGate(t *testing.T) {
	dir, _, config := leanGateFixture(t, map[string]string{
		"Fx.Bad.yaml": "name: Fx.Bad\nhome: Fixture.lean\nmeaning: Register x2 is aligned. It stays so.\n",
	})
	proposal := filepath.Join(dir, "proposal.json")
	writeTestFile(t, proposal, `{"id":"p9","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "function:open_file", proposal)
	if r.code != 0 || !strings.Contains(r.errOut, "concepts/Fx.Bad.yaml") {
		t.Fatalf("gate with one malformed concept file: want exit 0 and a defect note naming it, got %+v", r)
	}
}
