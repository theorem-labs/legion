package declindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
