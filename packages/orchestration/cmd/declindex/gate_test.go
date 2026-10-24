package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

func gateFixture(t *testing.T) (string, string, string) {
	t.Helper()
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	registry := filepath.Join(tree, "registry")
	if err := os.MkdirAll(filepath.Join(registry, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(body, []byte("registry: registry\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(filepath.Dir(config), "proposal.json")
	if err := os.WriteFile(proposal, []byte(`{"id":"p","concept":{"name":"Fresh","home":"New.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return config, tree, proposal
}

func TestGateRequiresAValidProposer(t *testing.T) {
	config, _, proposal := gateFixture(t)
	for _, args := range [][]string{
		{"--config", config, "gate", proposal},
		{"--config", config, "gate", "--proposer", "not an owner", proposal},
		{"--config", config, "gate", "--proposer", "role", "--accepted-by", "no!", proposal},
	} {
		r := invoke(t, args...)
		if r.code != 2 || !strings.Contains(r.errOut, "owner id") {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

func TestGateRequiresIndexCommitAndCleanRegistry(t *testing.T) {
	old := gateSandboxAvailable
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() { gateSandboxAvailable = old })
	t.Run("head", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		path := filepath.Join(tree, "Golden.lean")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(body, []byte("\n-- changed\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, tree, "commit", "-qam", "advance tree")
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "index commit") {
			t.Fatalf("head mismatch: %+v", r)
		}
	})
	t.Run("registry", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		if err := os.WriteFile(filepath.Join(tree, "registry", "untracked"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry") {
			t.Fatalf("dirty registry: %+v", r)
		}
	})
}

func TestGateRejectsInTreeStateAndEscapedRegistry(t *testing.T) {
	old := gateSandboxAvailable
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() { gateSandboxAvailable = old })
	t.Run("state", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		body = []byte(strings.Replace(string(body), "db: state/index.sqlite", "db: tree/.declindex/index.sqlite", 1))
		if err := os.WriteFile(config, body, 0o644); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "outside the tree") {
			t.Fatalf("in-tree state: %+v", r)
		}
		if status := git(t, tree, "status", "--porcelain"); status != "" {
			t.Fatalf("gate dirtied the tree: %q", status)
		}
	})
	t.Run("registry", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		outside := filepath.Join(filepath.Dir(tree), "outside-registry")
		if err := os.MkdirAll(filepath.Join(outside, "concepts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		body = []byte(strings.Replace(string(body), "registry: registry", "registry: ../outside-registry", 1))
		if err := os.WriteFile(config, body, 0o644); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry") {
			t.Fatalf("escaped registry: %+v", r)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		config, tree, proposal := gateFixture(t)
		outside := filepath.Join(filepath.Dir(tree), "symlink-registry")
		if err := os.MkdirAll(filepath.Join(outside, "concepts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(tree, "registry")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(tree, "registry")); err != nil {
			t.Fatal(err)
		}
		r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
		if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "inside the tree") {
			t.Fatalf("symlink registry: %+v", r)
		}
	})
}

func TestGateRejectsRegistryMutationAfterInitialStatus(t *testing.T) {
	oldSandbox, oldStatus := gateSandboxAvailable, gateAfterRegistryStatus
	gateSandboxAvailable = func() error { return nil }
	t.Cleanup(func() {
		gateSandboxAvailable = oldSandbox
		gateAfterRegistryStatus = oldStatus
	})
	config, tree, proposal := gateFixture(t)
	gateAfterRegistryStatus = func() {
		if err := os.WriteFile(filepath.Join(tree, "registry", "mutated"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 3 || r.out != "" || !strings.Contains(r.errOut, "registry has local changes") {
		t.Fatalf("registry mutation: %+v", r)
	}
}

func TestGatePrintsCanonicalAdditionPathAfterSnapshotCleanup(t *testing.T) {
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("the successful gate path compiles untrusted source: set DECLINDEX_SANDBOX_TEST=1 in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "registry", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "Fixture.lean"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	lean, err := declindex.FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "registry", "registry.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "index.yaml")
	body := "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n"
	if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	proposal := filepath.Join(dir, "proposal.json")
	if err := os.WriteFile(proposal, []byte(`{"id":"p","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	var verdict struct {
		CompiledHash string `json:"compiled_hash"`
		Addition     struct {
			Path string `json:"path"`
		} `json:"addition"`
	}
	if err := json.Unmarshal([]byte(r.out), &verdict); err != nil {
		t.Fatal(err)
	}
	want := "registry/concepts/" + verdict.CompiledHash + ".yaml"
	if verdict.CompiledHash == "" || verdict.Addition.Path != want {
		t.Fatalf("addition path %q, want %q", verdict.Addition.Path, want)
	}
	if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(verdict.Addition.Path))); !os.IsNotExist(err) {
		t.Fatalf("gate wrote canonical addition path: %v", err)
	}
	if status := git(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("gate dirtied the tree: %q", status)
	}
}

// The gate's registry snapshot comes from `git archive`, whose entries name tree paths; extraction must still refuse
// any entry that would write outside the snapshot root or outside the registry, and links.
func TestExtractRegistryArchiveRefusesEntriesOutsideTheRegistry(t *testing.T) {
	archive := func(t *testing.T, entries ...tar.Header) *bytes.Buffer {
		t.Helper()
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		for _, h := range entries {
			body := []byte("escaped\n")
			if h.Typeflag == tar.TypeReg {
				h.Size = int64(len(body))
			}
			h.Mode = 0o644
			if err := w.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := w.Write(body); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return &buf
	}
	written := func(t *testing.T, dir string) []string {
		t.Helper()
		var got []string
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				got = append(got, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	hostile := map[string]func(dir string) tar.Header{
		"parent":          func(string) tar.Header { return tar.Header{Name: "../escape", Typeflag: tar.TypeReg} },
		"registry-parent": func(string) tar.Header { return tar.Header{Name: "registry/../../escape", Typeflag: tar.TypeReg} },
		"absolute": func(dir string) tar.Header {
			return tar.Header{Name: filepath.Join(dir, "escape"), Typeflag: tar.TypeReg}
		},
		"sibling": func(string) tar.Header { return tar.Header{Name: "registry/../other/escape", Typeflag: tar.TypeReg} },
		"outside": func(string) tar.Header { return tar.Header{Name: "other/escape", Typeflag: tar.TypeReg} },
		"symlink": func(string) tar.Header {
			return tar.Header{Name: "registry/link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}
		},
	}
	for name, entry := range hostile {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "snapshot")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractRegistryArchive(archive(t, entry(dir)), root, "registry")
			if err == nil {
				t.Fatal("the hostile entry was accepted")
			}
			if got := written(t, dir); len(got) != 0 {
				t.Fatalf("extraction wrote %v (error %v)", got, err)
			}
		})
	}
	dir := t.TempDir()
	if err := extractRegistryArchive(archive(t,
		tar.Header{Name: "registry/", Typeflag: tar.TypeDir},
		tar.Header{Name: "registry/registry.yaml", Typeflag: tar.TypeReg},
		tar.Header{Name: "registry/concepts/0123456789abcdef0123456789abcdef.yaml", Typeflag: tar.TypeReg},
	), dir, "registry"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "registry", "concepts", "0123456789abcdef0123456789abcdef.yaml"),
		filepath.Join(dir, "registry", "registry.yaml"),
	}
	if got := written(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("extracted %v, want %v", got, want)
	}

	// A nested registry: `git archive` lists the directories that lead to it, and nothing else outside it.
	nested := t.TempDir()
	if err := extractRegistryArchive(archive(t,
		tar.Header{Name: "docs/", Typeflag: tar.TypeDir},
		tar.Header{Name: "docs/lx/", Typeflag: tar.TypeDir},
		tar.Header{Name: "docs/lx/registry/", Typeflag: tar.TypeDir},
		tar.Header{Name: "docs/lx/registry/registry.yaml", Typeflag: tar.TypeReg},
	), nested, "docs/lx/registry"); err != nil {
		t.Fatal(err)
	}
	if got := written(t, nested); len(got) != 1 || got[0] != filepath.Join(nested, "docs", "lx", "registry", "registry.yaml") {
		t.Fatalf("nested registry extracted %v", got)
	}
	for _, outside := range []tar.Header{
		{Name: "docs/other.yaml", Typeflag: tar.TypeReg},
		{Name: "docs/other/", Typeflag: tar.TypeDir},
	} {
		if err := extractRegistryArchive(archive(t, outside), t.TempDir(), "docs/lx/registry"); err == nil {
			t.Errorf("entry %s beside a nested registry was accepted", outside.Name)
		}
	}
}

// leanGateFixture is the Lean fixture tree, compiled, with a format-2 registry holding concepts (file name to
// content), committed, and a configuration that reads the compiled part; the index is refreshed. It needs Lean and
// the sandbox.
func leanGateFixture(t *testing.T, concepts map[string]string) (dir, tree, config string) {
	t.Helper()
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("the successful gate path compiles untrusted source: set DECLINDEX_SANDBOX_TEST=1 in a disposable container or CI runner")
	}
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and lake is unavailable")
	}
	dir = t.TempDir()
	tree = filepath.Join(dir, "tree")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "Fixture.lean"), string(fixture))
	lean, err := declindex.FindLean(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(context.Background(), lean, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tree, "registry", "registry.yaml"), "version: 2\n")
	for name, body := range concepts {
		writeTestFile(t, filepath.Join(tree, "registry", "concepts", name), body)
	}
	config = filepath.Join(dir, "index.yaml")
	writeTestFile(t, config, "tree: tree\nlib: .\ndb: state/index.sqlite\nroots: [Fixture]\nregistry: registry\nlean:\n  export: true\n  jobs: 1\n")
	git(t, tree, "init", "-q")
	git(t, tree, "add", ".")
	git(t, tree, "commit", "-qm", "fixture")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	return dir, tree, config
}

// A concept file that the loader refuses stops neither a refresh that reads the compiled part nor the gate: both
// note the file as a registry defect and go on without it.
func TestAMalformedConceptFileStopsNeitherRefreshNorTheGate(t *testing.T) {
	dir, _, config := leanGateFixture(t, map[string]string{
		"Fx.Bad.yaml": "name: Fx.Bad\nhome: Fixture.lean\nmeaning: Register x2 is aligned. It stays so.\n",
	})
	proposal := filepath.Join(dir, "proposal.json")
	writeTestFile(t, proposal, `{"id":"p9","concept":{"name":"Fresh","home":"Fresh.lean","meaning":"A fresh fact."},"source":"def Fresh : Prop := True\n"}`)
	r := invoke(t, "--config", config, "gate", "--proposer", "function:open_file", proposal)
	if r.code != 0 || !strings.Contains(r.errOut, "concepts/Fx.Bad.yaml") {
		t.Fatalf("gate with one malformed concept file: want exit 0 and a defect note naming it, got %+v", r)
	}
}
