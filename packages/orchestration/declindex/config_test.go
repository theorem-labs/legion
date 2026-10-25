package declindex

import (
	"context"
	"errors"
	"io"
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

// configWithTree writes a configuration whose `tree: ../tree` exists, which StateDir needs: it resolves the tree
// and the state directory through symlinks to compare them.
func configWithTree(t *testing.T, body string) (config, tree string) {
	t.Helper()
	config = writeConfig(t, body)
	tree = filepath.Join(filepath.Dir(filepath.Dir(config)), "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	return config, tree
}

// resolved is a path as StateDir reports it: with the symlinks of its existing part resolved.
func resolved(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigResolvesPathsAgainstItsDirectory(t *testing.T) {
	p := writeConfig(t, "tree: ../tree\nindex: idx\nstate: /var/lib/declindex\nroots: [A, B]\nregistry: docs/registry.yaml\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	if c.Tree != filepath.Join(dir, "..", "tree") || c.State != "/var/lib/declindex" {
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

func TestConfigGivesAbsolutePathsForARelativeConfigPath(t *testing.T) {
	p := writeConfig(t, "tree: ../tree\nindex: idx\nstate: state\nroots: [A]\nregistry: registry\n")
	t.Chdir(filepath.Dir(filepath.Dir(p)))
	c, err := LoadConfig(filepath.Join("conf", "index.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(filepath.Dir(p)), "tree")
	if c.Tree != want || !filepath.IsAbs(c.State) || c.Registry != filepath.Join(want, "registry") {
		t.Fatalf("relative config gave tree %q, state %q, registry %q; want tree %q and absolute paths", c.Tree, c.State, c.Registry, want)
	}
}

func TestConfigRefusesMissingTreeAndRoots(t *testing.T) {
	for _, body := range []string{"roots: [A]\n", "tree: x\n", "tree: x\nroots: [A]\nunknown_key: 1\n"} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("config %q accepted", body)
		}
	}
}

// The index is one schema of a Postgres database: index: names it, state: is the local directory of the exporter
// build and the forms module, and reader: is the read-only role a refresh grants. No key holds a connection
// string: Config.DSN comes from DECLINDEX_DSN or --index-dsn-file (P-1, P-2, P-7, P-8).
func TestConfigReadsTheIndexItsStateAndItsReaderRole(t *testing.T) {
	config, _ := configWithTree(t, "tree: ../tree\nindex: machcsl\nstate: state\nreader: declindex_ro\nroots: [A]\n")
	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Index != "machcsl" || c.Reader != "declindex_ro" {
		t.Fatalf("index %q, reader %q", c.Index, c.Reader)
	}
	if c.State != filepath.Join(filepath.Dir(config), "state") {
		t.Fatalf("state %q is not relative to the configuration file's directory", c.State)
	}
	if c.DSN != "" {
		t.Fatalf("the configuration file set a connection string: %q", c.DSN)
	}
	c.DSN = "postgres://example.invalid/declindex"
	want := Target{DSN: c.DSN, Index: "machcsl", Reader: "declindex_ro"}
	if got := c.Target(); got != want {
		t.Fatalf("Target() = %+v, want %+v", got, want)
	}
	state, err := c.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved(t, filepath.Dir(config)), "state"); state != want {
		t.Fatalf("StateDir() = %q, want %q", state, want)
	}
}

// ValidIndexName is the rule for a schema name: a lowercase letter, then lowercase letters, digits and _, at most
// 48 characters, and none of Postgres's own names (P-2).
func TestValidIndexNameTakesASchemaNameOfTheIndex(t *testing.T) {
	for _, name := range []string{"machcsl", "idx", "a_1", "a", strings.Repeat("a", 48)} {
		if err := ValidIndexName(name); err != nil {
			t.Errorf("ValidIndexName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "1a", "Idx", "pg_x", "pg_catalog", "public", "information_schema",
		strings.Repeat("a", 49), "a-b", "a.b", "a b", `a"b`, "_a"} {
		if err := ValidIndexName(name); err == nil {
			t.Errorf("ValidIndexName accepted %q", name)
		}
	}
}

func TestConfigRefusesAnIndexNameThatIsNoSchemaName(t *testing.T) {
	err := refusedConfig(t, "tree: ../tree\nroots: [A]\nindex: Idx\n")
	if !strings.Contains(err.Error(), "Idx") {
		t.Fatalf("an invalid index name was reported as %v", err)
	}
}

// db: named the SQLite file. It is gone, and its message names what replaced it, so that an old configuration
// fails with the new keys in front of its owner (P-1).
func TestConfigRefusesTheRemovedDBKey(t *testing.T) {
	for _, body := range []string{
		"tree: ../tree\nroots: [A]\ndb: /var/index.sqlite\n",
		"tree: ../tree\nroots: [A]\nindex: idx\ndb:\n",
	} {
		err := refusedConfig(t, body)
		if !errors.Is(err, ErrDBRemoved) {
			t.Errorf("config %q: %v is not ErrDBRemoved", body, err)
			continue
		}
		for _, want := range []string{"db: was removed", "index:", "DECLINDEX_DSN", "--index-dsn-file"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of db: does not name %q: %v", want, err)
			}
		}
	}
}

// refusedConfig is the error of a configuration that must be refused.
func refusedConfig(t *testing.T, body string) error {
	t.Helper()
	c, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatalf("configuration %q accepted: %+v", body, c)
	}
	return err
}

// Without state:, the exporter and the forms module go below the user's cache directory, under the index's name,
// so that two indexes on one machine keep their own (P-8).
func TestTheDefaultStateIsTheUserCacheDirectoryOfTheIndex(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	config, _ := configWithTree(t, "tree: ../tree\nindex: machcsl\nroots: [A]\n")
	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != "" {
		t.Fatalf("state %q: the default belongs to StateDir, not to the file", c.State)
	}
	state, err := c.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved(t, cache), "declindex", "machcsl"); state != want {
		t.Fatalf("StateDir() = %q, want %q", state, want)
	}
}

// The gate must not dirty the tree, so the state directory, where it builds the exporter and the forms module,
// must resolve outside the tree (P-8), as the index file had to.
func TestAStateInsideTheTreeIsRefused(t *testing.T) {
	config, tree := configWithTree(t, "tree: ../tree\nindex: idx\nstate: ../tree/state\nroots: [A]\n")
	if err := os.MkdirAll(filepath.Join(tree, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StateDir(); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("a state directory inside the tree gave %v", err)
	}
	link := filepath.Join(filepath.Dir(config), "state-link")
	if err := os.Symlink(filepath.Join(tree, "state"), link); err != nil {
		t.Fatal(err)
	}
	c.State = link
	if _, err := c.StateDir(); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("a state directory that is a symlink into the tree gave %v", err)
	}
	// The tree itself is as wrong a place for the exporter's build as a directory inside it.
	c.State = tree
	if _, err := c.StateDir(); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("a state directory that is the tree gave %v", err)
	}
	treeLink := filepath.Join(filepath.Dir(config), "tree-link")
	if err := os.Symlink(tree, treeLink); err != nil {
		t.Fatal(err)
	}
	c.State = treeLink
	if _, err := c.StateDir(); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("a state directory that is a symlink to the tree gave %v", err)
	}
}

// The exporter and the forms module of a configuration are below its state directory (P-8).
func TestTheExporterAndTheFormsModuleAreBuiltInTheStateDirectory(t *testing.T) {
	leanForTest(t)
	tree := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	options, err := Config{Tree: tree, Index: "idx", State: state}.Compiled(context.Background(), FormsModule{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolved(t, filepath.Dir(state)), "state")
	if !strings.HasPrefix(options.Exporter.Bin, filepath.Join(want, "exporter")+string(os.PathSeparator)) {
		t.Errorf("the exporter is %s, not below %s", options.Exporter.Bin, filepath.Join(want, "exporter"))
	}
	if options.Dir != filepath.Join(want, "forms") {
		t.Errorf("the forms module is in %s, want %s", options.Dir, filepath.Join(want, "forms"))
	}
}

func TestConfigBuildSettingsAndLibraryRelativePath(t *testing.T) {
	p := writeConfig(t, "tree: ../tree\nlib: out/lean\nindex: idx\nroots: [A]\nbuild: [tool, build]\nremote: /srv/owners.git\nbuild_timeout: 90s\nregistry: registry\n")
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
	defaults, err := LoadConfig(writeConfig(t, "tree: tree\nindex: idx\nroots: [A]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.LibRel != filepath.Join(".lake", "build", "lib", "lean") || defaults.BuildTimeout != 6*time.Hour {
		t.Fatalf("defaults: relative library %q, timeout %s", defaults.LibRel, defaults.BuildTimeout)
	}
	for _, timeout := range []string{"tomorrow", "-1s", "0s", "90"} {
		if _, err := LoadConfig(writeConfig(t, "tree: tree\nroots: [A]\nbuild_timeout: "+timeout+"\n")); err == nil {
			t.Errorf("accepted build_timeout %q", timeout)
		}
	}
}

func TestPublicConfigKeepsItsYAMLSchema(t *testing.T) {
	raw, err := yaml.Marshal(Config{
		Tree:         "tree",
		Lib:          "lib",
		Index:        "idx",
		State:        "state",
		Reader:       "declindex_ro",
		DSN:          "postgres://user:secret@example.invalid/declindex",
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
	for _, key := range []string{"index:", "state:", "reader:", "file_kinds:", "build:", "remote:", "build_timeout:", "owners_format:"} {
		if !strings.Contains(text, key) {
			t.Errorf("public Config YAML lacks %q:\n%s", key, text)
		}
	}
	for _, key := range []string{"filekinds:", "buildtimeout:", "ownersformat:"} {
		if strings.Contains(text, key) {
			t.Errorf("public Config YAML uses an implicit key %q:\n%s", key, text)
		}
	}
	// No configuration file names a secret: the connection string is read from the environment or a file of its
	// own (P-1), so it has no YAML key at all.
	for _, gone := range []string{"db:", "dsn:", "secret"} {
		if strings.Contains(text, gone) {
			t.Errorf("public Config YAML holds %q:\n%s", gone, text)
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
