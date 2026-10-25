package declindex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// goldenCommit is the commit refreshedGolden records.
const goldenCommit = "0123456789abcdef0123456789abcdef01234567"

func refreshedGolden(t *testing.T) *Store {
	t.Helper()
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), goldenTree(t), RefreshOptions{Commit: goldenCommit}); err != nil {
		t.Fatal(err)
	}
	return s
}

func declNames(decls []Decl) []string {
	out := make([]string, len(decls))
	for i, d := range decls {
		out[i] = d.Name + "@" + d.Module
	}
	return out
}

func TestReadFunctionsAnswerFromTheGoldenIndex(t *testing.T) {
	s := refreshedGolden(t)
	ctx := context.Background()
	if commit, err := s.Commit(ctx); err != nil || commit != goldenCommit {
		t.Fatalf("Commit = %q, %v; want %q", commit, err, goldenCommit)
	}
	paths, err := s.ModulePaths(ctx)
	if want := map[string]string{"Golden": "Golden.lean", "Sub.Wrap": "Sub/Wrap.lean"}; err != nil || !reflect.DeepEqual(paths, want) {
		t.Fatalf("ModulePaths = %v, %v; want %v", paths, err, want)
	}
	paths["Golden"] = "changed by the caller"
	if again, err := s.ModulePaths(ctx); err != nil || again["Golden"] != "Golden.lean" {
		t.Fatalf("ModulePaths after the caller changed its map = %v, %v; want Golden at Golden.lean", again, err)
	}

	decls, err := s.DeclsInModules(ctx, []string{"Sub.Wrap", "Sub.Wrap", "Missing"})
	want := []string{"Gold2.FrameAt@Sub.Wrap", "Gold2.box_two@Sub.Wrap", "Gold2.frameAt_al@Sub.Wrap",
		"Gold2.frameAt_use@Sub.Wrap", "Gold2.only_proof@Sub.Wrap"}
	if err != nil || !reflect.DeepEqual(declNames(decls), want) {
		t.Fatalf("DeclsInModules(Sub.Wrap) = %v, %v; want %v", declNames(decls), err, want)
	}
	box, err := s.DeclsNamed(ctx, []string{"Gold.Box", "Gold.Nothing"})
	if err != nil || len(box) != 1 {
		t.Fatalf("DeclsNamed(Gold.Box) = %+v, %v; want one declaration", box, err)
	}
	if got := box[0]; got.Short != "Box" || got.Module != "Golden" || got.Kind != "structure" || got.Line != 36 || got.StmtHash == "" {
		t.Fatalf("DeclsNamed(Gold.Box) = %+v; want the structure Box of Golden at line 36 with a statement hash", got)
	}
	short, err := s.DeclsShort(ctx, []string{"frame_al", "frameAt_al"})
	if want := []string{"Gold.frame_al@Golden", "Gold2.frameAt_al@Sub.Wrap"}; err != nil || !reflect.DeepEqual(declNames(short), want) {
		t.Fatalf("DeclsShort = %v, %v; want %v", declNames(short), err, want)
	}
	if none, err := s.DeclsNamed(ctx, nil); err != nil || len(none) != 0 {
		t.Fatalf("DeclsNamed(nil) = %+v, %v; want none", none, err)
	}
}

// UsesOf gives the uses of a declaration and of the constants Lean made from it (Box's field and constructor), and
// not those of another declaration whose name it begins (Gold.Frame is not Gold.FrameAt's prefix by a dot).
func TestUsesOfGivesTheUsesOfADeclarationAndItsGeneratedParts(t *testing.T) {
	s := refreshedGolden(t)
	ctx := context.Background()
	uses, err := s.UsesOf(ctx, []string{"Gold.Box", "Gold.Box"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[Use]bool{
		{Target: "Gold.Box.val", TargetModule: "Golden", Module: "Golden", Line: 40, Col: 33, Parent: "Gold.useBox", Class: "proof"}: true,
		{Target: "Gold.Box.mk", TargetModule: "Golden", Module: "Golden", Line: 40, Col: 40, Parent: "Gold.useBox", Class: "proof"}:  true,
	}
	found := 0
	for _, u := range uses {
		if u.Target != "Gold.Box" && !strings.HasPrefix(u.Target, "Gold.Box.") {
			t.Errorf("UsesOf(Gold.Box) gave a use of %s", u.Target)
		}
		if want[u] {
			found++
		}
	}
	if found != len(want) {
		t.Fatalf("UsesOf(Gold.Box) = %+v; want among them %v", uses, want)
	}
	frame, err := s.UsesOf(ctx, []string{"Gold.Frame"})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range frame {
		if u.Target == "Gold2.FrameAt" {
			t.Fatalf("UsesOf(Gold.Frame) gave a use of Gold2.FrameAt: %+v", u)
		}
	}
	if !hasUse(frame, "Gold.Frame", "Sub.Wrap", 8, 50) {
		t.Fatalf("UsesOf(Gold.Frame) = %+v; want the use in Sub/Wrap at 8:50", frame)
	}
}

func hasUse(uses []Use, target, module string, line, col int) bool {
	for _, u := range uses {
		if u.Target == target && u.Module == module && u.Line == line && u.Col == col {
			return true
		}
	}
	return false
}

// Imports gives every module the index read, with the modules its reference file says it imports directly: none for
// Golden.
func TestImportsGivesEachModulesDirectImports(t *testing.T) {
	s := refreshedGolden(t)
	imports, err := s.Imports(context.Background())
	if want := map[string][]string{"Golden": nil, "Sub.Wrap": {"Golden"}}; err != nil || !reflect.DeepEqual(imports, want) {
		t.Fatalf("Imports = %#v, %v; want %#v", imports, err, want)
	}
}

// OpenReadOnlyConfig opens the index that a configuration names, read-only, as graph checks do.
func TestOpenReadOnlyConfigOpensTheConfiguredIndex(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DB: filepath.Join(t.TempDir(), "index.sqlite")}
	s, err := Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, goldenTree(t), RefreshOptions{Commit: goldenCommit}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnlyConfig(ctx, cfg, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.CheckComplete(ctx); err != nil {
		t.Fatalf("CheckComplete: %v", err)
	}
	if commit, err := reader.Commit(ctx); err != nil || commit != goldenCommit {
		t.Fatalf("Commit = %q, %v; want %q", commit, err, goldenCommit)
	}
	if _, err := OpenReadOnlyConfig(ctx, Config{}, IndexOnly); !errors.Is(err, ErrNoDB) {
		t.Fatalf("OpenReadOnlyConfig of a configuration without an index: %v; want ErrNoDB", err)
	}
}

// The index keeps each declaration's shape (SourceShapes of its source) and the resolved heads of its explicit
// binders and conclusion (SourceHeads, ResolveHead), so a candidate's source can be compared with the base's.
func TestReadFunctionsFindPersistedShapesAndHeads(t *testing.T) {
	_, src, _ := readGolden(t)
	shape := sourceShapeAt(t, SourceShapes(src), 14).Shape
	s := refreshedGolden(t)
	ctx := context.Background()

	decls, err := s.DeclsWithShape(ctx, []string{shape})
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 || decls[0].Name != "Gold.frame_al" || decls[0].Shape != shape {
		t.Fatalf("DeclsWithShape(%q) = %+v, want Gold.frame_al with that shape", shape, decls)
	}
	named, err := s.DeclsNamed(ctx, []string{"Gold.frame_al"})
	if err != nil || len(named) != 1 || named[0].Shape != shape {
		t.Fatalf("DeclsNamed(Gold.frame_al) = %+v, %v; want its shape %q", named, err, shape)
	}

	heads, err := s.Heads(ctx, "binder", []string{"Gold.Frame", "Gold.Frame"})
	if err != nil {
		t.Fatal(err)
	}
	wantBinder := Head{Name: "Gold.frame_al", Module: "Golden", Role: "binder", Head: "Gold.Frame", Line: 14}
	if want := []Head{wantBinder, {Name: "Gold.frame_lo", Module: "Golden", Role: "binder", Head: "Gold.Frame", Line: 17}}; !reflect.DeepEqual(heads, want) {
		t.Fatalf("Heads(binder, Gold.Frame) = %+v, want %+v", heads, want)
	}
	if got, err := s.Heads(ctx, "conclusion", []string{"Gold.Frame"}); err != nil || len(got) != 0 {
		t.Fatalf("Heads(conclusion, Gold.Frame) = %+v, %v; want none", got, err)
	}
	wantConclusion := Head{Name: "Gold.frame_al", Module: "Golden", Role: "conclusion", Head: "Gold.Aligned", Line: 14}
	aligned, err := s.Heads(ctx, "", []string{"Gold.Aligned"})
	if want := []Head{wantConclusion, {Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Gold.Aligned", Line: 10}}; err != nil || !reflect.DeepEqual(aligned, want) {
		t.Fatalf("Heads(any role, Gold.Aligned) = %+v, %v; want %+v", aligned, err, want)
	}
	assertHeads(t, s, "Gold.frame_al", []Head{wantBinder, wantConclusion})
}

// An instance is a producer of its class: its conclusion head is the class, and its explicit binders' heads are kept
// as for a theorem.
func TestRefreshKeepsTheHeadsOfInstances(t *testing.T) {
	s := refreshedGolden(t)
	assertHeads(t, s, "Gold.instFrameDec", []Head{
		{Name: "Gold.instFrameDec", Module: "Golden", Role: "binder", Head: "BitVec", Line: 27},
		{Name: "Gold.instFrameDec", Module: "Golden", Role: "conclusion", Head: "Decidable", Line: 27},
	})
	// An anonymous instance has the name Lean gave it, and its heads are joined to it by its line.
	src := "class Sized (α : Type) where\n  size : Nat\ninstance : Sized Nat := ⟨1⟩\n"
	ilean := `{"version":5,"module":"M","directImports":[],"decls":{"Sized":[0,0,1,12,0,6,0,11],` +
		`"instSizedNat":[2,0,2,27,2,0,2,8]},"references":{}}`
	dir := t.TempDir()
	writeModuleFiles(t, dir, "M", src, ilean)
	m := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"M": true}})
	heads, err := m.Heads(context.Background(), "conclusion", []string{"Sized"})
	if want := []Head{{Name: "instSizedNat", Module: "M", Role: "conclusion", Head: "Sized", Line: 3}}; err != nil || !reflect.DeepEqual(heads, want) {
		t.Fatalf("Heads(conclusion, Sized) = %+v, %v; want %+v", heads, err, want)
	}
}

func TestDeclsWithShapeReturnsEveryMatchingDeclaration(t *testing.T) {
	dir := t.TempDir()
	src := "theorem one (h : Input) : Output := by sorry\ntheorem two (h : Input) : Output := by sorry\n"
	writeModuleFiles(t, dir, "M", src, `{"version":5,"module":"M","decls":{"M.one":[0,0,0,44,0,8,0,11],"M.two":[1,0,1,44,1,8,1,11]},"references":{}}`)
	s := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"M": true}})
	shape := sourceShapeAt(t, SourceShapes([]byte(src)), 1).Shape
	decls, err := s.DeclsWithShape(context.Background(), []string{shape})
	if want := []string{"M.one@M", "M.two@M"}; err != nil || !reflect.DeepEqual(declNames(decls), want) {
		t.Fatalf("DeclsWithShape(%q) = %v, %v; want %v", shape, declNames(decls), err, want)
	}
}

// A declaration's line is its first, doc comment included, both in the reference file and in SourceShapes and
// SourceHeads, so the index joins them on it.
func TestPersistedSourceFactsUseDocumentedDeclarationLines(t *testing.T) {
	_, src, _ := readGolden(t)
	shape := sourceShapeAt(t, SourceShapes(src), 4).Shape
	s := refreshedGolden(t)
	decls, err := s.DeclsWithShape(context.Background(), []string{shape})
	if err != nil || len(decls) != 1 || decls[0].Name != "Gold.SpWord" {
		t.Fatalf("DeclsWithShape(%q) = %+v, %v; want Gold.SpWord", shape, decls, err)
	}
	assertHeads(t, s, "Gold.Aligned", []Head{
		{Name: "Gold.Aligned", Module: "Golden", Role: "binder", Head: "BitVec", Line: 7},
		{Name: "Gold.Aligned", Module: "Golden", Role: "conclusion", Head: "Prop", Line: 7},
	})
}

// A head is resolved against every declaration of the index. When another module adds or removes a declaration of
// its name, the refresh resolves it again, though its own module did not change.
func TestRefreshReresolvesHeadsWhenAnotherModuleChanges(t *testing.T) {
	tree := goldenTree(t)
	s := refreshed(t, tree)
	ctx := context.Background()
	resolved := []Head{
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "BitVec", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "Gold2.FrameAt", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Gold.Aligned", Line: 10},
	}
	assertHeads(t, s, "Gold2.frameAt_al", resolved)

	path := filepath.Join(tree.Lib, "Golden.ilean")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ilean map[string]json.RawMessage
	if err := json.Unmarshal(original, &ilean); err != nil {
		t.Fatal(err)
	}
	var decls map[string]json.RawMessage
	if err := json.Unmarshal(ilean["decls"], &decls); err != nil {
		t.Fatal(err)
	}
	delete(decls, "Gold.Aligned")
	if ilean["decls"], err = json.Marshal(decls); err != nil {
		t.Fatal(err)
	}
	withoutAligned, err := json.Marshal(ilean)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, withoutAligned, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "without-aligned"}); err != nil {
		t.Fatal(err)
	}
	unresolved := slices.Clone(resolved)
	unresolved[2].Head = "Aligned"
	assertHeads(t, s, "Gold2.frameAt_al", unresolved)

	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "with-aligned"}); err != nil {
		t.Fatal(err)
	}
	assertHeads(t, s, "Gold2.frameAt_al", resolved)
}

// A module that leaves the index takes its heads with it, and the heads that resolved to its declarations are
// resolved again.
func TestRefreshDeletesProviderHeadsAndReresolvesDependents(t *testing.T) {
	tree := goldenTree(t)
	s := refreshed(t, tree)
	if err := os.Remove(filepath.Join(tree.Lib, "Golden.ilean")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "without-golden"}); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.HeadsOf(context.Background(), []string{"Gold.Aligned"}); err != nil || len(removed) != 0 {
		t.Fatalf("heads of the removed Gold.Aligned = %+v, %v; want none", removed, err)
	}
	assertHeads(t, s, "Gold2.frameAt_al", []Head{
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "BitVec", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "Gold2.FrameAt", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Aligned", Line: 10},
	})
}

func assertHeads(t *testing.T, s *Store, name string, want []Head) {
	t.Helper()
	got, err := s.HeadsOf(context.Background(), []string{name})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HeadsOf(%s) = %+v, want %+v", name, got, want)
	}
}

// writeModuleFiles writes the source and reference file of module rel into dir, a tree that is its own library.
func writeModuleFiles(t *testing.T, dir, rel, src, ilean string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel+".lean"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel+".ilean"), []byte(ilean), 0o644); err != nil {
		t.Fatal(err)
	}
}
