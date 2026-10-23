package declindex

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestConfigBuildSettingsAndLibraryRelativePath(t *testing.T) {
	p := writeConfig(t, "tree: ../tree\nlib: out/lean\ndb: state/index.sqlite\nroots: [A]\nbuild: [tool, build]\nremote: /srv/owners.git\nbuild_timeout: 90s\nregistry: registry\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LibRel != filepath.Join("out", "lean") || c.Lib != filepath.Join(c.Tree, "out", "lean") {
		t.Fatalf("library paths: relative %q, resolved %q", c.LibRel, c.Lib)
	}
	if !reflect.DeepEqual(c.Build, []string{"tool", "build"}) || c.Remote != "/srv/owners.git" {
		t.Fatalf("build settings: %+v", c)
	}
	if c.BuildTimeout != 90*time.Second {
		t.Fatalf("build timeout %s, want 90s", c.BuildTimeout)
	}
	defaults, err := LoadConfig(writeConfig(t, "tree: tree\ndb: state/index.sqlite\nroots: [A]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.LibRel != filepath.Join(".lake", "build", "lib", "lean") || defaults.BuildTimeout != 6*time.Hour {
		t.Fatalf("defaults: relative library %q, timeout %s", defaults.LibRel, defaults.BuildTimeout)
	}
	for _, timeout := range []string{"tomorrow", "-1s"} {
		if _, err := LoadConfig(writeConfig(t, "tree: tree\nroots: [A]\nbuild_timeout: "+timeout+"\n")); err == nil {
			t.Errorf("accepted build_timeout %q", timeout)
		}
	}
}

func TestPublicConfigKeepsItsYAMLSchema(t *testing.T) {
	raw, err := yaml.Marshal(Config{
		Tree:         "tree",
		Lib:          "lib",
		DB:           "state.sqlite",
		FileKinds:    []string{"Spec"},
		Build:        []string{"build"},
		Remote:       "remote",
		BuildTimeout: time.Minute,
		OwnersFormat: "owners/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, key := range []string{"file_kinds:", "build:", "remote:", "build_timeout:", "owners_format:"} {
		if !strings.Contains(text, key) {
			t.Errorf("public Config YAML lacks %q:\n%s", key, text)
		}
	}
	for _, key := range []string{"filekinds:", "buildtimeout:", "ownersformat:"} {
		if strings.Contains(text, key) {
			t.Errorf("public Config YAML uses an implicit key %q:\n%s", key, text)
		}
	}
}

func TestConfigRefusesUnsafeRegistryPaths(t *testing.T) {
	for _, registry := range []string{"/srv/registry", "../registry", "../../registry"} {
		_, err := LoadConfig(writeConfig(t, "tree: tree\nroots: [A]\nregistry: "+registry+"\n"))
		if err == nil || !strings.Contains(err.Error(), "registry") {
			t.Errorf("unsafe registry path %q: %v", registry, err)
		}
	}
}
