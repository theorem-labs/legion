package declindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/internal/indextest"
)

// pgIndex opens an index of a database of its own, closed when the test ends.
func pgIndex(t *testing.T) (*Store, Target) {
	t.Helper()
	target := Target{DSN: indextest.DSN(t), Index: "idx"}
	s, err := Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, target
}

// pgEmptyTree is a tree with no build output: a refresh of it writes the index's meta rows and nothing else, so
// a test of the locks and the transactions needs no Lean.
func pgEmptyTree(t *testing.T) Tree {
	t.Helper()
	dir := t.TempDir()
	return Tree{Source: dir, Lib: dir, Roots: Roots{}}
}

// pgTerminate ends the server session of pid, as an operator or a failing network does.
func pgTerminate(t *testing.T, dsn string, pid uint32) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var ended bool
	if err := conn.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, int32(pid)).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if !ended {
		t.Fatalf("no session %d to end", pid)
	}
}

// pgWrite writes the index as a refresh does: under its lock, on the connection that holds it.
func pgWrite(t *testing.T, s *Store, f func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.writeTx(ctx, f); err != nil {
		t.Fatal(err)
	}
}

// The index's names order and compare by their bytes, as Lean's own name order does: `A.b` is below `A/b`, which
// is below `A_b` and `Ab`, and the range of a declaration's generated parts (`A.` to `A/`) holds exactly the
// names below it. A database of another collation would order and group them differently.
func TestTheIndexOrdersNamesByTheirBytes(t *testing.T) {
	s, _ := pgIndex(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO decls (name, short, module, kind, line, stmt_hash)
		VALUES ('A', 'A', 'M', 'theorem', 1, 'h')`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Ab", "A_b", "A/b", "A.b", "A"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO uses (target, tmodule, module, line, col, parent, cls)
			VALUES ($1, 'M', 'U', 1, 1, 'User', 'proof')`, name); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO decls (name, short, module, kind, line, stmt_hash)
			VALUES ($1, $1, 'U', 'theorem', 1, 'h')`, name); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	rows, err := s.pool.Query(ctx, `SELECT name FROM decls WHERE module = 'U' ORDER BY name`)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		order = append(order, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "A.b", "A/b", "A_b", "Ab"}; strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("the index ordered the names %v, not %v", order, want)
	}
	var inRange []string
	rows, err = s.pool.Query(ctx, `SELECT target FROM uses WHERE target >= 'A.' AND target < 'A/' ORDER BY target`)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		inRange = append(inRange, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(inRange) != 1 || inRange[0] != "A.b" {
		t.Errorf("the range of A's generated parts holds %v, not [A.b]", inRange)
	}
	uses, err := s.usersOf(ctx, declRef{"M", "A"})
	if err != nil {
		t.Fatal(err)
	}
	// The uses of A and of A.b are A's; A/b, A_b and Ab are other declarations.
	if len(uses) != 2 {
		t.Errorf("A has %d uses, not the 2 of A and A.b", len(uses))
	}
}

// Two refreshes of one index never interleave, and a reader of the tree (the gate, the ledger) waits for the
// whole refresh while a reader of the index alone reads the state before it.
func TestTwoRefreshesSerializeAndATreeReaderWaitsForOne(t *testing.T) {
	first, target := pgIndex(t)
	ctx := context.Background()
	tree := pgEmptyTree(t)
	held, release := make(chan struct{}), make(chan struct{})
	first.afterRefreshLock = func(context.Context) error {
		close(held)
		<-release
		return nil
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Refresh(ctx, tree, RefreshOptions{Commit: "c0ffee"})
		firstDone <- err
	}()
	<-held
	second, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	secondDone := make(chan error, 1)
	go func() {
		_, err := second.Refresh(ctx, tree, RefreshOptions{Commit: "beef"})
		secondDone <- err
	}()
	readerDone := make(chan error, 1)
	go func() {
		r, err := OpenReadOnly(ctx, target, IndexAndTree)
		if err == nil {
			_ = r.Close()
		}
		readerDone <- err
	}()
	// A reader of the index alone takes no lock: it reads the state the first refresh has not replaced yet.
	indexOnly, err := OpenReadOnly(ctx, target, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	if commit, err := indexOnly.Meta(ctx, "commit"); err != nil || commit != "" {
		t.Errorf("the reader of the index alone read commit %q, %v, and the first refresh has not committed yet", commit, err)
	}
	_ = indexOnly.Close()
	select {
	case err := <-secondDone:
		t.Fatalf("the second refresh did not wait for the first: %v", err)
	case err := <-readerDone:
		t.Fatalf("the reader of the tree did not wait for the refresh: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	for _, done := range []chan error{firstDone, secondDone, readerDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a refresh or a reader did not finish once the first refresh gave up the lock")
		}
	}
	commit, err := second.Meta(ctx, "commit")
	if err != nil {
		t.Fatal(err)
	}
	if commit != "beef" {
		t.Errorf("the index's commit is %q: the second refresh did not run after the first", commit)
	}
}

// A refresh that loses the connection holding its lock writes nothing: the server rolls back its open
// transaction and gives up its lock together, and the error says the database is unreachable (exit 75).
func TestARefreshThatLosesItsConnectionWritesNothing(t *testing.T) {
	s, target := pgIndex(t)
	ctx := context.Background()
	tree := pgEmptyTree(t)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	s.afterRefreshLock = func(context.Context) error {
		pgTerminate(t, target.DSN, s.refresh.Conn().PgConn().PID())
		return nil
	}
	_, err := s.Refresh(ctx, tree, RefreshOptions{Commit: "beef"})
	if err == nil {
		t.Fatal("the refresh reported no error although its connection was gone")
	}
	if !Unreachable(err) {
		t.Errorf("the refresh error %v is not an unreachable database, so the command would not exit 75", err)
	}
	s.afterRefreshLock = nil

	// A write transaction that the lost connection carried is rolled back with it.
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.writeTx(ctx, func(tx pgx.Tx) error {
		if err := setMeta(ctx, tx, "commit", "beef"); err != nil {
			return err
		}
		pgTerminate(t, target.DSN, s.refresh.Conn().PgConn().PID())
		return setMeta(ctx, tx, "built", "2026-10-25T00:00Z")
	})
	unlock()
	if err == nil {
		t.Fatal("the write reported no error although its connection was gone")
	}
	if !Unreachable(err) {
		t.Errorf("the write error %v is not an unreachable database", err)
	}
	after, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = after.Close() }()
	commit, err := after.Meta(ctx, "commit")
	if err != nil {
		t.Fatal(err)
	}
	if commit != "c0ffee" {
		t.Errorf("the index's commit is %q: a refresh that lost its connection wrote it", commit)
	}
}

// A refresh writes the reference part and the compiled part in two transactions. A reader between them holds one
// commit's declarations and the commit before's compiled rows, and CompiledCommit refuses to join them.
func TestAReaderBetweenARefreshTwoWritesIsRefused(t *testing.T) {
	s, target := pgIndex(t)
	ctx := context.Background()
	pgWrite(t, s, func(tx pgx.Tx) error {
		if err := setMeta(ctx, tx, "commit", "c1"); err != nil {
			return err
		}
		return setMeta(ctx, tx, "compiled_commit", "c1")
	})
	early, err := OpenReadOnly(ctx, target, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = early.Close() }()
	if commit, err := early.CompiledCommit(ctx, true); err != nil || commit != "c1" {
		t.Fatalf("the reader of one whole commit read %q, %v", commit, err)
	}
	// The reference part of the next commit lands first.
	pgWrite(t, s, func(tx pgx.Tx) error { return setMeta(ctx, tx, "commit", "c2") })
	between, err := OpenReadOnly(ctx, target, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = between.Close() }()
	_, err = between.CompiledCommit(ctx, true)
	if err == nil {
		t.Fatal("a reader between the two writes of a refresh joined two commits' rows")
	}
	if !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "c2") {
		t.Errorf("the error %v does not name the commit of each part", err)
	}
	// The reader that began before the second write still reads one state of the index.
	if commit, err := early.CompiledCommit(ctx, true); err != nil || commit != "c1" {
		t.Errorf("the earlier reader read %q, %v, and its snapshot holds c1", commit, err)
	}
	pgWrite(t, s, func(tx pgx.Tx) error { return setMeta(ctx, tx, "compiled_commit", "c2") })
	after, err := OpenReadOnly(ctx, target, IndexOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = after.Close() }()
	if commit, err := after.CompiledCommit(ctx, true); err != nil || commit != "c2" {
		t.Errorf("the reader of the whole commit read %q, %v", commit, err)
	}
}

// A login that holds the reader role alone reads the index, and goes on reading it after a refresh rebuilt the
// schema; it cannot write a row of it.
func TestTheReaderRoleReadsARebuiltIndexAndCannotWriteIt(t *testing.T) {
	ctx := context.Background()
	dsn := indextest.DSN(t)
	role, readerDSN := indextest.ReaderRole(t, dsn)
	target := Target{DSN: dsn, Index: "idx", Reader: role}
	s, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	// An index of another schema version: the next Open rebuilds it from nothing, and grants its reads again.
	if err := setMeta(ctx, s.pool, "schema", "13"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	if _, err := rebuilt.pool.Exec(ctx, `INSERT INTO modules (rel, module, ilean_key) VALUES ('Gold', 'Gold', 'k')`); err != nil {
		t.Fatal(err)
	}
	readerTarget := Target{DSN: readerDSN, Index: "idx"}
	reader, err := OpenReadOnly(ctx, readerTarget, IndexOnly)
	if err != nil {
		t.Fatalf("the reader role cannot open the rebuilt index: %v", err)
	}
	defer func() { _ = reader.Close() }()
	n, err := reader.ModuleCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the reader counted %d modules, not 1", n)
	}
	pool, err := openPool(ctx, readerTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `INSERT INTO meta (k, v) VALUES ('commit', 'beef')`)
	if err == nil {
		t.Fatal("the reader role wrote the index")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("the reader role's write failed with %v, not for want of privilege", err)
	}
}

// A reader of an index of another schema version, or of one no refresh ever wrote, is refused with the message
// that says what to run.
func TestAReaderOfAnotherSchemaVersionIsRefused(t *testing.T) {
	s, target := pgIndex(t)
	ctx := context.Background()
	if err := setMeta(ctx, s.pool, "schema", "13"); err != nil {
		t.Fatal(err)
	}
	_, err := OpenReadOnly(ctx, target, IndexOnly)
	if err == nil {
		t.Fatal("a reader read an index of another schema version")
	}
	if !strings.Contains(err.Error(), `"13"`) || !strings.Contains(err.Error(), SchemaVersion) {
		t.Errorf("the error %v does not say which schema the index has and which this declindex reads", err)
	}
	_, err = OpenReadOnly(ctx, Target{DSN: target.DSN, Index: "never"}, IndexOnly)
	if err == nil {
		t.Fatal("a reader read an index that no refresh ever wrote")
	}
	if !strings.Contains(err.Error(), "never refreshed") {
		t.Errorf("the error %v does not say that the index was never refreshed", err)
	}
}

// A database that cannot be reached is told apart from an answer of the database: the commands exit 75 for it, so
// a post-landing hook is run again rather than counted as a defect of what it checked.
func TestAnUnreachableDatabaseIsReportedAsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Port 1 is not a Postgres server on any host of ours.
	target := Target{DSN: "postgres://postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=2", Index: "idx"}
	if _, err := Open(ctx, target); err == nil {
		t.Error("Open reported no error for a server that is not there")
	} else if !Unreachable(err) {
		t.Errorf("Open's error %v is not an unreachable database", err)
	}
	if _, err := OpenReadOnly(ctx, target, IndexOnly); err == nil {
		t.Error("OpenReadOnly reported no error for a server that is not there")
	} else if !Unreachable(err) {
		t.Errorf("OpenReadOnly's error %v is not an unreachable database", err)
	}
	if Unreachable(errors.New("the index's compiled part is of c1")) {
		t.Error("an answer of the database was reported as an unreachable one")
	}
}
