package conceptgate

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// openFixture is the golden tree of the declindex package as a Git work tree with an empty format-2 registry, a
// configuration whose index is outside the tree, and that index refreshed at the tree's commit. It needs no Lean.
func openFixture(t *testing.T) (cfg declindex.Config, tree string) {
	t.Helper()
	dir := t.TempDir()
	tree = filepath.Join(dir, "tree")
	src := filepath.Join("..", "declindex", "testdata", "golden")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(tree, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tree, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "registry", "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tree, "init", "-q")
	runGit(t, tree, "config", "user.email", "test@example.com")
	runGit(t, tree, "config", "user.name", "Test")
	runGit(t, tree, "add", ".")
	runGit(t, tree, "commit", "-qm", "fixture")
	config := filepath.Join(dir, "index.yaml")
	body := "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Golden, Sub]\nregistry: registry\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = declindex.LoadConfig(config); err != nil {
		t.Fatal(err)
	}
	s, err := declindex.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Refresh(context.Background(), cfg.TreeSpec(), declindex.RefreshOptions{Commit: gitHead(t, tree)}); err != nil {
		t.Fatal(err)
	}
	return cfg, tree
}

// A registry that changes while the gate is being opened, after its first check that the registry is clean, is
// refused: the gate opens only over a registry that its snapshot matches.
func TestGateRejectsRegistryMutationAfterInitialStatus(t *testing.T) {
	cfg, tree := openFixture(t)
	old := afterRegistryStatus
	t.Cleanup(func() { afterRegistryStatus = old })
	afterRegistryStatus = func() {
		if err := os.WriteFile(filepath.Join(tree, "registry", "mutated"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := OpenGate(context.Background(), cfg, Options{SandboxAvailable: func() error { return nil }})
	if g != nil {
		g.Close()
	}
	if g != nil || err == nil || !strings.Contains(err.Error(), "registry has local changes") {
		t.Fatalf("registry mutation: gate %v, error %v", g, err)
	}
}
