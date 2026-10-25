package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/conceptgate"
	"github.com/sjawhar/legion/orchestration/declindex"
)

// formsClashTree is a Git tree, configured with the compiled part and refreshed by the caller, whose registry's forms
// import Home. A and B each declare the theorem `dup` with one statement, and Home imports both: Lean merges them.
// UseA imports A only and UseB B only. C declares a definition `dup`, which Lean cannot merge with a theorem, and
// UseC imports C. Each Use module states the fact of the concept Reflexive, whose form is `fun (n : Nat) => n = n`.
func formsClashTree(t *testing.T) (config, tree string, lean declindex.Lean) {
	t.Helper()
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree = filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "theorem dup : (2 : Nat) ≤ 3 := by decide\n"},
		{"B", "theorem dup : (2 : Nat) ≤ 3 := by decide\n"},
		{"Home", "import A\nimport B\ntheorem Reflexive (n : Nat) : n = n := rfl\n"},
		{"UseA", "import A\ntheorem useA (k : Nat) : k = k := rfl\n"},
		{"UseB", "import B\ntheorem useB (k : Nat) : k = k := rfl\n"},
		{"C", "def dup : Nat := 3\n"},
		{"UseC", "import C\ntheorem useC (k : Nat) : k = k := rfl\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Reflexive.yaml"), "name: Reflexive\nhome: Home.lean\n"+
		"meaning: A natural number equals itself.\nforms:\n  - \"fun (n : Nat) => n = n\"\n")
	config = filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B, Home, UseA, UseB, C, UseC]\n"+
		"registry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-qm", "forms clash fixture")
	return config, tree, lean
}

// The index defect that refresh reports for the fixture: C and UseC each import C's `dup`, and the forms' imports
// declare `dup` only in A and B.
const (
	withoutFormsCount = "declindex: index defect: 2 modules are read without the registry's forms and state no fact of a written form"
	withoutFormsC     = "declindex: index defect: C is read without the registry's forms: its imports declare dup in C, and the forms' imports declare it in A, B"
	withoutFormsUseC  = "declindex: index defect: UseC is read without the registry's forms: its imports declare dup in C, and the forms' imports declare it in A, B"
)

// A module whose imports hold one of the declarations of a name that the forms' imports hold is read with the forms:
// UseA and UseB state Reflexive's fact. C and UseC, whose `dup` the forms' imports do not hold, are read without the
// forms. Refresh exits 0 and names each of them and its clash as an index defect, and it names them again after a
// refresh that reads only another module. `status` lists them, and `states` gives their count.
func TestRefreshReadsADuplicateThatTheFormsImportsHoldWithTheForms(t *testing.T) {
	requireLake(t)
	config, tree, lean := formsClashTree(t)
	checkDefect := func(label string, r result) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: want exit 0, got %+v", label, r)
		}
		for _, want := range []string{withoutFormsCount, withoutFormsC, withoutFormsUseC} {
			if !strings.Contains(r.errOut, want+"\n") {
				t.Errorf("%s: stderr lacks %q:\n%s", label, want, r.errOut)
			}
		}
	}
	checkDefect("refresh", invoke(t, "--config", config, "refresh"))

	var states map[string][]struct {
		Declaration string `json:"declaration"`
		Module      string `json:"declaration_module"`
	}
	r := invoke(t, "--config", config, "--json", "states", "Reflexive")
	decode(t, r, &states)
	var got []string
	for _, s := range states["Reflexive"] {
		if d := s.Module + "." + s.Declaration; !slices.Contains(got, d) {
			got = append(got, d)
		}
	}
	slices.Sort(got)
	if want := []string{"Home.Reflexive", "UseA.useA", "UseB.useB"}; !slices.Equal(got, want) {
		t.Errorf("states Reflexive: declarations %q, want %q (UseC is read without the forms)", got, want)
	}
	if !strings.Contains(r.errOut, withoutFormsCount) {
		t.Errorf("states Reflexive: stderr lacks %q:\n%s", withoutFormsCount, r.errOut)
	}

	var st struct {
		WithoutForms []struct {
			Module          string   `json:"module"`
			Name            string   `json:"name"`
			DeclaredIn      []string `json:"declared_in"`
			FormsDeclaredIn []string `json:"forms_declared_in"`
		} `json:"without_forms"`
	}
	sr := invoke(t, "--config", config, "--json", "status")
	decode(t, sr, &st)
	// The JSON mode names the index defect on stderr as the text mode does.
	for _, want := range []string{withoutFormsCount, withoutFormsC, withoutFormsUseC} {
		if !strings.Contains(sr.errOut, want+"\n") {
			t.Errorf("status --json: stderr lacks %q:\n%s", want, sr.errOut)
		}
	}
	if len(st.WithoutForms) != 2 {
		t.Fatalf("status --json: without_forms %+v, want C's and UseC's clash on dup", st.WithoutForms)
	}
	for i, module := range []string{"C", "UseC"} {
		w := st.WithoutForms[i]
		if w.Module != module || w.Name != "dup" || !slices.Equal(w.DeclaredIn, []string{"C"}) ||
			!slices.Equal(w.FormsDeclaredIn, []string{"A", "B"}) {
			t.Errorf("status --json: without_forms[%d] = %+v, want %s: dup declared in C, in the forms' imports in A and B", i, w, module)
		}
	}

	// A later refresh reads UseA alone; C and UseC are still read without the forms, and refresh still names them.
	writeTestFile(t, filepath.Join(tree, "UseA.lean"), "import A\ntheorem useA (k : Nat) : k = k := rfl\ntheorem useA2 : True := trivial\n")
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "UseA"); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "commit", "-qam", "change UseA")
	checkDefect("refresh after UseA changed", invoke(t, "--config", config, "refresh"))
}

// Two axioms of one name whose type Lean finds a proposition (`axiom dup : True`) import together in any order: Both,
// which imports Home (A's axiom) and UseB (B's), compiles. So UseB is read with the forms, which import Home, and
// states Reflexive's fact; refresh names no index defect.
func TestRefreshReadsWithTheFormsAModuleWhosePropositionAxiomTheFormsImportsDeclareElsewhere(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "axiom dup : True\n"},
		{"B", "axiom dup : True\n"},
		{"Home", "import A\ntheorem Reflexive (n : Nat) : n = n := rfl\n"},
		{"UseB", "import B\ntheorem useB (n : Nat) : n = n := rfl\n"},
		{"Both", "import Home\nimport UseB\nexample : True := dup\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatalf("compile %s: %v", m.name, err)
		}
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Reflexive.yaml"), "name: Reflexive\nhome: Home.lean\n"+
		"meaning: A natural number equals itself.\nforms:\n  - \"fun (n : Nat) => n = n\"\n")
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B, Home, UseB, Both]\nregistry: registry\n"+
		"lean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-qm", "proposition axioms")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 || strings.Contains(r.errOut, "index defect") {
		t.Fatalf("refresh: want exit 0 and no index defect, got %+v", r)
	}
	var states map[string][]struct {
		Declaration string `json:"declaration"`
		Module      string `json:"declaration_module"`
	}
	decode(t, invoke(t, "--config", config, "--json", "states", "Reflexive"), &states)
	var got []string
	for _, s := range states["Reflexive"] {
		if d := s.Module + "." + s.Declaration; !slices.Contains(got, d) {
			got = append(got, d)
		}
	}
	slices.Sort(got)
	if want := []string{"Home.Reflexive", "UseB.useB"}; !slices.Equal(got, want) {
		t.Errorf("states Reflexive: declarations %q, want %q", got, want)
	}
}

// Lean merges two axioms of one name only when a cheap check finds their type a proposition, which it does not for
// `∀ (p : Nat → Prop) (n : Nat), p n → p n`, and a theorem of that type subsumes each axiom. Home imports Thm's
// theorem `dup` and then A's and B's axiom `dup`, and builds; an environment that imports A and B before Thm does
// not. Refresh reads A, B and Thm without the forms, each alone, and names them; Home, whose declarations of `dup`
// are the forms' own, is read with them.
func TestRefreshKeepsApartTwoAxiomsThatOnlyATheoremReconciles(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "axiom dup : ∀ (p : Nat → Prop) (n : Nat), p n → p n\n"},
		{"B", "axiom dup : ∀ (p : Nat → Prop) (n : Nat), p n → p n\n"},
		{"Thm", "theorem dup : ∀ (p : Nat → Prop) (n : Nat), p n → p n := fun _ _ h => h\n"},
		{"Home", "import Thm\nimport A\nimport B\ntheorem Reflexive (n : Nat) : n = n := rfl\n"},
	} {
		writeTestFile(t, filepath.Join(tree, m.name+".lean"), m.source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, m.name); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Reflexive.yaml"), "name: Reflexive\nhome: Home.lean\n"+
		"meaning: A natural number equals itself.\nforms:\n  - \"fun (n : Nat) => n = n\"\n")
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B, Thm, Home]\nregistry: registry\n"+
		"lean:\n  export: true\n  jobs: 1\n")
	r := invoke(t, "--config", config, "refresh")
	if r.code != 0 || !strings.Contains(r.errOut, "declindex: index defect: 3 modules are read without the registry's forms") {
		t.Fatalf("refresh: want exit 0 and A, B and Thm named as read without the forms, got %+v", r)
	}
	// Thm's theorem is one of the forms' declarations of dup, so it is on both sides of its clash.
	const thm = "declindex: index defect: Thm is read without the registry's forms: its imports declare dup in Thm, and the forms' imports declare it in A, B, Thm\n"
	if !strings.Contains(r.errOut, thm) {
		t.Errorf("refresh: stderr lacks %q:\n%s", thm, r.errOut)
	}
	var st struct {
		WithoutForms []struct {
			Module string `json:"module"`
		} `json:"without_forms"`
	}
	decode(t, invoke(t, "--config", config, "--json", "status"), &st)
	var got []string
	for _, w := range st.WithoutForms {
		got = append(got, w.Module)
	}
	if want := []string{"A", "B", "Thm"}; !slices.Equal(got, want) {
		t.Errorf("status --json: modules read without the forms %q, want %q", got, want)
	}
}

// What the compiled part read with the forms depends on the forms' whole import closure, not only on the forms'
// text. A and B each declare a definition `dup`, which Lean cannot import together. Home, which the forms import,
// first imports A, and M, which imports A, is read with the forms and states Reflexive's fact. Then Home imports B
// instead, with the same forms text, and M and A, which did not change, no longer load with the forms: the next
// refresh reads them again, without the forms, and names them, and `states` no longer gives M's statement.
func TestRefreshReadsAgainWhenTheFormsImportsChange(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	compile := func(name, source string) {
		t.Helper()
		writeTestFile(t, filepath.Join(tree, name+".lean"), source)
		if err := declindex.CompileModule(ctx, lean, nil, tree, name); err != nil {
			t.Fatal(err)
		}
	}
	compile("A", "def dup : Nat := 3\n")
	compile("B", "def dup : Nat := 4\n")
	compile("Home", "import A\ntheorem Reflexive (n : Nat) : n = n := rfl\n")
	compile("M", "import A\ntheorem useM (k : Nat) : k = k := rfl\n")
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	writeTestFile(t, filepath.Join(tree, "registry", "concepts", "Reflexive.yaml"), "name: Reflexive\nhome: Home.lean\n"+
		"meaning: A natural number equals itself.\nforms:\n  - \"fun (n : Nat) => n = n\"\n")
	config := filepath.Join(dir, "index.yaml")
	writeIndexConfig(t, config, "tree: tree\nlib: .\nindex: idx\nstate: state\nroots: [A, B, Home, M]\nregistry: registry\n"+
		"lean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-qm", "forms import A")
	stating := func(label string) []string {
		t.Helper()
		var states map[string][]struct {
			Declaration string `json:"declaration"`
			Module      string `json:"declaration_module"`
		}
		decode(t, invoke(t, "--config", config, "--json", "states", "Reflexive"), &states)
		var got []string
		for _, s := range states["Reflexive"] {
			if d := s.Module + "." + s.Declaration; !slices.Contains(got, d) {
				got = append(got, d)
			}
		}
		slices.Sort(got)
		return got
	}
	apart := func() []string {
		t.Helper()
		var st struct {
			WithoutForms []struct {
				Module string `json:"module"`
			} `json:"without_forms"`
		}
		decode(t, invoke(t, "--config", config, "--json", "status"), &st)
		var got []string
		for _, w := range st.WithoutForms {
			if !slices.Contains(got, w.Module) {
				got = append(got, w.Module)
			}
		}
		return got
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	if got, want := stating("first"), []string{"Home.Reflexive", "M.useM"}; !slices.Equal(got, want) {
		t.Fatalf("states Reflexive with the forms importing A: %q, want %q", got, want)
	}
	if got := apart(); !slices.Equal(got, []string{"B"}) {
		t.Fatalf("modules without the forms importing A: %q, want [B]", got)
	}
	compile("Home", "import B\ntheorem Reflexive (n : Nat) : n = n := rfl\n")
	git(t, tree, "commit", "-qam", "forms import B")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh after Home imports B: %+v", r)
	}
	if got, want := stating("after"), []string{"Home.Reflexive"}; !slices.Equal(got, want) {
		t.Errorf("states Reflexive after Home imports B: %q, want %q (M no longer loads with the forms)", got, want)
	}
	if got := apart(); !slices.Equal(got, []string{"A", "M"}) {
		t.Errorf("modules without the forms after Home imports B: %q, want [A M]", got)
	}
}

// The gate reads the index's facts, so its verdict gives the count of the modules read without the registry's
// forms, and its stderr names the index defect.
func TestTheGateGivesTheCountOfModulesReadWithoutTheForms(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	config, _, _ := formsClashTree(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(t.TempDir(), "fresh.json")
	writeTestFile(t, proposal, `{"id":"fresh","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"Seven."},`+
		`"source":"def Fresh : Nat := 7\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	var v struct {
		Outcome      conceptgate.Outcome `json:"outcome"`
		WithoutForms int                 `json:"modules_without_forms"`
	}
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || r.code != 0 || v.Outcome != conceptgate.Added {
		t.Fatalf("gate a fresh concept: want exit 0, added, got %+v (%v)", r, err)
	}
	if v.WithoutForms != 2 {
		t.Errorf("gate verdict: modules_without_forms %d, want 2 (C and UseC)", v.WithoutForms)
	}
	if !strings.Contains(r.errOut, withoutFormsCount) {
		t.Errorf("gate: stderr lacks %q:\n%s", withoutFormsCount, r.errOut)
	}
}
