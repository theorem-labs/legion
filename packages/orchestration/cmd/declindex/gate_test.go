package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
