package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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
	if err := os.WriteFile(config, append(body, []byte("registry: registry\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
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
	old := gateSandboxAvailable
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() { gateSandboxAvailable = old })
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
	old := gateSandboxAvailable
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() { gateSandboxAvailable = old })
	t.Run("state", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		body = []byte(strings.Replace(string(body), "db: state/index.sqlite", "db: tree/.declindex/index.sqlite", 1))
		if err := os.WriteFile(config, body, 0o644); err != nil {
			t.Fatal(err)
		}
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
		body = []byte(strings.Replace(string(body), "registry: registry", "registry: ../outside-registry", 1))
		if err := os.WriteFile(config, body, 0o644); err != nil {
			t.Fatal(err)
		}
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

func TestGateRejectsRegistryMutationAfterInitialStatus(t *testing.T) {
	oldSandbox, oldStatus := gateSandboxAvailable, gateAfterRegistryStatus
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() {
		gateSandboxAvailable = oldSandbox
		gateAfterRegistryStatus = oldStatus
	})
	config, tree, proposal := gateFixture(t)
	gateAfterRegistryStatus = func() {
		if err := os.WriteFile(filepath.Join(tree, "registry", "mutated"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry has local changes") {
		t.Fatalf("registry mutation: %+v", r)
	}
}

func TestGatePrintsCanonicalAdditionPathAfterSnapshotCleanup(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("the successful gate path compiles untrusted source in Docker")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
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
	body := "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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
	want := filepath.Join(tree, "registry", "concepts", verdict.CompiledHash+".yaml")
	if verdict.CompiledHash == "" || verdict.Addition.Path != want || strings.Contains(verdict.Addition.Path, "registry-snapshot-") {
		t.Fatalf("addition path %q, want %q", verdict.Addition.Path, want)
	}
	if _, err := os.Stat(verdict.Addition.Path); !os.IsNotExist(err) {
		t.Fatalf("gate wrote canonical addition path: %v", err)
	}
	if status := git(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("gate dirtied the tree: %q", status)
	}
}
