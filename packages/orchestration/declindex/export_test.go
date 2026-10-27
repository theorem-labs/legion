package declindex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// leanForTest finds a Lean toolchain on PATH. The exporter tests need one: CI installs it and sets
// DECLINDEX_REQUIRE_LEAN, which turns a missing toolchain into a failure; elsewhere they skip.
func leanForTest(t *testing.T) Lean {
	t.Helper()
	if _, err := exec.LookPath("lake"); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_LEAN") != "" {
			t.Fatal("DECLINDEX_REQUIRE_LEAN is set and no lake is on PATH")
		}
		t.Skip("no lake on PATH: the exporter tests need a Lean toolchain")
	}
	l, err := FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var (
	exporterOnce sync.Once
	exporterBin  string
	exporterErr  error
)

func exporterForTest(t *testing.T, l Lean) string {
	t.Helper()
	exporterOnce.Do(func() {
		exporterBin, exporterErr = BuildExporter(context.Background(), l, "", filepath.Join(os.TempDir(), "declindex-test-exporter"))
	})
	if exporterErr != nil {
		t.Fatal(exporterErr)
	}
	return exporterBin
}

// spForms are the forms of the fixture's concept: x2 aligned, written three ways.
var spForms = []string{
	"fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64",
	"fun (R : BitVec 5 → BitVec 64) (c : BitVec 64) => (R 2#5 + c) &&& 7#64 = 0#64",
	"fun (R : BitVec 5 → BitVec 64) => (R 2#5).toNat % 8 = 0",
}

// compiledFixture compiles testdata/lean/Fixture.lean and the forms module for it into a fresh directory.
func compiledFixture(t *testing.T, l Lean) (dir string, forms []ConstRef) {
	t.Helper()
	dir = t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Fixture.lean"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := CompileModule(ctx, l, nil, dir, "Fixture"); err != nil {
		t.Fatal(err)
	}
	copies, err := os.ReadFile(filepath.Join("testdata", "lean", "Copies.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Copies.lean"), copies, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(ctx, l, nil, dir, "Copies"); err != nil {
		t.Fatal(err)
	}
	fm := FormsModule{Imports: []string{"Fixture"}, Concepts: []ConceptForms{{Concept: "Fx.SpAligned", Forms: spForms}}}
	text, refs := fm.Source()
	if err := os.WriteFile(filepath.Join(dir, FormsModuleName+".lean"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(ctx, l, nil, dir, FormsModuleName); err != nil {
		t.Fatal(err)
	}
	return dir, refs
}

func exportFixture(t *testing.T) map[string]Constant {
	t.Helper()
	l := leanForTest(t)
	dir, forms := compiledFixture(t, l)
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	consts, err := x.Run(context.Background(), Manifest{
		Modules: []string{"Fixture", "Copies"}, Imports: []string{FormsModuleName}, Roots: []string{"Fixture", "Copies"},
		Forms: forms, Concepts: []ConstRef{{Concept: "Fx.SpAligned", Const: "Fx.SpAligned"}}, Types: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Constant{}
	for _, c := range consts {
		out[c.Name] = c
	}
	return out
}

func howsOf(c Constant) []string {
	var out []string
	for _, f := range c.Facts {
		out = append(out, f.Concept+" "+f.How)
	}
	sort.Strings(out)
	return out
}

func TestExporterFindsTheFactHoweverItIsWritten(t *testing.T) {
	consts := exportFixture(t)
	const c = "Fx.SpAligned"
	want := map[string][]string{
		"Fx.inline":       {c + " direct"},
		"Fx.inline_proj":  {c + " direct"},
		"Fx.swapped":      {c + " direct"},
		"Fx.aliased":      {c + " direct"},
		"Fx.offset":       {c + " direct"},
		"Fx.mod8":         {c + " direct"},
		"Fx.via_frame":    {c + " via:Fx.Frame"},
		"Fx.via_frame2":   {c + " via:Fx.Frame2"},
		"Fx.via_wrap":     {c + " via:Fx.Wrap"},
		"Fx.via_inner":    {c + " via:Fx.Inner"},
		"Fx.uses_concept": {c + " uses", c + " via:Fx.SpAligned"},
		"Fx.NearSp":       {c + " direct"},
		"Fx.SpBox":        {c + " direct"},
		"Fx.SpBox.mk":     {c + " direct"},
		"Fx.other_reg":    nil,
		"Fx.other_word":   nil,
		"Fx.frame_other":  nil,
		"Fx.Frame":        nil,
		"Fx.Wrap":         nil,
	}
	for name, w := range want {
		got, ok := consts[name]
		if !ok {
			t.Errorf("%s was not exported", name)
			continue
		}
		if g := howsOf(got); !reflect.DeepEqual(g, w) {
			t.Errorf("%s: facts %v, want %v", name, g, w)
		}
	}
}

// A matcher is an auxiliary definition, not a declaration of its own: the exporter, which imports without loading
// environment extensions, finds it in its module's matcher entries and exports it with no row.
func TestExporterExportsNoMatcher(t *testing.T) {
	consts := exportFixture(t)
	if _, ok := consts["Fx.pickA"]; !ok {
		t.Fatal("Fx.pickA was not exported")
	}
	for _, m := range []string{"Fx.pickA.match_1", "Fx.pickB.match_1"} {
		if _, ok := consts[m]; ok {
			t.Errorf("the matcher %s was exported as a declaration", m)
		}
	}
}

// A toolchain process takes the thread count of the build that runs it (LEAN_NUM_THREADS), so that many of them
// together keep to the cores that build was given; nothing else of the caller's environment reaches it.
func TestLeanEnvPassesTheThreadCount(t *testing.T) {
	l := Lean{Prefix: t.TempDir()}
	t.Setenv("LEAN_NUM_THREADS", "3")
	if env := l.env(nil, t.TempDir()); !slices.Contains(env, "LEAN_NUM_THREADS=3") {
		t.Errorf("env with LEAN_NUM_THREADS=3 set = %v; want it among them", env)
	}
	t.Setenv("LEAN_NUM_THREADS", "")
	for _, v := range l.env(nil, t.TempDir()) {
		if strings.HasPrefix(v, "LEAN_NUM_THREADS=") {
			t.Errorf("env with LEAN_NUM_THREADS unset gave %q", v)
		}
	}
}

// A build passes its own options to each `lean` (a package's options, as `-Dname=value`): CompileModule gives them
// to Lean, so a module that builds only with them compiles here too.
func TestCompileModulePassesTheCallersLeanArguments(t *testing.T) {
	l := leanForTest(t)
	dir := t.TempDir()
	src := "theorem big : (List.replicate 2000 1).length = 2000 := by decide\n"
	if err := os.WriteFile(filepath.Join(dir, "Deep.lean"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := CompileModule(ctx, l, nil, dir, "Deep"); err == nil || !strings.Contains(err.Error(), "maximum recursion depth") {
		t.Fatalf("compile without the option: %v; want Lean's recursion depth error", err)
	}
	if err := CompileModule(ctx, l, nil, dir, "Deep", "-DmaxRecDepth=8192"); err != nil {
		t.Fatalf("compile with -DmaxRecDepth=8192: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Deep.olean")); err != nil {
		t.Fatal(err)
	}
}

func TestExporterHashesACopyAsTheOriginal(t *testing.T) {
	consts := exportFixture(t)
	orig := consts["Fx.SpAligned"]
	if orig.Hash == "" || len(orig.Hash) != 32 {
		t.Fatalf("compiled statement hash %q", orig.Hash)
	}
	for _, copy := range []string{"Fx.SpAlignedCopy", "Fx.SpAlignedRenamed"} {
		if consts[copy].Hash != orig.Hash {
			t.Errorf("%s hashes %s, the original %s", copy, consts[copy].Hash, orig.Hash)
		}
	}
	for _, pair := range [][2]string{{"Fx.pickA", "Fx.pickB"}, {"Fx.posA", "Fx.posB"}} {
		if a, b := consts[pair[0]].Hash, consts[pair[1]].Hash; a == "" || a != b {
			t.Errorf("%s hashes %s and its copy %s hashes %s: a declaration's own auxiliaries must not name it",
				pair[0], a, pair[1], b)
		}
	}
	if consts["Fx.Q"].Hash == "" || consts["Fx.Q"].Hash == consts["Fx.MyQ"].Hash {
		t.Errorf("Fx.Q and Fx.MyQ hash %s and %s: a sub-declaration the user wrote is hashed by its name",
			consts["Fx.Q"].Hash, consts["Fx.MyQ"].Hash)
	}
	if consts["Fx.NearSp"].Hash == orig.Hash {
		t.Error("a different statement hashed as the original")
	}
	if orig.Kind != "def" || consts["Fx.inline"].Kind != "theorem" || consts["Fx.SpBox"].Kind != "inductive" {
		t.Errorf("kinds: %s %s %s", orig.Kind, consts["Fx.inline"].Kind, consts["Fx.SpBox"].Kind)
	}
	if orig.Type == "" {
		t.Error("no printed type")
	}
}

func TestExporterAtomsAgreeAcrossSidesOfAnEquation(t *testing.T) {
	consts := exportFixture(t)
	shared := func(a, b string) bool {
		for _, x := range consts[a].Atoms {
			for _, y := range consts[b].Atoms {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	if !shared("Fx.inline", "Fx.swapped") {
		t.Errorf("`R 2#5 &&& 7 = 0` and `0 = R 2#5 &&& 7` share no atom: %v %v", consts["Fx.inline"].Atoms, consts["Fx.swapped"].Atoms)
	}
	if shared("Fx.inline", "Fx.other_reg") {
		t.Error("the atoms about x2 and x10 agree")
	}
}

// A relation atom counts an abstracted proof by its type, as the statement hash does. A's `one` and B's `two` each
// abstract the proof `0 < n + 1` out of their types (`one._proof_1`, `two._proof_1`), so every atom of `one`,
// `(⟨0, _⟩ : Fin (n + 1)).val = 0` among them, is an atom of `two`.
func TestAnAtomCountsAnAbstractedProofByItsType(t *testing.T) {
	l := leanForTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, m := range []struct{ name, source string }{
		{"A", "theorem one (n : Nat) : (⟨0, by omega⟩ : Fin (n + 1)).val = 0 := rfl\n"},
		{"B", "theorem two (n : Nat) : (⟨0, by omega⟩ : Fin (n + 1)).val = 0 ∧ True := ⟨rfl, trivial⟩\n"},
	} {
		if err := os.WriteFile(filepath.Join(dir, m.name+".lean"), []byte(m.source), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CompileModule(ctx, l, nil, dir, m.name); err != nil {
			t.Fatal(err)
		}
	}
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	consts, err := x.Run(ctx, Manifest{Modules: []string{"A", "B"}, Roots: []string{"A", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	atoms := map[string][]string{}
	for _, c := range consts {
		atoms[c.Name] = c.Atoms
	}
	if len(atoms["one"]) == 0 {
		t.Fatalf("one has no atom: %v", atoms)
	}
	for _, a := range atoms["one"] {
		if !slices.Contains(atoms["two"], a) {
			t.Errorf("one's atom %s is not an atom of two: one %v, two %v", a, atoms["one"], atoms["two"])
		}
	}
}

// A compiled module sees only the toolchain's environment: none of the caller's variables, and its own directory
// as home.
func TestCompileModuleRunsLeanInAMinimalEnvironment(t *testing.T) {
	l := leanForTest(t)
	dir := t.TempDir()
	t.Setenv("DECLINDEX_TEST_SECRET", "leaked")
	probe := "def probe : IO Unit := do\n" +
		"  if (← IO.getEnv \"DECLINDEX_TEST_SECRET\").isSome then throw (IO.userError \"the caller's environment leaked\")\n" +
		"  if (← IO.getEnv \"HOME\") != some \"" + dir + "\" then throw (IO.userError \"home is not the module's directory\")\n" +
		"#eval probe\n"
	if err := os.WriteFile(filepath.Join(dir, "Probe.lean"), []byte(probe), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(context.Background(), l, nil, dir, "Probe"); err != nil {
		t.Fatal(err)
	}
}

// Lean's own parser decides what a proposal holds: text that hides a command from a text filter does not hide it
// from the parser.
func TestCheckSourceAcceptsOnlyDeclarations(t *testing.T) {
	l := leanForTest(t)
	dir, _ := compiledFixture(t, l)
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l)}
	check := func(body string) error {
		t.Helper()
		return x.CheckSource(context.Background(), []byte("import Fixture\n"+body), []string{dir}, t.TempDir())
	}
	for _, ok := range []string{
		"namespace Fx\nopen Fx\n/-- doc -/\n@[simp] def MySp (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64\nend Fx\n",
		"structure B where\n  pre : Nat\ntheorem t : True := by trivial\n",
		"attribute [local instance] Fx.SpAligned\nattribute [-simp] Fx.inline\n",
		// The options that proofs set inside a term or a tactic, with a numeral or true/false.
		"theorem t : True := by\n  set_option maxRecDepth 2000 in\n  set_option maxHeartbeats 400000 in\n  set_option simprocs false in trivial\n",
		"def n : Nat := set_option synthInstance.maxHeartbeats 2000000 in set_option synthInstance.maxSize 1024 in 1\n",
		"theorem t : True := by\n  set_option pp.all true in\n  set_option linter.unusedVariables false in trivial\n",
	} {
		if err := check(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"def c : Char := '\"'\n#eval 1\n",
		"def e : Nat := by_elab return Lean.mkNatLit 1\n",
		"theorem t : True := by run_tac pure ()\n",
		"theorem t : 2 + 2 = 4 := by decide +native\n",
		"@[init] def x : IO Unit := pure ()\n",
		"attribute [init] Fx.SpAligned\n",
		"attribute [local instance, export x] Fx.SpAligned\n",
		"open Fx in\n#eval 1\n",
		"unsafe def x : Nat := 1\n",
		"initialize counter : IO.Ref Nat ← IO.mkRef 0\n",
		"builtin_initialize pure ()\n",
		"set_option maxHeartbeats 0\n",
		// An option set inside a term or a tactic that names a program or a path, lifts the heartbeat limit, or is
		// not on the allow list.
		"theorem t : True := by\n  set_option sat.solver \"/usr/bin/env\" in trivial\n",
		"def n : Nat := set_option sat.solver \"/usr/bin/env\" in 1\n",
		"theorem t : True := by\n  set_option trace.profiler.output \"/tmp/p\" in trivial\n",
		"theorem t : True := by\n  set_option maxHeartbeats 0 in trivial\n",
		"def n : Nat := set_option synthInstance.maxHeartbeats 0 in 1\n",
		"theorem t : True := by\n  set_option maxRecDepth \"9\" in trivial\n",
		"theorem t : True := by\n  set_option debug.skipKernelTC true in trivial\n",
		"macro \"m\" : term => `(1)\n",
	} {
		if err := check(bad); !errors.Is(err, ErrRefusedSource) {
			t.Errorf("%q: %v, want a refusal", bad, err)
		}
	}
}
