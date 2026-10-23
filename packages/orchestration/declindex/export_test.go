package declindex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
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

// Code that a confined compile runs (here an `#eval`) cannot write outside its scratch directory, read the caller's
// home, start another program, or start Lean itself again.
func TestCompileConfinedKeepsTheModulesCodeIn(t *testing.T) {
	l := leanForTest(t)
	if err := CheckSandbox(context.Background(), l); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir, err := NewScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "written")
	spawn := func(cmd string) string {
		return "  let r ← (IO.Process.output { cmd := \"" + cmd + "\", args := #[\"--version\"] }).toBaseIO\n" +
			"  if let .ok o := r then if o.exitCode == 0 then throw (IO.userError s!\"ran " + cmd + ": {o.stdout}\")\n"
	}
	probe := "def probe : IO Unit := do\n" +
		"  try IO.FS.writeFile \"" + target + "\" \"x\" catch _ => pure ()\n" +
		"  let home ← (IO.FS.readFile \"" + filepath.Join(os.Getenv("HOME"), ".bashrc") + "\").toBaseIO\n" +
		"  if home matches .ok _ then throw (IO.userError \"read the home directory\")\n" +
		spawn("/bin/sh") + spawn(l.bin("lean")) +
		"  IO.FS.writeFile \"inside\" \"x\"\n" +
		"#eval probe\n"
	if err := CompileConfined(context.Background(), l, nil, dir, "Probe", []byte(probe)); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the confined module wrote outside its directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "inside")); err != nil {
		t.Fatalf("the confined module could not write in its own directory: %v", err)
	}
	if err := CompileConfined(context.Background(), l, nil, dir, "Probe", []byte(probe)); err == nil {
		t.Fatal("a scratch directory that a run already used was accepted")
	}
}

// Lean's own parser decides what a proposal holds: text that hides a command from a text filter does not hide it
// from the parser.
func TestCheckSourceAcceptsOnlyDeclarations(t *testing.T) {
	l := leanForTest(t)
	if err := CheckSandbox(context.Background(), l); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	dir, _ := compiledFixture(t, l)
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l)}
	check := func(body string) error {
		t.Helper()
		work, err := NewScratch(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return x.CheckSource(context.Background(), []byte("import Fixture\n"+body), []string{dir}, work)
	}
	for _, ok := range []string{
		"namespace Fx\nopen Fx\n/-- doc -/\n@[simp] def MySp (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64\nend Fx\n",
		"structure B where\n  pre : Nat\ntheorem t : True := by trivial\n",
		"attribute [local instance] Fx.SpAligned\nattribute [-simp] Fx.inline\n",
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
		"macro \"m\" : term => `(1)\n",
	} {
		if err := check(bad); !errors.Is(err, ErrRefusedSource) {
			t.Errorf("%q: %v, want a refusal", bad, err)
		}
	}
}
