package declindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeSource(t *testing.T, root, rel, text string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadSourcesUsesTheSourceBytes catches an index that records the git index blob for a modified file or
// omits an untracked Lean source that refresh actually read.
func TestReadSourcesUsesTheSourceBytes(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	writeSource(t, dir, "Root/Clean.lean", "def clean := 1\n")
	writeSource(t, dir, "Root/Changed.lean", "def changed := 1\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	writeSource(t, dir, "Root/Changed.lean", "def changed := 2\n")
	writeSource(t, dir, "Root/Untracked.lean", "def untracked := 3\n")

	got, err := readSources(Tree{Source: dir, Roots: Roots{"Root": true}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, rel := range []string{"Root/Clean.lean", "Root/Changed.lean", "Root/Untracked.lean"} {
		want[rel] = gitIn(t, dir, "hash-object", "--", rel)
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	for rel, blob := range want {
		if got[rel] != blob {
			t.Errorf("source %s = %q, want %q", rel, got[rel], blob)
		}
	}
}

func TestReadSourcesSkipsANonGitTree(t *testing.T) {
	got, err := readSources(Tree{Source: t.TempDir(), Roots: Roots{"Root": true}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("sources = %v, want nil", got)
	}
}

// TestRefreshStoresSourcesAndTheReplacedCommit catches a refresh that cannot later compare the candidate's
// blobs with the exact source bytes it indexed, or that loses the commit the refresh replaced.
func TestRefreshStoresSourcesAndTheReplacedCommit(t *testing.T) {
	tree := goldenTree(t)
	gitIn(t, tree.Source, "init", "-q")
	gitIn(t, tree.Source, "add", "-A")
	gitIn(t, tree.Source, "commit", "-q", "-m", "base")
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "candidate"}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRowContext(ctx, `SELECT blob FROM sources WHERE path = 'Golden.lean'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := gitIn(t, tree.Source, "hash-object", "--", "Golden.lean"); got != want {
		t.Errorf("stored Golden.lean blob = %q, want %q", got, want)
	}
	if got, err := s.Meta(ctx, "sources"); err != nil || got != "git" {
		t.Errorf("sources meta = %q, %v; want git", got, err)
	}
	if got, err := s.Meta(ctx, "prev_commit"); err != nil || got != "base" {
		t.Errorf("prev_commit = %q, %v; want base", got, err)
	}
}

// TestOpenRebuildsAnOlderSchema catches an upgrade that leaves declaration or source rows from a schema which
// land-hash cannot interpret.
func TestOpenRebuildsAnOlderSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sources (path, blob) VALUES ('Root.lean', 'deadbeef')`); err != nil {
		t.Fatal(err)
	}
	if err := s.setMeta(ctx, s.db, "built", "now"); err != nil {
		t.Fatal(err)
	}
	if err := s.setMeta(ctx, s.db, "schema", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if got, err := rebuilt.Meta(ctx, "schema"); err != nil || got != SchemaVersion {
		t.Fatalf("schema = %q, %v; want %q", got, err, SchemaVersion)
	}
	if got, err := rebuilt.Meta(ctx, "built"); err != nil || got != "" {
		t.Fatalf("built = %q, %v; want empty after rebuild", got, err)
	}
	var sources int
	if err := rebuilt.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 0 {
		t.Fatalf("sources after rebuild = %d, want 0", sources)
	}
}
