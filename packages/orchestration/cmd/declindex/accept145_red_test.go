package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Accept145 (INFRA-145 acceptance, E3 outcome 4; contract 4, D-7, D-8): an addition is the repository file
// registry/concepts/<compiled hash>.yaml, which its proposer writes in its own worktree and opens with
// `ledger open --write <addition.path>` (declindex/README.md, "Concept registry"). The gate prints the absolute
// path of that file inside the gate's tree instead, and the ledger refuses an absolute write path ("path ... is
// absolute; give it relative to the repository root", ledger/paths.go); an absolute path also names the gate's
// tree, not the proposer's worktree, so two exact duplicates proposed from different trees would not overlap.
func TestGateAdditionPathIsRepositoryRelative(t *testing.T) {
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
	want := "registry/concepts/" + verdict.CompiledHash + ".yaml"
	if verdict.CompiledHash == "" || verdict.Addition.Path != want {
		t.Fatalf("addition path %q, want the repository-relative %q that `ledger open --write` accepts", verdict.Addition.Path, want)
	}
}
