package declindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// goldenTree copies testdata/golden into a fresh directory that is both the source tree and the library
// directory, as `lean -R . -o … -i …` left it.
func goldenTree(t *testing.T) Tree {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join("testdata", "golden")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return Tree{Source: dir, Lib: dir, Roots: Roots{"Golden": true, "Sub": true}}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenRebuildsAnIndexFromSchema3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
		INSERT INTO meta (k, v) VALUES ('schema', '3');
		CREATE TABLE decls (
			name TEXT NOT NULL, short TEXT NOT NULL, module TEXT NOT NULL, kind TEXT NOT NULL,
			line INTEGER NOT NULL, stmt_hash TEXT NOT NULL
		)`)
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got, err := s.Meta(context.Background(), "schema"); err != nil || got != "4" {
		t.Fatalf("schema = %q, %v; want 4", got, err)
	}
	if _, err := s.db.Exec(`SELECT shape FROM decls`); err != nil {
		t.Fatalf("decls has no shape column after the rebuild: %v", err)
	}
}

func refreshed(t *testing.T, tree Tree) *Store {
	t.Helper()
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// referenceAnswer is one answer of the reference tool (testdata/golden/answers.json), without its `why` chains,
// whose choice among equal paths depends on row order.
type referenceAnswer struct {
	Declaration string          `json:"declaration"`
	Kind        string          `json:"kind"`
	At          string          `json:"at"`
	Direct      DirectUsers     `json:"direct"`
	Reach       json.RawMessage `json:"reach"`
	Lines       []string        `json:"lines"`
}

func TestAnswersMatchReferenceTool(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", "answers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]referenceAnswer
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, w := range want {
		got, err := s.Answer(ctx, name, AnswerOptions{Lines: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Declaration != w.Declaration || got.Kind != w.Kind || got.At != w.At {
			t.Errorf("%s: header %q %q %q, want %q %q %q", name, got.Declaration, got.Kind, got.At, w.Declaration, w.Kind, w.At)
		}
		if !reflect.DeepEqual(got.Direct, w.Direct) {
			t.Errorf("%s: direct users\n got %+v\nwant %+v", name, got.Direct, w.Direct)
		}
		var wantReach ReachCounts
		if err := json.Unmarshal(w.Reach, &wantReach); err != nil {
			t.Fatal(err)
		}
		if got.Reach.ReachCounts != wantReach {
			t.Errorf("%s: reach %+v, want %+v", name, got.Reach.ReachCounts, wantReach)
		}
		gl, wl := append([]string(nil), got.Lines...), append([]string(nil), w.Lines...)
		sort.Strings(gl)
		sort.Strings(wl)
		if !reflect.DeepEqual(gl, wl) && !(len(gl) == 0 && len(wl) == 0) {
			t.Errorf("%s: lines\n got %q\nwant %q", name, gl, wl)
		}
	}
}

func TestWhyNamesAChainForEveryReachedDirectory(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	got, err := s.Answer(context.Background(), "Gold.Aligned", AnswerOptions{Why: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"": "Aligned -> Gold.Frame", "Sub": "Aligned -> Gold.Frame -> Gold2.FrameAt"}
	if !reflect.DeepEqual(got.Reach.Why, want) {
		t.Fatalf("why = %v, want %v", got.Reach.Why, want)
	}
}

func TestRefreshRereadsOnlyChangedModules(t *testing.T) {
	tree := goldenTree(t)
	for _, rel := range []string{"Golden", "Sub/Wrap"} {
		if err := os.WriteFile(filepath.Join(tree.Lib, rel+".ilean.hash"), []byte("aaaa"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := openStore(t)
	ctx := context.Background()
	first, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Reread != 2 {
		t.Fatalf("first refresh re-read %d modules, want 2", first.Reread)
	}
	again, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Reread != 0 || again.Removed != 0 {
		t.Fatalf("an unchanged tree re-read %d and removed %d modules", again.Reread, again.Removed)
	}
	// Lake rewrites a hash file with the same contents when it replays a module: not a change.
	if err := os.WriteFile(filepath.Join(tree.Lib, "Golden.ilean.hash"), []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Lib, "Sub/Wrap.ilean.hash"), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	one, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Reread != 1 {
		t.Fatalf("one changed module: re-read %d", one.Reread)
	}
	if got, _ := s.Meta(ctx, "commit"); got != "two" {
		t.Fatalf("commit = %q, want two", got)
	}
	if err := os.Remove(filepath.Join(tree.Lib, "Sub/Wrap.ilean")); err != nil {
		t.Fatal(err)
	}
	gone, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "three"})
	if err != nil {
		t.Fatal(err)
	}
	if gone.Removed != 1 {
		t.Fatalf("removed %d modules, want 1", gone.Removed)
	}
	if found, err := s.Resolve(ctx, "Gold2.FrameAt"); err != nil || len(found) != 0 {
		t.Fatalf("a removed module's declaration still resolves: %v %v", found, err)
	}
	a, err := s.Answer(ctx, "Gold.Frame", AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Direct.Directories != 1 || a.Reach.StatementsChanged != 2 {
		t.Fatalf("a removed module's uses and edges remain: %+v %+v", a.Direct, a.Reach)
	}
}

func TestResolveByFullNameOrDottedSuffix(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	ctx := context.Background()
	for q, want := range map[string][]string{
		"Frame":         {"Gold.Frame"},
		"Gold.Frame":    {"Gold.Frame"},
		"Gold2.FrameAt": {"Gold2.FrameAt"},
		"hidden":        {"_private.Golden.0.Gold.hidden"},
		"rame":          nil,
		"NoSuchName":    nil,
	} {
		found, err := s.Resolve(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range found {
			names = append(names, f.Name)
		}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("Resolve(%q) = %v, want %v", q, names, want)
		}
	}
}

func TestHashesAreTheStatementHashes(t *testing.T) {
	s := refreshed(t, goldenTree(t))
	_, _, want := readGolden(t)
	ctx := context.Background()
	for _, d := range want.Decls {
		found, err := s.Resolve(ctx, d.Name)
		if err != nil || len(found) != 1 {
			t.Fatalf("%s: %v %v", d.Name, found, err)
		}
		if found[0].StmtHash != *d.Hash {
			t.Errorf("%s: hash %s, want %s", d.Name, found[0].StmtHash, *d.Hash)
		}
		if found[0].Path != "Golden.lean" {
			t.Errorf("%s: path %s", d.Name, found[0].Path)
		}
	}
}

// Lean names every `example` `_example`, so one name can be a declaration of many modules. Each is its own
// statement: a change reaches all of them, each in its own directory.
func TestReachCountsEachModulesExample(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, ilean, src string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".ilean", []byte(ilean), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".lean", []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("A", `{"version":5,"module":"A","decls":{"A.t":[0,0,0,19,0,4,0,5]},"references":{}}`, "def t : Prop := True\n")
	for _, rel := range []string{"P/B", "Q/C"} {
		mod := strings.ReplaceAll(rel, "/", ".")
		write(rel, `{"version":5,"module":"`+mod+`","decls":{"_example":[0,0,0,33,0,0,0,7]},`+
			`"references":{"{\"c\":{\"m\":\"A\",\"n\":\"A.t\"}}":{"definition":null,"usages":[[0,13,0,16,"_example"]]}}}`,
			"example (h : A.t) : True := trivial\n")
	}
	s := refreshed(t, Tree{Source: dir, Lib: dir, Roots: Roots{"A": true, "P": true, "Q": true}})
	a, err := s.Answer(context.Background(), "A.t", AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Direct.Declarations != 1 || a.Direct.Directories != 2 {
		t.Fatalf("direct users %+v", a.Direct)
	}
	want := ReachCounts{StatementsChanged: 2, StatementDirectories: 2, AllDirectories: 2}
	if a.Reach.ReachCounts != want {
		t.Fatalf("reach %+v, want %+v", a.Reach.ReachCounts, want)
	}
}

// An edit that keeps every position (a numeral changed in place) can leave the .ilean byte for byte the same;
// the statement hash comes from the source, so the module is read again.
func TestRefreshRereadsAModuleWhoseSourceChangedInPlace(t *testing.T) {
	tree := goldenTree(t)
	s := refreshed(t, tree)
	ctx := context.Background()
	before, _ := s.Resolve(ctx, "Gold.frame_lo")
	p := filepath.Join(tree.Source, "Golden.lean")
	src, _ := os.ReadFile(p)
	edited := strings.Replace(string(src), "sp.toNat ≥ 16 := by", "sp.toNat ≥ 32 := by", 1)
	if edited == string(src) {
		t.Fatal("the fixture changed: no numeral to edit")
	}
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"})
	if err != nil || st.Reread != 1 {
		t.Fatalf("re-read %d modules (%v), want 1", st.Reread, err)
	}
	after, _ := s.Resolve(ctx, "Gold.frame_lo")
	if after[0].StmtHash == before[0].StmtHash {
		t.Fatal("the statement hash did not change with the statement")
	}
}

func TestRefreshAtSerializesTheSyncStepWithTheWriterLock(t *testing.T) {
	tree := goldenTree(t)
	s := openStore(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.RefreshAt(ctx, tree, RefreshOptions{Commit: "one"}, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
		firstDone <- err
	}()
	<-entered
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		_, err := s.RefreshAt(ctx, tree, RefreshOptions{Commit: "two"}, func(context.Context) error {
			close(secondStarted)
			return nil
		})
		secondDone <- err
	}()
	select {
	case <-secondStarted:
		t.Fatal("the second sync ran while the first writer lock was held")
	default:
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}
