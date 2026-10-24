package declindex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A symlinked index file is one index whichever path names it (store.go, resolveIndex): its lock files and graph are
// beside the file it resolves to. Its compiled state must be too. Config.Compiled builds the exporter and the forms
// module under filepath.Dir(c.DB), the unresolved path, and compiledDigest hashes the exporter's path, so the same
// index refreshed through the link and then through the file gets two exporters and two digests: each switch reads
// every compiled module again (on the copy, 52,548 modules, 13 minutes, where an unchanged exporter reads 1 module in
// 3 s; accept145 run infra145-r7-locks-link-20261024T1319Z on linux28-poc-build), and a second exporter and forms
// directory appear beside the link.
func TestASymlinkedIndexFileHasOneCompiledState(t *testing.T) {
	leanForTest(t)
	ctx := context.Background()
	tree := t.TempDir()
	target := filepath.Join(t.TempDir(), "index.sqlite")
	created, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "index.sqlite")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	byFile, err := Config{Tree: tree, DB: target}.Compiled(ctx, FormsModule{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	byLink, err := Config{Tree: tree, DB: link}.Compiled(ctx, FormsModule{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if byLink.Exporter.Bin != byFile.Exporter.Bin || byLink.Dir != byFile.Dir {
		t.Errorf("through the link the exporter is %s and the forms directory %s; through the file %s and %s",
			byLink.Exporter.Bin, byLink.Dir, byFile.Exporter.Bin, byFile.Dir)
	}
	if entries, _ := os.ReadDir(linkDir); len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the link's directory holds %v; it should hold the link alone", names)
	}
}
