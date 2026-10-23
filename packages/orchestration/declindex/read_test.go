package declindex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadFunctionsAnswerFromTheGoldenIndex(t *testing.T) {
	s := refreshedGolden(t)
	ctx := context.Background()
	if err := s.Complete(ctx); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	decls, err := s.DeclsInModules(ctx, []string{"Golden"})
	if err != nil || !hasDecl(decls, "Gold.Box", "structure") {
		t.Fatalf("DeclsInModules = %+v, %v; want Gold.Box", decls, err)
	}
	uses, err := s.UsesOf(ctx, []string{"Gold.Box"})
	if err != nil || !hasUse(uses, "Gold.Box.val", "Golden", 40, 33) || !hasUse(uses, "Gold.Box.mk", "Golden", 40, 40) {
		t.Fatalf("UsesOf(Gold.Box) = %+v, %v; want its field and constructor uses", uses, err)
	}
	paths, err := s.ModulePaths(ctx)
	if err != nil || paths["Golden"] != "Golden.lean" {
		t.Fatalf("ModulePaths = %v, %v", paths, err)
	}
	commit, err := s.Commit(ctx)
	if err != nil || len(commit) != 40 {
		t.Fatalf("Commit = %q, %v", commit, err)
	}
}

func TestCompleteRefusesAnIndexWithUnreadableModules(t *testing.T) {
	s := refreshedGoldenWithUnreadable(t)
	var incomplete *IncompleteError
	if err := s.Complete(context.Background()); !errors.As(err, &incomplete) || incomplete.Unreadable != "1" {
		t.Fatalf("Complete = %v, want *IncompleteError with Unreadable 1", err)
	}
}

func TestRefreshRecordsImports(t *testing.T) {
	s := refreshedGolden(t)
	imports, err := s.Imports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := imports["Golden"]; !ok {
		t.Fatalf("Imports lacks the module Golden: %v", imports)
	}
}

func refreshedGolden(t *testing.T) *Store {
	t.Helper()
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), goldenTree(t), RefreshOptions{Commit: "0123456789abcdef0123456789abcdef01234567"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func refreshedGoldenWithUnreadable(t *testing.T) *Store {
	t.Helper()
	tree := goldenTree(t)
	path := filepath.Join(tree.Source, "Golden.lean")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("-- edited after the build\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "0123456789abcdef0123456789abcdef01234567"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func hasDecl(decls []Decl, name, kind string) bool {
	for _, declaration := range decls {
		if declaration.Name == name && declaration.Kind == kind {
			return true
		}
	}
	return false
}

func hasUse(uses []Use, target, module string, line, column int) bool {
	for _, use := range uses {
		if use.Target == target && use.Module == module && use.Line == line && use.Col == column {
			return true
		}
	}
	return false
}

func TestReadFunctionsFindPersistedShapesAndHeads(t *testing.T) {
	_, src, _ := readGolden(t)
	shape := sourceShapeAt(t, SourceShapes(src), 14).Shape
	s := refreshedGolden(t)
	ctx := context.Background()

	decls, err := s.DeclsWithShape(ctx, []string{shape})
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 || decls[0].Name != "Gold.frame_al" {
		t.Fatalf("DeclsWithShape(%q) = %+v, want Gold.frame_al", shape, decls)
	}

	heads, err := s.Heads(ctx, "binder", []string{"Gold.Frame"})
	if err != nil {
		t.Fatal(err)
	}
	wantBinder := Head{Name: "Gold.frame_al", Module: "Golden", Role: "binder", Head: "Gold.Frame", Line: 14}
	wantFrameHeads := []Head{
		wantBinder,
		{Name: "Gold.frame_lo", Module: "Golden", Role: "binder", Head: "Gold.Frame", Line: 17},
	}
	if !reflect.DeepEqual(heads, wantFrameHeads) {
		t.Fatalf("Heads(binder, Gold.Frame) = %+v, want %+v", heads, wantFrameHeads)
	}
	if got, err := s.Heads(ctx, "conclusion", []string{"Gold.Frame"}); err != nil || len(got) != 0 {
		t.Fatalf("Heads(conclusion, Gold.Frame) = %+v, %v; want none", got, err)
	}

	wantConclusion := Head{Name: "Gold.frame_al", Module: "Golden", Role: "conclusion", Head: "Gold.Aligned", Line: 14}
	assertHeads(t, s, "Gold.frame_al", []Head{wantBinder, wantConclusion})
}

func TestDeclsWithShapeReturnsEveryMatchingDeclaration(t *testing.T) {
	dir := t.TempDir()
	src := "theorem one (h : Input) : Output := by sorry\ntheorem two (h : Input) : Output := by sorry\n"
	ilean := `{"version":5,"module":"M","decls":{"M.one":[0,0,0,44,0,8,0,11],"M.two":[1,0,1,44,1,8,1,11]},"references":{}}`
	if err := os.WriteFile(filepath.Join(dir, "M.lean"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "M.ilean"), []byte(ilean), 0o644); err != nil {
		t.Fatal(err)
	}
	s := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"M": true}})
	shape := sourceShapeAt(t, SourceShapes([]byte(src)), 1).Shape
	decls, err := s.DeclsWithShape(context.Background(), []string{shape})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(decls))
	for i, declaration := range decls {
		got[i] = declaration.Name
	}
	if want := []string{"M.one", "M.two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeclsWithShape(%q) = %v, want %v", shape, got, want)
	}
}

func TestPersistedSourceFactsUseDocumentedDeclarationLines(t *testing.T) {
	_, src, _ := readGolden(t)
	shape := sourceShapeAt(t, SourceShapes(src), 4).Shape
	s := refreshedGolden(t)
	decls, err := s.DeclsWithShape(context.Background(), []string{shape})
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 || decls[0].Name != "Gold.SpWord" {
		t.Fatalf("DeclsWithShape(%q) = %+v, want Gold.SpWord", shape, decls)
	}
	assertHeads(t, s, "Gold.Aligned", []Head{
		{Name: "Gold.Aligned", Module: "Golden", Role: "binder", Head: "BitVec", Line: 7},
		{Name: "Gold.Aligned", Module: "Golden", Role: "conclusion", Head: "Prop", Line: 7},
	})
}

func TestRefreshReresolvesHeadsWhenAnotherModuleChanges(t *testing.T) {
	tree := goldenTree(t)
	s := refreshed(t, tree)
	ctx := context.Background()
	assertHeads(t, s, "Gold2.frameAt_al", []Head{
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "BitVec", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "Gold2.FrameAt", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Gold.Aligned", Line: 10},
	})

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
	ilean["decls"], err = json.Marshal(decls)
	if err != nil {
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
	assertHeads(t, s, "Gold2.frameAt_al", []Head{
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "BitVec", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "Gold2.FrameAt", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Aligned", Line: 10},
	})

	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "with-aligned"}); err != nil {
		t.Fatal(err)
	}
	assertHeads(t, s, "Gold2.frameAt_al", []Head{
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "BitVec", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "binder", Head: "Gold2.FrameAt", Line: 10},
		{Name: "Gold2.frameAt_al", Module: "Sub.Wrap", Role: "conclusion", Head: "Gold.Aligned", Line: 10},
	})
}

func TestRefreshDeletesProviderHeadsAndReresolvesDependents(t *testing.T) {
	tree := goldenTree(t)
	s := refreshed(t, tree)
	if err := os.Remove(filepath.Join(tree.Lib, "Golden.ilean")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "without-golden"}); err != nil {
		t.Fatal(err)
	}
	removed, err := s.HeadsOf(context.Background(), []string{"Gold.Aligned"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("heads of removed Gold.Aligned = %+v, want none", removed)
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
