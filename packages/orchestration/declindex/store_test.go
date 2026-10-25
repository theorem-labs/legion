package declindex

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/indextest"
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

// indexTarget is an index of a test's own: a fresh database, which indextest drops when the test ends, and the
// schema idx in it.
func indexTarget(t *testing.T) Target {
	t.Helper()
	return Target{DSN: indextest.DSN(t), Index: "idx", TreeLock: filepath.Join(t.TempDir(), "tree.lock")}
}

// openAt opens the index tg, which Open creates when the database holds none.
func openAt(t *testing.T, tg Target) *Store {
	t.Helper()
	s, err := Open(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, indexTarget(t))
}

// execIndex runs one statement in the index's schema, for a test that writes rows no refresh would write. No
// connection of the store names a schema of its own, so the statement runs in a transaction that names it.
func execIndex(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, namedSchema(s.index)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// setIndexMeta writes one of the index's own values, for a test that puts the index in a state a refresh would
// not leave (another schema version, another compiled commit).
func setIndexMeta(t *testing.T, s *Store, k, v string) {
	t.Helper()
	execIndex(t, s, `INSERT INTO meta (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v`, k, v)
}

func refreshed(t *testing.T, tree Tree) *Store {
	t.Helper()
	s := openStore(t)
	if _, err := s.Refresh(context.Background(), tree, RefreshOptions{Commit: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// declaration is the one declaration whose full name is name; the test fails when it names none or several.
func declaration(t *testing.T, s *Store, name string) Found {
	t.Helper()
	found, err := s.Resolve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	found = slices.DeleteFunc(found, func(f Found) bool { return f.Name != name })
	if len(found) != 1 {
		t.Fatalf("%s names %d declarations: %+v", name, len(found), found)
	}
	return found[0]
}

// answerFor is the answer about the one declaration whose full name is name (declaration).
func answerFor(t *testing.T, s *Store, name string, opt AnswerOptions) (Answer, error) {
	t.Helper()
	return s.Answer(context.Background(), declaration(t, s, name), opt)
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
	for name, w := range want {
		got, err := answerFor(t, s, name, AnswerOptions{Lines: true})
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
	got, err := answerFor(t, s, "Gold.Aligned", AnswerOptions{Why: true})
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
	a, err := answerFor(t, s, "Gold.Frame", AnswerOptions{})
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
// declaration and statement: each counts as a user, and a change reaches all of them, each in its own directory.
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
	a, err := answerFor(t, s, "A.t", AnswerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Direct.Declarations != 2 || a.Direct.Directories != 2 {
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

// A refresh's step under the refresh lock (the landed sync) never runs while another refresh holds the lock: the
// second refresh waits for the lock, here until its context ends, and runs once the first is done.
func TestRefreshSerializesTheSyncStepWithTheRefreshLock(t *testing.T) {
	tree := goldenTree(t)
	s := openStore(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	var steps atomic.Int32
	s.afterRefreshLock = func(context.Context) error {
		if steps.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"})
		firstDone <- err
	}()
	<-entered
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := s.Refresh(short, tree, RefreshOptions{Commit: "two"}); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-firstDone
		t.Fatalf("a second refresh while the first held the refresh lock: %v; want it to wait", err)
	}
	if n := steps.Load(); n != 1 {
		t.Fatalf("%d steps ran under the refresh lock while the first refresh held it; want 1", n)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); err != nil || steps.Load() != 2 {
		t.Fatalf("the second refresh after the first: %v, %d steps; want 2", err, steps.Load())
	}
}

func TestOpenReadOnlyRequiresAnInitializedIndex(t *testing.T) {
	ctx := context.Background()
	tg := indexTarget(t)
	if _, err := OpenReadOnly(ctx, tg, IndexOnly); err == nil {
		t.Fatal("opened an index that no refresh made")
	}
	writer, err := Open(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(ctx, tg, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if version, err := reader.Meta(ctx, "schema"); err != nil || version != SchemaVersion {
		t.Fatalf("read-only schema: %q, %v", version, err)
	}
}

// A refresh queued behind the index's lock ends when its context ends (a SIGTERM to a queued `refresh`), rather
// than waiting for the lock.
func TestARefreshWaitingForTheIndexLockEndsWithItsContext(t *testing.T) {
	tree := goldenTree(t)
	tg := indexTarget(t)
	s := openAt(t, tg)
	holder := holdIndexLock(t, tg)
	holder.take(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a refresh whose context ended while it waited for the index's lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		holder.release(t)
		<-done
		t.Fatal("the refresh kept waiting for the index's lock after its context ended")
	}
}

// indexLock is a connection of the test's own that takes the index's advisory lock, as a refresh in another
// process would, and keeps it until the test gives it back.
type indexLock struct {
	conn  *pgx.Conn
	index string
}

func holdIndexLock(t *testing.T, tg Target) *indexLock {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), tg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return &indexLock{conn: conn, index: tg.Index}
}

func (h *indexLock) take(t *testing.T) {
	t.Helper()
	k1, k2 := lockKeys(h.index)
	if _, err := h.conn.Exec(context.Background(), `SELECT pg_advisory_lock($1, $2)`, k1, k2); err != nil {
		t.Fatal(err)
	}
}

func (h *indexLock) release(t *testing.T) {
	t.Helper()
	k1, k2 := lockKeys(h.index)
	if _, err := h.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, k1, k2); err != nil {
		t.Fatal(err)
	}
}

// A reader of the tree (the gate and the ledger, which compile against the build that a refresh's sync checks out
// and rebuilds) waits for a running refresh, and a refresh waits for it: each holds the index's advisory lock, the
// reader shared and the refresh exclusive. A reader of the index alone takes no lock, so it reads the index of the
// refresh before while the refresh syncs.
func TestAReaderOfTheTreeAndARefreshExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	tg := indexTarget(t)
	writer := openAt(t, tg)
	tree := goldenTree(t)
	if _, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	writer.afterRefreshLock = func(context.Context) error {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if reader, err := OpenReadOnly(wait, tg, IndexOnly); err != nil {
			t.Errorf("an index-only reader during a refresh's sync: %v; want it to read at once", err)
		} else {
			_ = reader.Close()
		}
		short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancelShort()
		if reader, err := OpenReadOnly(short, tg, IndexAndTree); !errors.Is(err, context.DeadlineExceeded) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Errorf("a reader of the index and the tree during a refresh's sync: %v; want it to wait", err)
		}
		return nil
	}
	_, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "two"})
	writer.afterRefreshLock = nil
	if err != nil {
		t.Fatalf("the refresh: %v", err)
	}
	reader, err := OpenReadOnly(ctx, tg, IndexAndTree)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := writer.Refresh(short, tree, RefreshOptions{Commit: "three"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a refresh while a reader of the tree holds the index: %v; want it to wait", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "three"}); err != nil {
		t.Fatalf("the refresh after the reader closed: %v", err)
	}
}

// Open rebuilds an index of another schema version under the index's lock, so that no refresh writes it while it is
// rebuilt and no second Open rebuilds it at the same time. An index of this schema needs no write, so Open of it
// does not wait.
func TestOpenRebuildsAnOlderSchemaOnlyUnderTheIndexLock(t *testing.T) {
	ctx := context.Background()
	dsn := indextest.DSN(t)
	current, older := Target{DSN: dsn, Index: "idx"}, Target{DSN: dsn, Index: "older"}
	for _, tg := range []Target{current, older} {
		s, err := Open(ctx, tg)
		if err != nil {
			t.Fatal(err)
		}
		if tg == older {
			setIndexMeta(t, s, "schema", "3")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open := func(tg Target) chan error {
		done := make(chan error, 1)
		go func() {
			s, err := Open(ctx, tg)
			if err == nil {
				err = s.Close()
			}
			done <- err
		}()
		return done
	}
	currentLock, olderLock := holdIndexLock(t, current), holdIndexLock(t, older)
	currentLock.take(t)
	select {
	case err := <-open(current):
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Open of an index of this schema waited for the index's lock")
	}
	currentLock.release(t)
	olderLock.take(t)
	done := open(older)
	select {
	case err := <-done:
		t.Fatalf("Open rebuilt an older index while its lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	olderLock.release(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not rebuild the older index after its lock was released")
	}
}

// Each refresh that moves the index to another commit records that landing with the commit it replaced, so a
// post-landing check of one landing compares the same two commits however many landings came after it.
func TestRefreshRecordsEachLandingWithItsPreviousCommit(t *testing.T) {
	tree := goldenTree(t)
	s := openStore(t)
	ctx := context.Background()
	for _, commit := range []string{"one", "two", "two", "three"} {
		if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: commit}); err != nil {
			t.Fatal(err)
		}
	}
	for commit, want := range map[string]string{"one": "", "two": "one", "three": "two"} {
		previous, ok, err := s.Landing(ctx, commit)
		if err != nil || !ok || previous != want {
			t.Errorf("landing %s: previous %q, recorded %v, %v; want %q", commit, previous, ok, err, want)
		}
	}
	if _, ok, err := s.Landing(ctx, "four"); err != nil || ok {
		t.Fatalf("an unrecorded landing: recorded %v, %v", ok, err)
	}
}

// An index of an older schema has no consts.binders or consts.conclusion column. Open reads its version before it
// creates this schema over it, since the index on consts.conclusion cannot be made on its consts table, and
// rebuilds the schema from nothing. An older schema cannot be written through the store, so the test makes one
// from this one with SQL.
func TestOpenRebuildsAnIndexOfAnOlderSchema(t *testing.T) {
	ctx := context.Background()
	target := indexTarget(t)
	s := openAt(t, target)
	tree := goldenTree(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX consts_conclusion`,
		`ALTER TABLE consts DROP COLUMN conclusion`,
		`ALTER TABLE consts DROP COLUMN binders`,
	} {
		execIndex(t, s, statement)
	}
	setIndexMeta(t, s, "schema", "13")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt := openAt(t, target)
	if version, err := rebuilt.Meta(ctx, "schema"); err != nil || version != SchemaVersion {
		t.Fatalf("schema after the rebuild = %q, %v; want %q", version, err, SchemaVersion)
	}
	if _, err := rebuilt.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	writeConstants(t, rebuilt, []string{"Golden"}, []Constant{{Name: "Gold.frame_al", Module: "Golden", Kind: "theorem",
		Hash: "h", Binders: []string{"Gold.Frame"}, Conclusion: "Gold.Aligned"}})
	if got, err := rebuilt.ConstantsConcluding(ctx, []string{"Gold.Aligned"}); err != nil || len(got) != 1 {
		t.Fatalf("ConstantsConcluding after the rebuild = %+v, %v; want one row", got, err)
	}
}
