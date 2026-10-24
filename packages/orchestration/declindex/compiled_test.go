package declindex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// compiledTree is the Lean fixture as a built tree: Fixture.lean with its .ilean and .olean, in one directory
// that is both the source tree and the library directory.
func compiledTree(t *testing.T) (Tree, *CompiledOptions) {
	t.Helper()
	l := leanForTest(t)
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Fixture.lean"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(context.Background(), l, nil, dir, "Fixture"); err != nil {
		t.Fatal(err)
	}
	return Tree{Source: dir, Lib: dir, Roots: Roots{"Fixture": true}}, &CompiledOptions{
		Exporter: &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1},
		Forms:    FormsModule{Imports: []string{"Fixture"}, Concepts: []ConceptForms{{Concept: "Fx.SpAligned", Forms: spForms}}},
		Dir:      t.TempDir(),
	}
}

// compiledWith is the compiled step of a refresh with fixed options.
func compiledWith(co *CompiledOptions) func(context.Context) (*CompiledOptions, error) {
	return func(context.Context) (*CompiledOptions, error) { return co, nil }
}

func TestRefreshReadsTheCompiledModules(t *testing.T) {
	tree, co := compiledTree(t)
	s := openStore(t)
	ctx := context.Background()
	st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one", Compiled: compiledWith(co)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Exported != 1 {
		t.Fatalf("exported %d modules, want 1", st.Exported)
	}
	c, err := s.Constant(ctx, "Fx.SpAligned")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "def" || c.Type == "" || len(c.Hash) != 32 || c.Module != "Fixture" {
		t.Fatalf("constant %+v", c)
	}
	rows, err := s.States(ctx, "Fx.SpAligned")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Decl] = r.How
	}
	for _, name := range []string{"Fx.inline", "Fx.via_wrap", "Fx.SpBox", "Fx.NearSp"} {
		if got[name] == "" {
			t.Errorf("%s is not listed as stating the concept: %v", name, got)
		}
	}
	if got["Fx.other_word"] != "" || got["Fx.Frame"] != "" {
		t.Errorf("listed a statement of another fact: %v", got)
	}
	var spBox []string
	for _, r := range rows {
		if r.Decl == "Fx.SpBox" {
			spBox = append(spBox, r.Const)
		}
	}
	sort.Strings(spBox)
	if !reflect.DeepEqual(spBox, []string{"Fx.SpBox", "Fx.SpBox.al", "Fx.SpBox.mk"}) {
		t.Errorf("a constructor's fact is not its structure's: %v", spBox)
	}
	same, err := s.SameAtoms(ctx, "Fx.inline")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(same, "Fx.swapped") || contains(same, "Fx.other_reg") {
		t.Errorf("same atoms as Fx.inline: %v", same)
	}
	again, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one", Compiled: compiledWith(co)})
	if err != nil {
		t.Fatal(err)
	}
	if again.Exported != 0 {
		t.Fatalf("an unchanged build was exported again: %d", again.Exported)
	}
	co.Forms.Concepts[0].Forms = spForms[:1]
	changed, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one", Compiled: compiledWith(co)})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Exported != 1 {
		t.Fatalf("a changed registry did not export again: %d", changed.Exported)
	}
	rows, err = s.States(ctx, "Fx.SpAligned")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Decl == "Fx.mod8" {
			t.Fatalf("a form that the registry dropped still matches: %+v", r)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// A definition's facts pass to every statement that applies it. When a landing changes the definition, a module
// that applies it is matched again even if its own compiled module is byte for byte the same.
func TestRefreshRematchesTheStatementsAChangedDefinitionReaches(t *testing.T) {
	tree, co := compiledTree(t)
	l, ctx := co.Exporter.Lean, context.Background()
	user := "import Fixture\n\ntheorem user_frame (R : BitVec 5 → BitVec 64) (_h : Fx.Frame (R 2#5) 8) : True := trivial\n"
	if err := os.WriteFile(filepath.Join(tree.Source, "User.lean"), []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := func(rel, h string) {
		t.Helper()
		for _, ext := range []string{".ilean.hash", ".olean.hash"} {
			if err := os.WriteFile(filepath.Join(tree.Lib, rel+ext), []byte(h), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := CompileModule(ctx, l, nil, tree.Source, "User"); err != nil {
		t.Fatal(err)
	}
	stamp("Fixture", "a")
	stamp("User", "u")
	tree.Roots["User"] = true
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one", Compiled: compiledWith(co)}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Constant(ctx, "user_frame"); err != nil || len(c.Facts) != 1 || c.Facts[0].How != "via:Fx.Frame" {
		t.Fatalf("before the change: %+v %v", c, err)
	}
	src, _ := os.ReadFile(filepath.Join(tree.Source, "Fixture.lean"))
	changed := []byte(string(src[:0]) + strings.Replace(string(src), "n ≤ 64 ∧ sp &&& 7#64 = 0#64", "n ≤ 64 ∧ sp.toNat ≥ 0", 1))
	if err := os.WriteFile(filepath.Join(tree.Source, "Fixture.lean"), changed, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Fixture", "User"} {
		if err := CompileModule(ctx, l, nil, tree.Source, rel); err != nil {
			t.Fatal(err)
		}
	}
	stamp("Fixture", "b")
	stamp("User", "u")
	st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two", Compiled: compiledWith(co)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Exported != 1 || st.Rematched != 1 {
		t.Fatalf("exported %d and re-matched %d modules, want 1 and 1", st.Exported, st.Rematched)
	}
	if c, err := s.Constant(ctx, "user_frame"); err != nil || len(c.Facts) != 0 {
		t.Fatalf("after the change the user still states the fact: %+v %v", c, err)
	}
}

// A concept with no written form is matched by its declaration's body. When a landing changes that body, every
// module is matched again, also one that restates the old body without applying the concept.
func TestRefreshRematchesEverythingWhenAFormlessConceptsBodyChanges(t *testing.T) {
	tree, co := compiledTree(t)
	l, ctx := co.Exporter.Lean, context.Background()
	restate := "theorem restate (x : BitVec 64) (_h : 8 ≤ 64 ∧ x &&& 7#64 = 0#64) : True := trivial\n"
	if err := os.WriteFile(filepath.Join(tree.Source, "Restate.lean"), []byte(restate), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(ctx, l, nil, tree.Source, "Restate"); err != nil {
		t.Fatal(err)
	}
	stamp := func(rel, h string) {
		t.Helper()
		for _, ext := range []string{".ilean.hash", ".olean.hash"} {
			if err := os.WriteFile(filepath.Join(tree.Lib, rel+ext), []byte(h), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stamp("Fixture", "a")
	stamp("Restate", "r")
	tree.Roots["Restate"] = true
	co.Forms = FormsModule{Imports: []string{"Fixture"}, Concepts: []ConceptForms{{Concept: "Fx.Frame"}}}
	s := openStore(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one", Compiled: compiledWith(co)}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Constant(ctx, "restate"); err != nil || len(c.Facts) != 1 || c.Facts[0].Concept != "Fx.Frame" {
		t.Fatalf("before the change: %+v %v", c, err)
	}
	src, _ := os.ReadFile(filepath.Join(tree.Source, "Fixture.lean"))
	changed := strings.Replace(string(src), "n ≤ 64 ∧ sp &&& 7#64 = 0#64", "n ≤ 64 ∧ sp.toNat ≥ 0", 1)
	if err := os.WriteFile(filepath.Join(tree.Source, "Fixture.lean"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(ctx, l, nil, tree.Source, "Fixture"); err != nil {
		t.Fatal(err)
	}
	stamp("Fixture", "b")
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two", Compiled: compiledWith(co)}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Constant(ctx, "restate"); err != nil || len(c.Facts) != 0 {
		t.Fatalf("after the concept's body changed, the old restatement still matches: %+v %v", c, err)
	}
}
