package declindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// indexView is what a reader sees of the index: its commit and the commit of its compiled part, a reference row
// (the module count and one declaration of the module that the next commit removes), a compiled row (that
// declaration's constant), and the direct uses of a declaration that the removed module uses.
type indexView struct {
	commit, compiled  string
	modules           int
	frameAt, constant bool
	frameUses         int
}

func viewOf(t *testing.T, s *Store) indexView {
	t.Helper()
	ctx := context.Background()
	var v indexView
	var err error
	if v.commit, err = s.Meta(ctx, "commit"); err != nil {
		t.Fatal(err)
	}
	if v.compiled, err = s.Meta(ctx, "compiled_commit"); err != nil {
		t.Fatal(err)
	}
	if v.modules, err = s.ModuleCount(ctx); err != nil {
		t.Fatal(err)
	}
	found, err := s.Resolve(ctx, "Gold2.FrameAt")
	if err != nil {
		t.Fatal(err)
	}
	v.frameAt = len(found) == 1
	_, err = s.Constant(ctx, "Gold2.FrameAt", "Sub.Wrap")
	var nf *NotFoundError
	if err != nil && !errors.As(err, &nf) {
		t.Fatal(err)
	}
	v.constant = err == nil
	answer, err := answerFor(t, s, "Gold.Frame", AnswerOptions{})
	if err != nil {
		t.Fatalf("answer about Gold.Frame: %v", err)
	}
	v.frameUses = answer.Direct.Uses
	return v
}

// readerAt opens readers of tg until one reads commit, and gives it open.
func readerAt(t *testing.T, tg Target, commit string) *Store {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, err := OpenReadOnly(ctx, tg, IndexOnly)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Meta(ctx, "commit")
		if err != nil {
			t.Fatal(err)
		}
		if got == commit {
			return r
		}
		_ = r.Close()
		if time.Now().After(deadline) {
			t.Fatalf("no reader read commit %s; the last read %s", commit, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var errCompiledStepStopped = errors.New("the test stops the compiled step")

// A reader of the index alone takes no lock, so a refresh can commit while it reads (a reader of the tree waits
// for the whole refresh instead: TestAReaderOfTheTreeAndARefreshExcludeEachOther). It reads one state of the index
// for its whole life, in one repeatable-read transaction: a reader opened before a refresh's commit keeps reading
// the old commit, its rows and its compiled part, and a reader opened after the commit reads the new ones. Between
// the refresh's write of the reference part and its write of the compiled part, a reader reads the new reference
// part and the old compiled part, which the readers of the compiled part refuse (TestCompiledCommitRefuses…).
func TestAReaderReadsOneStateOfTheIndexWhileARefreshWritesIt(t *testing.T) {
	ctx := context.Background()
	tree := goldenTree(t)
	tg := indexTarget(t)
	writer := openAt(t, tg)
	if _, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "one"}); err != nil {
		t.Fatal(err)
	}
	// The compiled part of one, as a compiled step leaves it: here one constant, of the module that two removes.
	if _, err := writer.pool.Exec(ctx, `INSERT INTO consts (name, module, kind, type, cstmt) VALUES ('Gold2.FrameAt', 'Sub.Wrap', 'def', 'Prop', 'hash-one')`); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(ctx, writer.pool, "compiled_commit", "one"); err != nil {
		t.Fatal(err)
	}
	old, err := OpenReadOnly(ctx, tg, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	atOne := viewOf(t, old)
	if atOne.commit != "one" || atOne.compiled != "one" || atOne.modules != 2 || !atOne.frameAt || !atOne.constant || atOne.frameUses == 0 {
		t.Fatalf("the index at one: %+v", atOne)
	}

	// Commit two removes the module Sub.Wrap.
	for _, ext := range []string{".lean", ".ilean"} {
		if err := os.Remove(filepath.Join(tree.Lib, "Sub", "Wrap"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	between, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		_, err := writer.Refresh(ctx, tree, RefreshOptions{Commit: "two", Compiled: func(context.Context) (*CompiledOptions, error) {
			close(between)
			<-release
			return nil, errCompiledStepStopped
		}})
		done <- err
	}()

	// The refresh has committed the reference part of two and stopped in its compiled step: the index holds the
	// reference part of two and the compiled part of one.
	select {
	case <-between:
	case <-time.After(60 * time.Second):
		t.Fatal("the refresh did not reach its compiled step")
	}
	fresh := readerAt(t, tg, "two")
	if got := viewOf(t, fresh); got.commit != "two" || got.compiled != "one" || got.modules != 1 || got.frameAt || got.constant ||
		got.frameUses >= atOne.frameUses {
		t.Errorf("a reader opened after the commit of two: %+v, want the reference part of two and the compiled part of one", got)
	}
	if got := viewOf(t, old); got != atOne {
		t.Errorf("the reader opened before the commit of two reads %+v, want %+v", got, atOne)
	}
	_ = fresh.Close()
	releaseOnce.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, errCompiledStepStopped) {
		t.Fatalf("the refresh: %v", err)
	}
	// The reader opened before the commit still reads the index of one, after the whole refresh ended.
	if got := viewOf(t, old); got != atOne {
		t.Errorf("after the refresh, the reader opened before its commit reads %+v, want %+v", got, atOne)
	}
}

// A refresh writes the compiled part after the reference part, in a second transaction; until it has (and after a
// compiled step that failed), the compiled part is of an earlier commit. The readers of the compiled part refuse such
// an index rather than join two commits' rows. An index that no refresh read with its compiled part has none, which
// a reader may accept (its compiled answers are empty, not mixed) or require.
func TestCompiledCommitRefusesAnIndexWhoseTwoPartsAreOfDifferentCommits(t *testing.T) {
	ctx := context.Background()
	s := refreshed(t, goldenTree(t))
	check := func(compiled string, required bool, want string) {
		t.Helper()
		if err := setMeta(ctx, s.pool, "compiled_commit", compiled); err != nil {
			t.Fatal(err)
		}
		got, err := s.CompiledCommit(ctx, required)
		switch {
		case want == "" && err == nil:
			t.Errorf("compiled part of %q, required %v: accepted at %q", compiled, required, got)
		case want == "" && compiled != "" && !strings.Contains(err.Error(), "compiled part is of "+compiled+", not of its commit c0ffee"):
			t.Errorf("compiled part of %q, required %v: %v", compiled, required, err)
		case want != "" && (err != nil || got != want):
			t.Errorf("compiled part of %q, required %v: %q, %v; want %q", compiled, required, got, err, want)
		}
	}
	check("c0ffee", true, "c0ffee")
	check("c0ffee", false, "c0ffee")
	check("", false, "c0ffee")
	check("", true, "")
	check("older", true, "")
	check("older", false, "")
}
