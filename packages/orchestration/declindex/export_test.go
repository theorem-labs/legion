package declindex

import (
	"context"
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
		Modules: []string{"Fixture"}, Imports: []string{FormsModuleName}, Roots: []string{"Fixture"},
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
