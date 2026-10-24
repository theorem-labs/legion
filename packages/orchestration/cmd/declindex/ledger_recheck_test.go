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
	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestLedgerRechecksAddedNearConcepts(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
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
		if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(addition.Path)), []byte(addition.Content), 0o644); err != nil {
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
	// The landing hook mounts the index directory read-only; the recheck works in the temporary directory.
	state := filepath.Join(dir, "state")
	if err := os.Chmod(state, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o755) })
	t.Setenv("TMPDIR", t.TempDir())
	emit := filepath.Join(dir, "operations.jsonl")
	type registryReport struct {
		Added []string `json:"added"`
		Near  []struct {
			ID    string `json:"id"`
			Added string `json:"added"`
		} `json:"near"`
		Defects []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"defects"`
		Unable string `json:"unable"`
	}
	report := func(result result) registryReport {
		t.Helper()
		if result.code != 1 {
			t.Fatalf("ledger exit code %d, want 1: %+v", result.code, result)
		}
		var report struct {
			Registry *registryReport `json:"registry"`
		}
		if err := json.Unmarshal([]byte(result.out), &report); err != nil {
			t.Fatal(err)
		}
		// The ledger reports landed additions by the same repository-relative paths that the gate printed.
		wantAdded := []string{first.Path, second.Path}
		sort.Strings(wantAdded)
		if report.Registry == nil || strings.Join(report.Registry.Added, "\x00") != strings.Join(wantAdded, "\x00") ||
			len(report.Registry.Defects) != 0 || report.Registry.Unable != "" || len(report.Registry.Near) != 1 {
			t.Fatalf("registry report %+v, want additions %v", report.Registry, wantAdded)
		}
		return *report.Registry
	}
	invocationsOf := func() []string {
		t.Helper()
		got, err := os.ReadFile(invocations)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Fields(string(got))
	}
	// The hook has no network: in --emit mode each near pair is an operation for the landing program to deliver,
	// after the registrations, and no librarian command runs.
	checkEmitted := func() {
		t.Helper()
		r := report(invoke(t, "--config", config, "ledger", "--contracts-file", contracts, "--emit", emit))
		raw, err := os.ReadFile(emit)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 1 {
			t.Fatalf("emitted %q, want one librarian operation", raw)
		}
		var op struct {
			Op       string `json:"op"`
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Proposer string `json:"proposer"`
			Envelope string `json:"envelope"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &op); err != nil {
			t.Fatal(err)
		}
		begin := strings.Index(op.Envelope, "-----BEGIN UNTRUSTED PROPOSAL ")
		end := strings.Index(op.Envelope, "-----END UNTRUSTED PROPOSAL ")
		if op.Op != "librarian" || op.ID != r.Near[0].ID || !strings.HasPrefix(op.ID, "recheck-"+r.Near[0].Added+"-") ||
			op.Kind != "recheck" || op.Proposer != "role" || begin < 0 || end < begin ||
			!strings.Contains(op.Envelope[begin:end], `"kind":"recheck"`) {
			t.Fatalf("librarian operation %+v for near pair %+v", op, r.Near[0])
		}
		if ids := invocationsOf(); len(ids) != 0 {
			t.Fatalf("the librarian command ran in --emit mode: %q", ids)
		}
	}
	checkEmitted()
	t.Run("operator DSN", func(t *testing.T) {
		dsn := testpg.Database(t, "declindex_recheck_librarian")
		l, err := ledger.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.EnsureRepo(context.Background(), "r", 3); err != nil {
			t.Fatal(err)
		}
		l.Close()
		r := report(invokeLedger(t, config, dsn, ""))
		if ids := invocationsOf(); len(ids) != 1 || ids[0] != r.Near[0].ID {
			t.Fatalf("librarian invocations %q, want %q once", ids, r.Near[0].ID)
		}
	})

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
		if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(first.Path)), body, 0o644); err != nil {
			t.Fatal(err)
		}
		// The check reads the registry as committed, so a working-tree edit changes nothing.
		checkEmitted()
		return
	}
	t.Fatal("Fx.First is absent from the mutable registry")
}
