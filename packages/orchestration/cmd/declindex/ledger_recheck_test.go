package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
)

func TestLedgerRechecksAddedNearConcepts(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only in Docker with DECLINDEX_SANDBOX_TEST=1")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}

	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	registry := filepath.Join(tree, "registry")
	if err := os.MkdirAll(filepath.Join(registry, "concepts"), 0o755); err != nil {
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
	if err := os.WriteFile(filepath.Join(registry, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configuredTree := filepath.Join(dir, "tree-link")
	if err := os.Symlink(tree, configuredTree); err != nil {
		t.Fatal(err)
	}

	invocations := filepath.Join(dir, "librarian-invocations")
	librarian := filepath.Join(dir, "record-librarian")
	script := "#!/bin/sh\nprintf '%s\\n' \"$2\" >> " + strconv.Quote(invocations) + "\n"
	if err := os.WriteFile(librarian, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "index.yaml")
	body := "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Fixture]\nregistry: registry\nrepo: r\ncontracts:\n  paths: [NoSuch/**]\n  kinds: [def]\nlibrarian:\n  - " + librarian + "\nlean:\n  export: true\n  jobs: 1\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "registry base")
	if result := invoke(t, "--config", config, "refresh"); result.code != 0 {
		t.Fatalf("refresh base: %+v", result)
	}

	addition := func(id, name, source string) conceptgate.Addition {
		t.Helper()
		proposal := conceptgate.Proposal{
			ID:      id,
			Concept: conceptgate.Concept{Name: name, Home: "Fixture.lean", Meaning: "A test concept."},
			Source:  source,
		}
		raw, err := json.Marshal(proposal)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		result := invoke(t, "--config", config, "gate", "--proposer", "role", path)
		if result.code != 0 {
			t.Fatalf("gate %s: %+v", id, result)
		}
		var verdict conceptgate.Verdict
		if err := json.Unmarshal([]byte(result.out), &verdict); err != nil {
			t.Fatal(err)
		}
		if verdict.Addition == nil {
			t.Fatalf("gate %s did not return an addition: %+v", id, verdict)
		}
		return *verdict.Addition
	}
	first := addition("first", "Fx.First", "theorem Fx.First (x : Nat) (_h : x < 3) : True := trivial\n")
	second := addition("second", "Fx.Second", "def Fx.Second (x : Nat) : Prop := x < 3\n")
	for _, addition := range []conceptgate.Addition{first, second} {
		if err := os.WriteFile(addition.Path, []byte(addition.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(registry, "concepts", "README.md"), []byte("support file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(registry, "concepts", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "concepts", "archive", "ignored.yaml"), []byte("name: ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "add", "registry")
	git(t, tree, "commit", "-qm", "add nearby concepts")
	if result := invoke(t, "--config", config, "refresh"); result.code != 0 {
		t.Fatalf("refresh additions: %+v", result)
	}
	if err := os.WriteFile(config, []byte(strings.Replace(body, "tree: tree\n", "tree: tree-link\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	contracts := filepath.Join(dir, "contracts.json")
	if err := os.WriteFile(contracts, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emit := filepath.Join(dir, "operations.jsonl")
	check := func() {
		t.Helper()
		result := invoke(t, "--config", config, "ledger", "--contracts-file", contracts, "--emit", emit)
		if result.code != 1 {
			t.Fatalf("ledger exit code %d, want 1: %+v", result.code, result)
		}
		var report struct {
			Registry *struct {
				Added []string `json:"added"`
				Near  []struct {
					ID string `json:"id"`
				} `json:"near"`
				Defects []struct {
					Path    string `json:"path"`
					Message string `json:"message"`
				} `json:"defects"`
				Unable string `json:"unable"`
			} `json:"registry"`
		}
		if err := json.Unmarshal([]byte(result.out), &report); err != nil {
			t.Fatal(err)
		}
		wantAdded := []string{
			filepath.ToSlash(filepath.Join("registry", "concepts", filepath.Base(first.Path))),
			filepath.ToSlash(filepath.Join("registry", "concepts", filepath.Base(second.Path))),
		}
		sort.Strings(wantAdded)
		if report.Registry == nil || strings.Join(report.Registry.Added, "\x00") != strings.Join(wantAdded, "\x00") ||
			len(report.Registry.Defects) != 0 || report.Registry.Unable != "" || len(report.Registry.Near) != 1 {
			t.Fatalf("registry report %+v, want additions %v", report.Registry, wantAdded)
		}
		got, err := os.ReadFile(invocations)
		if err != nil {
			t.Fatal(err)
		}
		if ids := strings.Fields(string(got)); len(ids) != 1 || ids[0] != report.Registry.Near[0].ID {
			t.Fatalf("librarian invocations %q, want %q once", got, report.Registry.Near[0].ID)
		}
	}
	check()

	if err := os.WriteFile(invocations, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reg, defects, err := conceptgate.Load(registry)
	if err != nil || len(defects) != 0 {
		t.Fatalf("load mutable registry: %v, defects %v", err, defects)
	}
	for _, concept := range reg.Concepts {
		if concept.Name != "Fx.First" {
			continue
		}
		concept.Source = "def Fx.First (x : Nat) : Prop := x < 4\n"
		body, err := conceptgate.Render(concept)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(first.Path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		check()
		return
	}
	t.Fatal("Fx.First is absent from the mutable registry")
}
