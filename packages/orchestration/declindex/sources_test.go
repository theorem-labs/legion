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

	got, prefix, err := readSources(Tree{Source: dir, Roots: Roots{"Root": true}})
	if err != nil || prefix != "" {
		t.Fatalf("prefix %q, %v; want the repository's root", prefix, err)
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
	got, _, err := readSources(Tree{Source: t.TempDir(), Roots: Roots{"Root": true}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("sources = %v, want nil", got)
	}
}

// TestRefreshKeysSourcesRelativeToASubdirectoryTree catches a refresh of a tree below its repository's root that
// keys a changed or untracked source by the repository-relative path git status prints: the index then keeps the
// committed blob of a modified file and loses an untracked one.
func TestRefreshKeysSourcesRelativeToASubdirectoryTree(t *testing.T) {
	tree := goldenTree(t)
	repo := t.TempDir()
	proj := filepath.Join(repo, "proj")
	if err := os.Rename(tree.Source, proj); err != nil {
		t.Fatal(err)
	}
	tree.Source, tree.Lib = proj, proj
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	golden, err := os.ReadFile(filepath.Join(proj, "Golden.lean"))
	if err != nil {
		t.Fatal(err)
	}
	writeSource(t, proj, "Golden.lean", string(golden)+"\n-- changed after the commit\n")
	writeSource(t, proj, "Sub/Untracked.lean", "def untracked := 3\n")

	s := openStore(t)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "base"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT path, blob FROM sources`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var path, blob string
		if err := rows.Scan(&path, &blob); err != nil {
			t.Fatal(err)
		}
		got[path] = blob
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, rel := range []string{"Golden.lean", "Sub/Wrap.lean", "Sub/Untracked.lean"} {
		want[rel] = gitIn(t, proj, "hash-object", "--", rel)
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
	if err := s.pool.QueryRow(ctx, `SELECT blob FROM sources WHERE path = 'Golden.lean'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := gitIn(t, tree.Source, "hash-object", "--", "Golden.lean"); got != want {
		t.Errorf("stored Golden.lean blob = %q, want %q", got, want)
	}
	if got, err := s.Meta(ctx, "sources"); err != nil || got != "git" {
		t.Errorf("sources meta = %q, %v; want git", got, err)
	}
	if got, recorded, err := s.Landing(ctx, "candidate"); err != nil || !recorded || got != "base" {
		t.Errorf("the landing of candidate replaced %q (recorded %v, %v); want base", got, recorded, err)
	}
}

// TestOpenRebuildsAnOlderSchema catches an upgrade that leaves declaration or source rows from a schema which
// land-hash cannot interpret.
func TestOpenRebuildsAnOlderSchema(t *testing.T) {
	ctx := context.Background()
	tg := indexTarget(t)
	s, err := Open(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sources (path, blob) VALUES ('Root.lean', 'deadbeef')`); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, s.pool, "built", "now"); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, s.pool, "schema", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Open(ctx, tg)
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
	if err := rebuilt.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sources`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 0 {
		t.Fatalf("sources after rebuild = %d, want 0", sources)
	}
}

// An index that the previous schema's binary wrote has no landings table and no compiled_commit or source_prefix
// key. Read as it is, it would answer the post-landing commands with "no such table", no record of a landing, or a
// tree at the repository's root. A read-only open refuses it and names the refresh that rebuilds it, and the next
// refresh rebuilds it from nothing: it reads the commit that the old index had read again, and records its landing.
func TestAnIndexOfThePreviousSchemaIsRebuiltByTheNextRefresh(t *testing.T) {
	ctx := context.Background()
	tree := goldenTree(t)
	tg := indexTarget(t)
	s, err := Open(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE landings`,
		`DELETE FROM meta WHERE k IN ('compiled_commit', 'source_prefix')`,
		`UPDATE meta SET v = '3' WHERE k = 'schema'`,
	} {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reader, err := OpenReadOnly(ctx, tg, IndexOnly); err == nil {
		_ = reader.Close()
		t.Errorf("opened an index of the previous schema read-only")
	} else if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("the refusal of an index of the previous schema does not name the refresh that rebuilds it: %v", err)
	}
	s, err = Open(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if previous, recorded, err := s.Landing(ctx, "one"); err != nil || !recorded || previous != "" {
		t.Errorf("the landing of one after the rebuild: previous %q, recorded %v, %v; want recorded with no previous", previous, recorded, err)
	}
	reader, err := OpenReadOnly(ctx, tg, IndexOnly)
	if err != nil {
		t.Fatalf("open the rebuilt index read-only: %v", err)
	}
	defer reader.Close()
	if version, err := reader.Meta(ctx, "schema"); err != nil || version != SchemaVersion {
		t.Errorf("the rebuilt index's schema: %q, %v; want %q", version, err, SchemaVersion)
	}
}
