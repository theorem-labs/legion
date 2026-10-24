package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// gateAddition runs `gate` on a proposal of the concept name, with its home file and source, and gives the added
// verdict.
func gateAddition(t *testing.T, config, id, name, home, source string) conceptgate.Verdict {
	t.Helper()
	proposal := conceptgate.Proposal{
		ID:      id,
		Concept: conceptgate.Concept{Name: name, Home: home, Meaning: "A test concept."},
		Source:  source,
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), id+".json")
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
	return verdict
}

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
		return *gateAddition(t, config, id, name, "Fixture.lean", source).Addition
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

// leanRegistryTree is a Git tree with the compiled Lean fixture and an empty format-2 registry, configured with the
// compiled part and a contract rule that selects nothing, and refreshed. roots are the roots of the modules that
// later landings add.
func leanRegistryTree(t *testing.T, roots ...string) (config, tree string, lean declindex.Lean) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	tree = filepath.Join(dir, "tree")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "Fixture.lean"), string(fixture))
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	if lean, err = declindex.FindLean(ctx, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(ctx, lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(dir, "index.yaml")
	writeTestFile(t, config, "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: ["+strings.Join(append([]string{"Fixture"}, roots...), ", ")+
		"]\nregistry: registry\nrepo: r\ncontracts:\n  paths: [NoSuch/**]\n  kinds: [def]\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-qm", "registry base")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh base: %+v", r)
	}
	return config, tree, lean
}

// An owner lands the gate's addition together with the concept's declaration in its home file, so the landed build
// declares the concept whose stored source the recheck compiles. The recheck still compares the stored source with
// its recorded hash and with the registry: alone, the addition is clean; landed in one home file with a concept
// whose fact it states, the two are a near pair.
func TestLedgerRechecksAnAdditionLandedWithItsDeclaration(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	t.Run("one concept in a new home file", func(t *testing.T) {
		config, tree, lean := leanRegistryTree(t, "Fresh")
		source := "import Fixture\n\ndef Fx.Fresh (n : Nat) : Prop := n < 3\n"
		fresh := *gateAddition(t, config, "fresh", "Fx.Fresh", "Fresh.lean", source).Addition
		writeTestFile(t, filepath.Join(tree, filepath.FromSlash(fresh.Path)), fresh.Content)
		writeTestFile(t, filepath.Join(tree, "Fresh.lean"), source)
		if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fresh"); err != nil {
			t.Fatal(err)
		}
		landed := land(t, config, tree, "land Fx.Fresh and its declaration")
		r := checkRegistry(t, config, landed)
		if r.code != 0 || !slices.Equal(r.added, []string{fresh.Path}) || len(r.defects) != 0 || r.unable != "" || len(r.ops) != 0 {
			t.Fatalf("want exit 0 with %s added and no defect, cause or operation, got %+v", fresh.Path, r)
		}
	})
	t.Run("two concepts in one home file", func(t *testing.T) {
		config, tree, lean := leanRegistryTree(t, "Both")
		firstSource := "theorem Fx.First (x : Nat) (_h : x < 3) : True := trivial\n"
		secondSource := "def Fx.Second (x : Nat) : Prop := x < 3\n"
		first := gateAddition(t, config, "first", "Fx.First", "Both.lean", firstSource)
		second := gateAddition(t, config, "second", "Fx.Second", "Both.lean", secondSource)
		for _, v := range []conceptgate.Verdict{first, second} {
			writeTestFile(t, filepath.Join(tree, filepath.FromSlash(v.Addition.Path)), v.Addition.Content)
		}
		writeTestFile(t, filepath.Join(tree, "Both.lean"), firstSource+"\n"+secondSource)
		if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Both"); err != nil {
			t.Fatal(err)
		}
		landed := land(t, config, tree, "land Fx.First, Fx.Second and their declarations")
		r := checkRegistry(t, config, landed)
		var report struct {
			Registry struct {
				Near []conceptgate.RecheckNear `json:"near"`
			} `json:"registry"`
		}
		if err := json.Unmarshal([]byte(r.raw), &report); err != nil {
			t.Fatal(err)
		}
		near := report.Registry.Near
		added := []string{first.Addition.Path, second.Addition.Path}
		sort.Strings(added)
		wantID := "recheck-" + first.CompiledHash + "-" + second.CompiledHash
		if r.code != 1 || !slices.Equal(r.added, added) || len(r.defects) != 0 || r.unable != "" || len(near) != 1 ||
			near[0].ID != wantID || len(r.ops) != 1 || r.ops[0]["id"] != wantID {
			t.Fatalf("want exit 1 with %v added, no defect or cause, and the one near pair %s, got %+v, near %+v", added, wantID, r, near)
		}
	})
}

// A landed addition is compared with every registered concept, landed or pending, through every declaration of its
// proposal: two additions gated apart, of which one states the other's fact, are one near pair whichever of them
// lands with its declaration, also when the first states that fact through a helper that its proposal declares,
// before and after that proposal lands.
func TestLedgerRechecksALandedAdditionAgainstEveryConcept(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	const theorem = "theorem Fx.First (x : Nat) (_h : x < 3) : True := trivial\n"
	const helper = "def Fx.Helper (n : Nat) : Prop := n < 3\n\ndef Fx.First (n : Nat) : Prop := Fx.Helper n\n"
	const second = "def Fx.Second (n : Nat) : Prop := n < 3\n"
	for _, c := range []struct {
		name                  string
		first                 string
		landFirst, landSecond bool
	}{
		{"the addition landed, the concept whose fact it states pending", theorem, true, false},
		{"the addition pending, the concept whose fact it states landed", theorem, false, true},
		{"a helper's proposal landed with the concept whose fact it states", helper, true, true},
		{"a helper's proposal and the concept whose fact it states pending", helper, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			config, tree, lean := leanRegistryTree(t, "Both")
			first := gateAddition(t, config, "first", "Fx.First", "Both.lean", c.first)
			other := gateAddition(t, config, "second", "Fx.Second", "Both.lean", second)
			landed := ""
			for _, v := range []struct {
				verdict conceptgate.Verdict
				source  string
				lands   bool
			}{{first, c.first, c.landFirst}, {other, second, c.landSecond}} {
				writeTestFile(t, filepath.Join(tree, filepath.FromSlash(v.verdict.Addition.Path)), v.verdict.Addition.Content)
				if v.lands {
					landed += v.source + "\n"
				}
			}
			if landed != "" {
				writeTestFile(t, filepath.Join(tree, "Both.lean"), landed)
				if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Both"); err != nil {
					t.Fatal(err)
				}
			}
			r := checkRegistry(t, config, land(t, config, tree, "land Fx.First and Fx.Second"))
			added := []string{first.Addition.Path, other.Addition.Path}
			sort.Strings(added)
			wantID := "recheck-" + first.CompiledHash + "-" + other.CompiledHash
			if r.code != 1 || !slices.Equal(r.added, added) || len(r.defects) != 0 || r.unable != "" || len(r.ops) != 1 ||
				r.ops[0]["id"] != wantID {
				t.Fatalf("want exit 1 with %v added, no defect or cause, and the one near pair %s, got %+v", added, wantID, r)
			}
		})
	}
}

// The recheck of an addition reads the concept's declaration in the module of its home file when the landed build
// has one, and compiles the stored source only when it has none:
//   - landed in an existing home file that the stored source imports, it is checked from the landed constant, which
//     the stored source could no longer compile beside;
//   - landed with another statement than the one the gate added, it is a defect of the concept;
//   - not landed, the stored source is compiled, and the forms import no module whose imports declare the concept,
//     here a registry import that imports a file that declares it outside its home.
func TestLedgerRechecksAnAdditionFromItsLandedDeclaration(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	ctx := context.Background()
	compile := func(t *testing.T, lean declindex.Lean, tree string, modules ...string) {
		t.Helper()
		for _, m := range modules {
			if err := declindex.CompileModule(ctx, lean, nil, tree, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("landed in a home file that the stored source imports", func(t *testing.T) {
		const lib = "import Fixture\n\ndef Fx.helper (n : Nat) : Nat := n + 1\n"
		config, tree, lean := leanRegistryTree(t, "Lib")
		writeTestFile(t, filepath.Join(tree, "Lib.lean"), lib)
		compile(t, lean, tree, "Lib")
		land(t, config, tree, "add Lib")
		source := "import Lib\n\ndef Fx.Lib (n : Nat) : Prop := Fx.helper n < 5\n"
		addition := *gateAddition(t, config, "lib", "Fx.Lib", "Lib.lean", source).Addition
		writeTestFile(t, filepath.Join(tree, filepath.FromSlash(addition.Path)), addition.Content)
		writeTestFile(t, filepath.Join(tree, "Lib.lean"), lib+"\ndef Fx.Lib (n : Nat) : Prop := Fx.helper n < 5\n")
		compile(t, lean, tree, "Lib")
		r := checkRegistry(t, config, land(t, config, tree, "land Fx.Lib in Lib.lean"))
		if r.code != 0 || !slices.Equal(r.added, []string{addition.Path}) || len(r.defects) != 0 || r.unable != "" {
			t.Fatalf("want exit 0 with %s added and no defect or cause, got %+v", addition.Path, r)
		}
	})
	t.Run("landed with another statement", func(t *testing.T) {
		config, tree, lean := leanRegistryTree(t, "Fresh")
		source := "import Fixture\n\ndef Fx.Fresh (n : Nat) : Prop := n < 3\n"
		addition := *gateAddition(t, config, "fresh", "Fx.Fresh", "Fresh.lean", source).Addition
		writeTestFile(t, filepath.Join(tree, filepath.FromSlash(addition.Path)), addition.Content)
		writeTestFile(t, filepath.Join(tree, "Fresh.lean"), strings.Replace(source, "n < 3", "n < 4", 1))
		compile(t, lean, tree, "Fresh")
		r := checkRegistry(t, config, land(t, config, tree, "land Fx.Fresh with another statement"))
		if r.code != 1 || len(r.defects) != 1 || !strings.Contains(r.defects[0], "the landed declaration is not the one the gate added") ||
			r.unable != "" {
			t.Fatalf("want exit 1 with the defect that the landed declaration is not the added one, got %+v", r)
		}
	})
	t.Run("not landed in its home file", func(t *testing.T) {
		config, tree, lean := leanRegistryTree(t, "Other", "Wrapper")
		writeTestFile(t, filepath.Join(tree, "Wrapper.lean"), "import Fixture\n")
		writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\nimports: [Wrapper]\n")
		// A concept with a written form, so that the forms module is built and imports Wrapper.
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Fx.SpAligned.yaml"), "name: Fx.SpAligned\n"+
			"home: Fixture.lean\nmeaning: Register x2 of a register file is 8-aligned.\nforms:\n"+
			"  - \"fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64\"\n")
		compile(t, lean, tree, "Wrapper")
		land(t, config, tree, "import Wrapper in the forms")
		source := "import Fixture\n\ndef Fx.Fresh (n : Nat) : Prop := n < 3\n"
		addition := *gateAddition(t, config, "fresh", "Fx.Fresh", "Fresh.lean", source).Addition
		writeTestFile(t, filepath.Join(tree, filepath.FromSlash(addition.Path)), addition.Content)
		writeTestFile(t, filepath.Join(tree, "Other.lean"), source)
		writeTestFile(t, filepath.Join(tree, "Wrapper.lean"), "import Other\n")
		compile(t, lean, tree, "Other", "Wrapper")
		r := checkRegistry(t, config, land(t, config, tree, "land Fx.Fresh outside its home file"))
		if r.code != 0 || !slices.Equal(r.added, []string{addition.Path}) || len(r.defects) != 0 || r.unable != "" {
			t.Fatalf("want exit 0 with %s added and no defect or cause, got %+v", addition.Path, r)
		}
	})
}
