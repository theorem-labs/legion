package declindex

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
