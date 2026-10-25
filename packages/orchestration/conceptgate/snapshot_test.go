package conceptgate

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
