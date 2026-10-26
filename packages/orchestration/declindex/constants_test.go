package declindex

import (
	"cmp"
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// writeConstants writes consts as the compiled part of modules, as a compiled step does.
func writeConstants(t *testing.T, s *Store, modules []string, consts []Constant) {
	t.Helper()
	ctx := context.Background()
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.writeTx(ctx, func(tx pgx.Tx) error {
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

// compileSource writes src as the module mod of dir and compiles it there with Lean.
func compileSource(t *testing.T, l Lean, dir, mod, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, mod+".lean"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CompileModule(context.Background(), l, nil, dir, mod); err != nil {
		t.Fatal(err)
	}
}

// Two modules that declare one theorem, one with its hypothesis explicit and one with it implicit, import together:
// Lean merges the two, since it compares their types without binder annotations, and keeps one of them. Each module's
// row still gives its own type and binders, so a module reads the same whether the exporter reads it alone, with the
// other, or as the index holds it.
func TestAMergedTheoremKeepsItsOwnModulesBinders(t *testing.T) {
	ctx := context.Background()
	l := leanForTest(t)
	dir := t.TempDir()
	compileSource(t, l, dir, "A", "theorem dup (h : True) : True := h\n")
	compileSource(t, l, dir, "B", "theorem dup {h : True} : True := h\n")
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	roots := []string{"A", "B"}
	var alone []Constant
	for _, mod := range roots {
		cs, err := x.Run(ctx, Manifest{Modules: []string{mod}, Roots: roots, Types: true})
		if err != nil {
			t.Fatal(err)
		}
		alone = append(alone, cs...)
	}
	alone = byModuleAndName(alone)
	if len(alone) != 2 || !reflect.DeepEqual(alone[0].Binders, []string{"True"}) || !reflect.DeepEqual(alone[1].Binders, []string{}) {
		t.Fatalf("A.dup and B.dup read alone = %+v; want binders [True] and []", alone)
	}
	together, err := x.Run(ctx, Manifest{Modules: roots, Roots: roots, Types: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := byModuleAndName(together); !reflect.DeepEqual(got, alone) {
		t.Errorf("read together:\n%+v\nread alone:\n%+v", got, alone)
	}
	s := openStore(t)
	tree := Tree{Source: dir, Lib: dir, Roots: Roots{"A": true, "B": true}}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: goldenCommit, Compiled: compiledWith(&CompiledOptions{Exporter: x, Dir: t.TempDir()})}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompiledCommit(ctx, true); err != nil {
		t.Fatal(err)
	}
	held, err := s.ConstantsIn(ctx, roots)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(held, alone) {
		t.Errorf("the index holds:\n%+v\nread alone:\n%+v", held, alone)
	}
}

// Metadata that an elaborator puts on a partly applied constant, inside an application, hides no head: the head is
// the constant at the bottom of the application, past metadata anywhere on the way.
func TestMetadataInsideAnApplicationHidesNoHead(t *testing.T) {
	ctx := context.Background()
	l := leanForTest(t)
	dir := t.TempDir()
	compileSource(t, l, dir, "Annotated", "import Lean\nopen Lean Elab Term\n"+
		"elab \"annotatedEq\" : term => do\n"+
		"  let e ← elabTerm (← `(Eq (0 : Nat))) none\n"+
		"  return mkMData (KVMap.empty.setBool `annotated true) e\n"+
		"theorem plainConclusion : (0 : Nat) = 0 := rfl\n"+
		"theorem annotatedConclusion : annotatedEq 0 := rfl\n"+
		"theorem annotatedBinder (h : annotatedEq 0) : True := trivial\n")
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	cs, err := x.Run(ctx, Manifest{Modules: []string{"Annotated"}, Roots: []string{"Annotated"}, Types: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Constant{}
	for _, c := range cs {
		got[c.Name] = c
	}
	for name, want := range map[string]struct {
		binders    []string
		conclusion string
	}{
		"plainConclusion":     {[]string{}, "Eq"},
		"annotatedConclusion": {[]string{}, "Eq"},
		"annotatedBinder":     {[]string{"Eq"}, "True"},
	} {
		c, ok := got[name]
		if !ok {
			t.Errorf("the exporter gave no constant %s", name)
			continue
		}
		if !reflect.DeepEqual(c.Binders, want.binders) || c.Conclusion != want.conclusion {
			t.Errorf("%s (%s): binders %q, conclusion %q; want %q, %q", name, c.Type, c.Binders, c.Conclusion, want.binders, want.conclusion)
		}
	}
}

// A `let` in a type, at its top or between its binders, hides no head: the binders after it and the conclusion are
// read past it.
func TestALetInATypeHidesNoHead(t *testing.T) {
	ctx := context.Background()
	l := leanForTest(t)
	dir := t.TempDir()
	compileSource(t, l, dir, "Lets", "theorem letConclusion : let n := 5; n = 5 := by intro n; rfl\n"+
		"theorem letThenBinder : let n := 5; n = 5 → True := by intros; trivial\n"+
		"theorem binderThenLet (k : Nat) : let n := k; n = k := by intro n; rfl\n")
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	cs, err := x.Run(ctx, Manifest{Modules: []string{"Lets"}, Roots: []string{"Lets"}, Types: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Constant{}
	for _, c := range cs {
		got[c.Name] = c
	}
	for name, want := range map[string]struct {
		binders    []string
		conclusion string
	}{
		"letConclusion": {[]string{}, "Eq"},
		"letThenBinder": {[]string{"Eq"}, "True"},
		"binderThenLet": {[]string{"Nat"}, "Eq"},
	} {
		c, ok := got[name]
		if !ok {
			t.Errorf("the exporter gave no constant %s", name)
			continue
		}
		if !reflect.DeepEqual(c.Binders, want.binders) || c.Conclusion != want.conclusion {
			t.Errorf("%s (%s): binders %q, conclusion %q; want %q, %q", name, c.Type, c.Binders, c.Conclusion, want.binders, want.conclusion)
		}
	}
}

// A type annotation (`optParam`, `autoParam`) under metadata, on the whole annotation or on its partly applied
// function, is read as the annotation it is: the head is the head of the type under it, and no head at all where
// that type has none. Lean reads an annotation only on an application that holds no metadata, so the exporter
// removes the two in turn. Metadata on what is not an annotation (a sort, a lambda, a variable, a let) makes no head.
func TestAnAnnotationUnderMetadataIsNoHead(t *testing.T) {
	ctx := context.Background()
	l := leanForTest(t)
	dir := t.TempDir()
	compileSource(t, l, dir, "Annotations", "import Lean\nopen Lean Elab Term\n"+
		// whole: metadata on the whole term; spine: on the function of its application, as an elaborator can leave a
		// partly applied constant; firstDomainSpine: the first binder type of a declaration, the same way.
		"elab \"whole\" t:term : term => do\n"+
		"  return mkMData (KVMap.empty.setBool `annotated true) (← elabTerm t none)\n"+
		"elab \"spine\" t:term : term => do\n"+
		"  match ← elabTerm t none with\n"+
		"  | .app f a => return .app (mkMData (KVMap.empty.setBool `annotated true) f) a\n"+
		"  | e => throwError \"not an application: {e}\"\n"+
		"elab \"firstDomainSpine\" id:ident : term => do\n"+
		"  let .forallE _ (.app f a) _ _ := (← getConstInfo id.getId).type | throwError \"no applied binder type\"\n"+
		"  return .app (mkMData (KVMap.empty.setBool `annotated true) f) a\n"+
		"elab \"lambdaUnderMetadata\" : term => do\n"+
		"  return mkMData (KVMap.empty.setBool `annotated true) (← elabTerm (← `(fun T : Type => T)) none)\n"+
		"def defaultPlain (h : optParam Nat 0) : Nat := h\n"+
		"def defaultSpine (h : spine (optParam Nat 0)) : Nat := h\n"+
		"def defaultWhole (h : whole (optParam Nat 0)) : Nat := h\n"+
		"theorem predicateDefaultPlain (P : optParam (Nat → Prop) (fun _ => True)) : True := trivial\n"+
		"theorem predicateDefaultSpine (P : spine (optParam (Nat → Prop) (fun _ => True))) : True := trivial\n"+
		"def tacticDefault (h : Nat := by exact 0) : Nat := h\n"+
		"def tacticDefaultSpine (h : firstDomainSpine tacticDefault) : Nat := h\n"+
		"def sortWhole : whole Type := Nat\n"+
		"def lambdaSpine : lambdaUnderMetadata Nat := (0 : Nat)\n"+
		"def letWhole : whole (let T := Nat; T) := (0 : Nat)\n"+
		"theorem variableSpine (P : Nat → Prop) (h : (whole P) 0) : (whole P) 0 := h\n")
	x := &Exporter{Lean: l, Bin: exporterForTest(t, l), SearchPath: []string{dir}, Jobs: 1}
	cs, err := x.Run(ctx, Manifest{Modules: []string{"Annotations"}, Roots: []string{"Annotations"}, Types: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Constant{}
	for _, c := range cs {
		got[c.Name] = c
	}
	for name, want := range map[string]struct {
		binders    []string
		conclusion string
	}{
		"defaultPlain":          {[]string{"Nat"}, "Nat"},
		"defaultSpine":          {[]string{"Nat"}, "Nat"},
		"defaultWhole":          {[]string{"Nat"}, "Nat"},
		"predicateDefaultPlain": {[]string{""}, "True"},
		"predicateDefaultSpine": {[]string{""}, "True"},
		"tacticDefault":         {[]string{"Nat"}, "Nat"},
		"tacticDefaultSpine":    {[]string{"Nat"}, "Nat"},
		"sortWhole":             {[]string{}, ""},
		"lambdaSpine":           {[]string{}, ""},
		"letWhole":              {[]string{}, ""},
		"variableSpine":         {[]string{"", ""}, ""},
	} {
		c, ok := got[name]
		if !ok {
			t.Errorf("the exporter gave no constant %s", name)
			continue
		}
		if !reflect.DeepEqual(c.Binders, want.binders) || c.Conclusion != want.conclusion {
			t.Errorf("%s (%s): binders %q, conclusion %q; want %q, %q", name, c.Type, c.Binders, c.Conclusion, want.binders, want.conclusion)
		}
	}
	// The index holds what the exporter gave, once a refresh with the compiled part has read the module.
	s := openStore(t)
	tree := Tree{Source: dir, Lib: dir, Roots: Roots{"Annotations": true}}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: goldenCommit, Compiled: compiledWith(&CompiledOptions{Exporter: x, Dir: t.TempDir()})}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompiledCommit(ctx, true); err != nil {
		t.Fatal(err)
	}
	held, err := s.ConstantsIn(ctx, []string{"Annotations"})
	if err != nil {
		t.Fatal(err)
	}
	if want := byModuleAndName(cs); !reflect.DeepEqual(held, want) {
		t.Errorf("the index holds:\n%+v\nthe exporter gave:\n%+v", held, want)
	}
}
