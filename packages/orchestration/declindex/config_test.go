package declindex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conf", "index.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigResolvesPathsAgainstItsDirectory(t *testing.T) {
	p := writeConfig(t, "tree: ../tree\ndb: /var/index.sqlite\nroots: [A, B]\nregistry: docs/registry.yaml\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	if c.Tree != filepath.Join(dir, "..", "tree") || c.DB != "/var/index.sqlite" {
		t.Fatalf("paths: %+v", c)
	}
	if c.Lib != filepath.Join(c.Tree, ".lake", "build", "lib", "lean") {
		t.Fatalf("default lib %q", c.Lib)
	}
	if c.Registry != filepath.Join(c.Tree, "docs", "registry.yaml") {
		t.Fatalf("registry is relative to the tree: %q", c.Registry)
	}
	if got := c.TreeSpec(); !reflect.DeepEqual(got.Roots, Roots{"A": true, "B": true}) || got.Lib != c.Lib {
		t.Fatalf("tree spec %+v", got)
	}
}

func TestConfigRefusesMissingTreeAndRoots(t *testing.T) {
	for _, body := range []string{"roots: [A]\n", "tree: x\n", "tree: x\nroots: [A]\nunknown_key: 1\n"} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("config %q accepted", body)
		}
	}
}
