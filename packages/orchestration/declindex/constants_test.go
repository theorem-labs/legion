package declindex

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// writeConstants writes consts as the compiled part of modules, as a compiled step does.
func writeConstants(t *testing.T, s *Store, modules []string, consts []Constant) {
	t.Helper()
	ctx := context.Background()
	if err := s.writeTx(ctx, nil, func(tx *sql.Tx) error {
		return s.writeCompiled(ctx, tx, compiledRead{read: modules, consts: consts, digest: "test", commit: goldenCommit})
	}); err != nil {
		t.Fatal(err)
	}
}

// ConstantsIn gives the compiled rows of modules, and ConstantsConcluding the rows whose conclusion head is one of
// heads: each as the exporter gave it, without its atoms and facts, sorted by module and name.
func TestConstantsReadTheCompiledRows(t *testing.T) {
	s := refreshedGolden(t)
	ctx := context.Background()
	frameAl := Constant{Name: "Gold.frame_al", Module: "Golden", Kind: "theorem", Type: "Gold.Frame sp → Gold.Aligned sp",
		Hash: "h1", Binders: []string{"Gold.Frame"}, Conclusion: "Gold.Aligned"}
	frame := Constant{Name: "Gold.Frame", Module: "Golden", Kind: "inductive", Type: "BitVec 64 → Prop", Hash: "h2",
		Binders: []string{"BitVec"}}
	withStr := Constant{Name: "Gold.withStr", Module: "Golden", Kind: "def", Type: "optParam String \"a\" → String",
		Hash: "h3", Binders: []string{"String"}, Conclusion: "String"}
	box := Constant{Name: "Gold.Box", Module: "Golden", Kind: "inductive", Type: "Type", Hash: "h4", Binders: []string{}}
	frameAtAl := Constant{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Kind: "theorem", Type: "…", Hash: "h5",
		Binders: []string{"BitVec", "Gold2.FrameAt"}, Conclusion: "Gold.Aligned"}
	written := []Constant{frameAtAl, withStr, frameAl, box, frame}
	written[2].Atoms, written[2].Facts = []string{"atom"}, []Fact{{Concept: "C", How: "direct"}}
	writeConstants(t, s, []string{"Golden", "Sub.Wrap"}, written)

	in, err := s.ConstantsIn(ctx, []string{"Sub.Wrap", "Golden", "Sub.Wrap"})
	if want := []Constant{box, frame, frameAl, withStr, frameAtAl}; err != nil || !reflect.DeepEqual(in, want) {
		t.Fatalf("ConstantsIn = %+v, %v; want %+v", in, err, want)
	}
	if none, err := s.ConstantsIn(ctx, []string{"Nowhere"}); err != nil || len(none) != 0 {
		t.Fatalf("ConstantsIn(a module without constants) = %+v, %v; want none", none, err)
	}
	concluding, err := s.ConstantsConcluding(ctx, []string{"Gold.Aligned", "String", "Gold.Aligned", "Nowhere"})
	if want := []Constant{frameAl, withStr, frameAtAl}; err != nil || !reflect.DeepEqual(concluding, want) {
		t.Fatalf("ConstantsConcluding = %+v, %v; want %+v", concluding, err, want)
	}
}

// builtGolden compiles testdata/golden with Lean into a fresh directory that is the source tree and the library
// directory of a built tree, and gives the exporter that reads it.
func builtGolden(t *testing.T) (Tree, *Exporter) {
	t.Helper()
	l := leanForTest(t)
	tree := goldenTree(t)
	for _, rel := range []string{"Golden", "Sub/Wrap"} {
		if err := CompileModule(context.Background(), l, nil, tree.Lib, rel); err != nil {
			t.Fatal(err)
		}
	}
	return tree, &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{tree.Lib}, Jobs: 1}
}

// byModuleAndName sorts constants as the index's reads give them, and drops what those reads do not give.
func byModuleAndName(consts []Constant) []Constant {
	out := slices.Clone(consts)
	for i := range out {
		out[i].Atoms, out[i].Facts, out[i].Truncated = nil, nil, false
	}
	slices.SortFunc(out, func(a, b Constant) int { return cmp.Or(cmp.Compare(a.Module, b.Module), cmp.Compare(a.Name, b.Name)) })
	return out
}

// The exporter gives each constant the heads of its explicit binders' types and of its conclusion, read from the
// elaborated type: full names, past a binder type's own binders and a default value's or tactic's annotation, and
// empty where the head is not a constant.
func TestExporterGivesTheHeadsOfBindersAndConclusion(t *testing.T) {
	tree, x := builtGolden(t)
	consts, err := x.Run(context.Background(), Manifest{Modules: []string{"Golden", "Sub.Wrap"}, Roots: slices.Sorted(maps.Keys(tree.Roots)), Types: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Constant{}
	for _, c := range consts {
		got[c.Name] = c
	}
	for name, want := range map[string]struct {
		binders    []string
		conclusion string
	}{
		"Gold.frame_al":     {[]string{"Gold.Frame"}, "Gold.Aligned"},
		"Gold.frame_lo":     {[]string{"BitVec", "Gold.Frame"}, "GE.ge"},
		"Gold.instFrameDec": {[]string{"BitVec"}, "Decidable"},
		"Gold.withStr":      {[]string{"String"}, "String"},
		"Gold.Aligned":      {[]string{"BitVec"}, ""},
		"Gold.Frame":        {[]string{"BitVec"}, ""},
		"Gold.Frame.mk":     {[]string{"Gold.Aligned", "GE.ge"}, "Gold.Frame"},
		"Gold.Frame.al":     {[]string{"Gold.Frame"}, "Gold.Aligned"},
		"Gold.Box":          {[]string{}, ""},
		"Gold.Box.mk":       {[]string{"Nat", "GT.gt"}, "Gold.Box"},
		"Gold.Box.val":      {[]string{"Gold.Box"}, "Nat"},
		"Gold.uses_hidden":  {[]string{}, "Eq"},
		"Gold.astral":       {[]string{"Nat", "Eq"}, "Eq"},
		"Gold.box_pos":      {[]string{"Gold.Box"}, "GT.gt"},
		"Gold.SpWord":       {[]string{"BitVec"}, "BitVec"},
		"Gold.pick":         {[]string{"Nat"}, "Nat"},
		"Gold2.FrameAt":     {[]string{"BitVec"}, ""},
		"Gold2.frameAt_al":  {[]string{"BitVec", "Gold2.FrameAt"}, "Gold.Aligned"},
		"Gold2.frameAt_use": {[]string{"BitVec", "Gold2.FrameAt"}, "True"},
		"Gold2.only_proof":  {[]string{}, "LE.le"},
	} {
		c, ok := got[name]
		if !ok {
			t.Errorf("the exporter gave no constant %s", name)
			continue
		}
		if !reflect.DeepEqual(c.Binders, want.binders) || c.Conclusion != want.conclusion {
			t.Errorf("%s: binders %q, conclusion %q; want %q, %q", name, c.Binders, c.Conclusion, want.binders, want.conclusion)
		}
	}
}

// A module that a graph check compiles into a scratch directory, against the library directory of a built tree,
// gives the exporter the rows that the index's compiled part holds for that module: the candidate side and the
// base side of a check read one thing.
func TestAModuleCompiledInScratchGivesTheRowsTheIndexHolds(t *testing.T) {
	tree, x := builtGolden(t)
	ctx := context.Background()
	s := openStore(t)
	co := &CompiledOptions{Exporter: x, Dir: t.TempDir()}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: goldenCommit, Compiled: compiledWith(co)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompiledCommit(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Golden", "Sub/Wrap"} {
		module := strings.ReplaceAll(rel, "/", ".")
		scratch := t.TempDir()
		src, err := os.ReadFile(filepath.Join(tree.Source, filepath.FromSlash(rel)+".lean"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(scratch, filepath.FromSlash(rel))), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, filepath.FromSlash(rel)+".lean"), src, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CompileModule(ctx, x.Lean, []string{tree.Lib}, scratch, rel); err != nil {
			t.Fatal(err)
		}
		sx := *x
		sx.SearchPath = []string{scratch, tree.Lib}
		exported, err := sx.Run(ctx, Manifest{Modules: []string{module}, Roots: slices.Sorted(maps.Keys(tree.Roots)), Types: true})
		if err != nil {
			t.Fatal(err)
		}
		held, err := s.ConstantsIn(ctx, []string{module})
		if err != nil {
			t.Fatal(err)
		}
		if len(held) == 0 {
			t.Fatalf("the index holds no compiled constant of %s", module)
		}
		if got := byModuleAndName(exported); !reflect.DeepEqual(got, held) {
			t.Fatalf("%s compiled in scratch gives\n%+v\nthe index holds\n%+v", module, got, held)
		}
	}
}
