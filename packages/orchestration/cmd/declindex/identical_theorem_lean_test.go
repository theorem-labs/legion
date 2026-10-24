package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Acceptance red test. Lean imports two theorems of one name and one statement together wherever they come from, so a
// module whose imports hold one copy of such a theorem while the forms' imports hold the other copy is no clash. On
// the project of the acceptance run, four names are declared that way: each pair is two theorems with one compiled
// statement (one of them a Prop-valued instance). An index of legion#4 e5f5309f read the 123 modules that import the
// other copy with the forms; one of 3acf2834 reads them without the forms and reports each as an index defect, and
// `states` loses 16 of the 23,095 rows that the format-1 run gives for one concept.
//
// Here the forms' imports (Home) hold A's theorem `dup`; B declares the same theorem, and UseB imports B. Both, which
// imports Home and UseB, compiles: Lean merges the two `dup`. UseB must be read with the forms. C's definition `dup`
// and D's theorem `dup` with another statement are real clashes: UseC and UseD stay without the forms.
func TestRefreshReadsWithTheFormsAModuleWhoseTheoremTheFormsImportsDeclareIdenticallyElsewhere(t *testing.T) {
	requireLake(t)
	ctx := context.Background()
	lean, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, m := range []struct{ name, source string }{
		{"A", "theorem dup : (2 : Nat) ≤ 3 := by decide\n"},
		{"B", "theorem dup : (2 : Nat) ≤ 3 := by decide\n"},
		{"Home", "import A\ntheorem Reflexive (n : Nat) : n = n := rfl\n"},
		{"UseB", "import B\ntheorem useB (k : Nat) : k = k := rfl\n"},
		{"Both", "import Home\nimport UseB\ntheorem both : True := trivial\n"},
		{"C", "def dup : Nat := 3\n"},
		{"UseC", "import C\ntheorem useC (k : Nat) : k = k := rfl\n"},
		{"D", "theorem dup : (2 : Nat) ≤ 4 := by decide\n"},
		{"UseD", "import D\ntheorem useD (k : Nat) : k = k := rfl\n"},
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
	writeTestFile(t, config, "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [A, B, Home, UseB, Both, C, UseC, D, UseD]\n"+
		"registry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-qm", "identical theorem fixture")

	r := invoke(t, "--config", config, "refresh")
	if r.code != 0 {
		t.Fatalf("refresh: want exit 0, got %+v", r)
	}
	for _, line := range strings.Split(r.errOut, "\n") {
		if strings.Contains(line, "index defect: UseB ") || strings.Contains(line, "index defect: B ") {
			t.Errorf("refresh: %q; B's dup is A's theorem, which Lean imports with it (Both compiles)", line)
		}
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
		t.Errorf("states Reflexive: declarations %q, want %q (UseB is read with the forms; UseC and UseD are not)", got, want)
	}
	var st struct {
		WithoutForms []struct {
			Module string `json:"module"`
		} `json:"without_forms"`
	}
	decode(t, invoke(t, "--config", config, "--json", "status"), &st)
	var apart []string
	for _, w := range st.WithoutForms {
		if !slices.Contains(apart, w.Module) {
			apart = append(apart, w.Module)
		}
	}
	slices.Sort(apart)
	if want := []string{"C", "D", "UseC", "UseD"}; !slices.Equal(apart, want) {
		t.Errorf("status --json: modules without the forms %q, want %q", apart, want)
	}
}
