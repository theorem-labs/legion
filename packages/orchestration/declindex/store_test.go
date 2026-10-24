package declindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
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
	path := filepath.Join(t.TempDir(), "index.sqlite")
	if _, err := OpenReadOnly(context.Background(), path, IndexOnly); err == nil {
		t.Fatal("opened a missing index read-only")
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(context.Background(), path, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if version, err := reader.Meta(context.Background(), "schema"); err != nil || version != SchemaVersion {
		t.Fatalf("read-only schema: %q, %v", version, err)
	}
}

// A refresh queued behind the refresh lock ends when its context ends (a SIGTERM to a queued `refresh`), rather
// than waiting for the lock.
func TestARefreshWaitingForTheRefreshLockEndsWithItsContext(t *testing.T) {
	tree := goldenTree(t)
	s := openStore(t)
	holder, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
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
			t.Fatalf("a refresh whose context ended while it waited for the lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		<-done
		t.Fatal("the refresh kept waiting for the refresh lock after its context ended")
	}
}

// The index directory of a post-landing check is read-only. The index there opens read-only, and a refresh waits
// until the reader is done, so the reader never sees a refresh half written.
func TestOpenReadOnlyReadsAnIndexInAReadOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "index.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tree := goldenTree(t)
	if _, err := writer.Refresh(context.Background(), tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	reader, err := OpenReadOnly(context.Background(), path, IndexOnly)
	if err != nil {
		t.Fatalf("open read-only in a read-only directory: %v", err)
	}
	if commit, err := reader.Meta(context.Background(), "commit"); err != nil || commit != "one" {
		t.Fatalf("commit %q, %v", commit, err)
	}
	if n, err := reader.ModuleCount(context.Background()); err != nil || n != 2 {
		t.Fatalf("modules %d, %v", n, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := other.Refresh(ctx, tree, RefreshOptions{Commit: "two"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a refresh while the read-only reader is open: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Refresh(context.Background(), tree, RefreshOptions{Commit: "two"}); err != nil {
		t.Fatalf("a refresh after the reader closed: %v", err)
	}
}

// lockHolder is a test's own open file of one of the index's lock files, which it locks exclusively as a refresh
// or a write of the index would.
type lockHolder struct{ f *os.File }

func openLockHolder(t *testing.T, path string) lockHolder {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return lockHolder{f}
}

func (h lockHolder) lock(t *testing.T) {
	t.Helper()
	if err := syscall.Flock(int(h.f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
}

func (h lockHolder) unlock(t *testing.T) {
	t.Helper()
	if err := syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

// In a read-only index directory a reader holds, shared, the locks of what it reads. A reader of the index alone
// (land-hash) holds only the write lock, which a refresh holds only while it writes the index, so it does not wait
// while a refresh holds its refresh lock for its fetch, checkout and build. A reader that also reads the tree
// (ledger, the gate) waits for both locks.
func TestAReadOnlyReaderWaitsForTheLocksOfWhatItReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "index.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Refresh(context.Background(), goldenTree(t), RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	refreshLock, writeLock := openLockHolder(t, path+".lock"), openLockHolder(t, path+".write.lock")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	read := func(scope ReadScope, wait time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		reader, err := OpenReadOnly(ctx, path, scope)
		if err != nil {
			return err
		}
		defer reader.Close()
		if commit, err := reader.Meta(ctx, "commit"); err != nil || commit != "one" {
			return fmt.Errorf("read commit %q, %v", commit, err)
		}
		return nil
	}
	readers := map[ReadScope]string{IndexOnly: "an index-only reader", IndexAndTree: "a reader of the index and the tree"}

	refreshLock.lock(t)
	if err := read(IndexOnly, 5*time.Second); err != nil {
		t.Errorf("an index-only reader while a refresh holds its refresh lock: %v; want it to read at once", err)
	}
	if err := read(IndexAndTree, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a reader of the index and the tree while a refresh holds its refresh lock: %v; want it to wait", err)
	}
	refreshLock.unlock(t)

	writeLock.lock(t)
	for scope, reader := range readers {
		if err := read(scope, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s while a refresh writes the index: %v; want it to wait", reader, err)
		}
	}
	writeLock.unlock(t)

	for scope, reader := range readers {
		if err := read(scope, 5*time.Second); err != nil {
			t.Errorf("%s with both locks free: %v", reader, err)
		}
	}
}

// The case that a landing's hasher meets: a refresh is in its sync (fetch, checkout and build) under its refresh
// lock. A reader of the index alone in a read-only directory reads the index of the refresh before, at once; a
// reader of the tree, which the sync changes, waits for the refresh.
func TestAnIndexOnlyReaderReadsWhileARefreshSyncs(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "index.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tree := goldenTree(t)
	if _, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	synced := false
	writer.afterRefreshLock = func(context.Context) error {
		synced = true
		if err := os.Chmod(dir, 0o555); err != nil {
			return err
		}
		defer func() { _ = os.Chmod(dir, 0o755) }()
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if reader, err := OpenReadOnly(wait, path, IndexOnly); err != nil {
			t.Errorf("an index-only reader during a refresh's sync: %v; want it to read the index of the refresh before", err)
		} else {
			if commit, err := reader.Meta(ctx, "commit"); err != nil || commit != "one" {
				t.Errorf("an index-only reader during a refresh's sync read commit %q, %v; want one", commit, err)
			}
			_ = reader.Close()
		}
		short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancelShort()
		if reader, err := OpenReadOnly(short, path, IndexAndTree); !errors.Is(err, context.DeadlineExceeded) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Errorf("a reader of the index and the tree during a refresh's sync: %v; want it to wait for the refresh", err)
		}
		return nil
	}
	_, err = writer.Refresh(ctx, tree, RefreshOptions{Commit: "two"})
	writer.afterRefreshLock = nil
	if err != nil || !synced {
		t.Fatalf("the refresh: %v, synced %v", err, synced)
	}
	if commit, err := writer.Meta(ctx, "commit"); err != nil || commit != "two" {
		t.Fatalf("commit after the refresh: %q, %v", commit, err)
	}
}

// In a writable index directory too, a reader of the tree (the gate and the ledger, which compile against the build
// that a refresh's sync checks out and rebuilds) waits for a running refresh, and a refresh waits for it. A reader of
// the index alone still reads the index of the refresh before while the refresh syncs.
func TestAReaderOfTheTreeAndARefreshExcludeEachOtherInAWritableDirectory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "index.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tree := goldenTree(t)
	if _, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	writer.afterRefreshLock = func(context.Context) error {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if reader, err := OpenReadOnly(wait, path, IndexOnly); err != nil {
			t.Errorf("an index-only reader during a refresh's sync: %v; want it to read at once", err)
		} else {
			_ = reader.Close()
		}
		short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancelShort()
		if reader, err := OpenReadOnly(short, path, IndexAndTree); !errors.Is(err, context.DeadlineExceeded) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Errorf("a reader of the index and the tree during a refresh's sync in a writable directory: %v; want it to wait", err)
		}
		return nil
	}
	_, err = writer.Refresh(ctx, tree, RefreshOptions{Commit: "two"})
	writer.afterRefreshLock = nil
	if err != nil {
		t.Fatalf("the refresh: %v", err)
	}
	reader, err := OpenReadOnly(ctx, path, IndexAndTree)
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

// A symlinked index file is one index whichever path names it: its lock files and its graph are beside the file it
// resolves to. So a reader of the tree that opens the resolved path, as the gate does (Config.GateIndexPath), waits
// for a refresh that opened the link, and reads the graph that refresh wrote.
func TestASymlinkedIndexFileHasOneRefreshLockAndOneGraph(t *testing.T) {
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "index.sqlite")
	created, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "index.sqlite")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(link)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.afterRefreshLock = func(context.Context) error {
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if reader, err := OpenReadOnly(short, target, IndexAndTree); !errors.Is(err, context.DeadlineExceeded) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Errorf("a reader of the resolved index during a refresh through its link: %v; want it to wait", err)
		}
		return nil
	}
	_, err = writer.Refresh(ctx, goldenTree(t), RefreshOptions{Commit: "one"})
	writer.afterRefreshLock = nil
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(ctx, target, IndexAndTree)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Answer(ctx, "Gold.Frame", AnswerOptions{}); err != nil {
		t.Errorf("an answer of the resolved index after a refresh through its link: %v", err)
	}
}

// Open rebuilds an index of another schema under the write lock, so that a rebuild never drops the tables under a
// reader in a read-only directory. An index of this schema needs no write, so Open of it does not wait.
func TestOpenRebuildsAnOlderSchemaOnlyUnderTheWriteLock(t *testing.T) {
	ctx := context.Background()
	current := filepath.Join(t.TempDir(), "index.sqlite")
	older := filepath.Join(t.TempDir(), "index.sqlite")
	for _, path := range []string{current, older} {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if path == older {
			if err := s.setMeta(ctx, s.db, "schema", "3"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open := func(path string) chan error {
		done := make(chan error, 1)
		go func() {
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			done <- err
		}()
		return done
	}
	currentLock, olderLock := openLockHolder(t, current+".write.lock"), openLockHolder(t, older+".write.lock")
	currentLock.lock(t)
	select {
	case err := <-open(current):
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Open of an index of this schema waited for the write lock")
	}
	currentLock.unlock(t)
	olderLock.lock(t)
	done := open(older)
	select {
	case err := <-done:
		t.Fatalf("Open rebuilt an older index while its write lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	olderLock.unlock(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not rebuild the older index after its write lock was released")
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
