package declindex

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/indextest"
)

// poolerDSN is a connection string through a stand-in for a transaction pooler: it reads each client's startup
// message, refuses a connection that asks for a parameter it does not carry (PgBouncer answers "unsupported
// startup parameter", SQLSTATE 08P01, and closes), and otherwise passes the bytes of the session on. The
// shared server's pooler carries none of ours, so every parameter but the fixed ones is refused.
func poolerDSN(t *testing.T, dsn string) string {
	t.Helper()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host := target.Host
	if !strings.Contains(host, ":") {
		host += ":5432"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				poolerSession(client, host)
			}()
		}
	}()
	through := *target
	through.Host = listener.Addr().String()
	return through.String()
}

// carried are the startup parameters a transaction pooler passes to the server; `search_path` is not one of them,
// because a server connection serves one client after another.
var carried = map[string]bool{
	"user": true, "database": true, "client_encoding": true, "application_name": true,
	"DateStyle": true, "TimeZone": true, "standard_conforming_strings": true, "extra_float_digits": true,
	"options": true, "replication": true,
}

// poolerSession reads one client's startup message. A parameter the pooler does not carry ends the connection
// with the error PgBouncer gives; otherwise the session is passed to the server, startup message included.
func poolerSession(client net.Conn, server string) {
	defer func() { _ = client.Close() }()
	var size [4]byte
	if _, err := io.ReadFull(client, size[:]); err != nil {
		return
	}
	length := int(binary.BigEndian.Uint32(size[:]))
	if length < 8 || length > 1<<16 {
		return
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(client, body); err != nil {
		return
	}
	// After the protocol version, the body is NUL-terminated keys and values. An SSL or cancel request has no
	// parameters, and pgx sends one of those first when sslmode allows it.
	if version := binary.BigEndian.Uint32(body[:4]); version == 196608 {
		fields := strings.Split(string(body[4:]), "\x00")
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "" || carried[fields[i]] {
				continue
			}
			// ErrorResponse: 'E', length, then NUL-terminated fields, then a NUL.
			fatal := "SFATAL\x00C08P01\x00Munsupported startup parameter: " + fields[i] + "\x00\x00"
			out := make([]byte, 5, 5+len(fatal))
			out[0] = 'E'
			binary.BigEndian.PutUint32(out[1:], uint32(4+len(fatal)))
			_, _ = client.Write(append(out, fatal...))
			return
		}
	}
	conn, err := net.Dial("tcp", server)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(append(size[:], body...)); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(conn, client); close(done) }()
	_, _ = io.Copy(client, conn)
	<-done
}

// The owners' pods reach the shared server through a transaction pooler, which refuses a connection that asks for
// a startup parameter it does not carry. So the index's schema cannot be named in the startup message: a reader
// names it inside its own transaction, where a pooler keeps it to that transaction.
func TestAReaderAnswersThroughAPoolerThatCarriesNoStartupParameter(t *testing.T) {
	ctx := context.Background()
	dsn := indextest.DSN(t)
	s := openAt(t, Target{DSN: dsn, Index: "idx"})
	if _, err := s.Refresh(ctx, goldenTree(t), RefreshOptions{Commit: "c0ffee"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	r, err := OpenReadOnly(ctx, Target{DSN: poolerDSN(t, dsn), Index: "idx"}, IndexOnly)
	if err != nil {
		t.Fatalf("open the index read-only through a pooler: %v", err)
	}
	defer func() { _ = r.Close() }()
	found, err := r.Resolve(ctx, "Gold.Frame")
	if err != nil {
		t.Fatalf("resolve through a pooler: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the index answered no declaration of Gold.Frame through a pooler")
	}
	a, err := r.Answer(ctx, found[0], AnswerOptions{})
	if err != nil {
		t.Fatalf("answer through a pooler: %v", err)
	}
	if a.Reach.StatementsChanged == 0 {
		t.Errorf("reach of Gold.Frame through a pooler: %+v", a.Reach.ReachCounts)
	}
}

// A write names the index's schema in its own transaction, so that it writes the index whatever the session it
// runs on holds: a route that pools transactions gives a transaction a backend that never saw what the session
// set, and a refresh that wrote the reference part on one backend and the compiled part on another wrote the
// second into no schema at all ("relation \"consts\" does not exist").
func TestAWriteNamesTheIndexSchemaInItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The session loses the schema it named, as it does when another backend answers.
	if _, err := s.refresh.Exec(ctx, `SET search_path = pg_catalog`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.writeTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('search_path')`).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	if got != `"idx"` && got != "idx" {
		t.Errorf("a write transaction read the schema %q, want the index's own", got)
	}
}

// Every route to the shared server pools transactions, so a lock of the server lives only as long as the
// transaction that took it: each write transaction takes the index's lock itself, and a second writer waits for
// the first transaction to commit, whatever connection or machine it came from.
func TestAWriteTransactionHoldsTheIndexLockUntilItCommits(t *testing.T) {
	ctx := context.Background()
	target := indexTarget(t)
	first, second := openAt(t, target), openAt(t, target)
	unlock, err := first.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The second writer takes no tree lock of its own (a refresh on another machine), so only the index's
	// lock holds it back.
	conn, err := second.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	second.refresh = conn
	waiting := make(chan error, 1)
	started := make(chan struct{})
	if err := first.writeTx(ctx, func(pgx.Tx) error {
		go func() {
			close(started)
			waiting <- second.writeTx(ctx, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT 1`)
				return err
			})
		}()
		<-started
		select {
		case err := <-waiting:
			t.Errorf("a second write ran while the first transaction held the index's lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatalf("the second write after the first committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second write did not run after the first transaction committed")
	}
}

// A refresh with lean.export whose compiled step never finishes leaves an index with no compiled part at all,
// not one of an earlier commit: states and same would then answer that nothing states a concept, which a caller
// cannot tell from an index that is complete. Such an index is refused, and the refusal names what to run. The
// acceptance tester met this on the shared server, where the first compiled step of a new index failed.
func TestAnIndexWhoseFirstCompiledStepFailedIsRefused(t *testing.T) {
	ctx := context.Background()
	s := refreshed(t, goldenTree(t))
	if _, err := s.CompiledCommit(ctx, false); err != nil {
		t.Fatalf("an index read without a compiled part (lean.export off): %v", err)
	}
	// A refresh that reads the compiled part and fails there: its reference part commits, its compiled part
	// never does.
	compiled := func(context.Context) (*CompiledOptions, error) { return nil, errors.New("no toolchain here") }
	if _, err := s.Refresh(ctx, goldenTree(t), RefreshOptions{Commit: "beef", Compiled: compiled}); err == nil {
		t.Fatal("a refresh whose compiled step failed reported no error")
	}
	got, err := s.CompiledCommit(ctx, false)
	if err == nil {
		t.Fatalf("an index whose compiled step never wrote a compiled part was accepted at %q", got)
	}
	if !strings.Contains(err.Error(), "no compiled part") {
		t.Errorf("the refusal does not say that the compiled part is missing: %v", err)
	}
}

// The same outside Git, where a refresh records an empty commit: an index whose compiled step failed then has an
// empty commit and an empty compiled commit, which are equal, and must still be refused. The cross-family review
// reproduced it with the command itself: refresh exited 3, then states answered "nothing states it", exit 1.
func TestAnIndexOutsideGitWhoseFirstCompiledStepFailedIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	compiled := func(context.Context) (*CompiledOptions, error) { return nil, errors.New("no toolchain here") }
	if _, err := s.Refresh(ctx, goldenTree(t), RefreshOptions{Compiled: compiled}); err == nil {
		t.Fatal("a refresh whose compiled step failed reported no error")
	}
	if got, err := s.CompiledCommit(ctx, false); err == nil {
		t.Fatalf("an index outside Git whose compiled step never wrote a compiled part was accepted at %q", got)
	}
}

// logWriter runs f on every message a refresh writes.
type logWriter struct{ f func() }

func (w logWriter) Write(p []byte) (int, error) {
	w.f()
	return len(p), nil
}

// Two refreshes of one index on two machines hold two file locks, so neither waits for the other, and a refresh
// plans its write (which modules changed, which are gone, which commit it replaces) from reads it made before
// its write transaction took the index's lock. A write that another refresh outdated would publish a commit with
// another refresh's modules; it is refused, and the refresh asks to be run again (exit 75).
func TestARefreshRefusesToWriteAPlanThatAnotherRefreshOutdated(t *testing.T) {
	ctx := context.Background()
	first := indexTarget(t)
	second := first
	second.TreeLock = filepath.Join(t.TempDir(), "tree.lock") // another machine's lock of its own copy
	a, b := openAt(t, first), openAt(t, second)
	tree := goldenTree(t)
	if _, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "base"}); err != nil {
		t.Fatal(err)
	}
	// B refreshes while A has read the index and not yet written it: A's first message comes after its read.
	var once sync.Once
	var bErr error
	log := logWriter{f: func() {
		once.Do(func() { _, bErr = b.Refresh(ctx, tree, RefreshOptions{Commit: "two"}) })
	}}
	_, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "three", Log: log})
	if bErr != nil {
		t.Fatalf("the second refresh: %v", bErr)
	}
	if err == nil {
		t.Fatal("a refresh wrote a plan that another refresh had outdated")
	}
	var temp *TempError
	if !errors.As(err, &temp) {
		t.Errorf("the refusal is not a retry: %v", err)
	}
	if commit, err := a.Meta(ctx, "commit"); err != nil || commit != "two" {
		t.Errorf("the index's commit is %q, %v; want the one the refresh that wrote it recorded", commit, err)
	}
}

// The compiled step plans its write from the modules the reference part read, so a refresh that another refresh
// outdated while its exporter ran must not write its compiled part either: it would leave the index's reference
// commit and its compiled commit of two different refreshes, which every compiled reader then refuses.
func TestARefreshRefusesToWriteACompiledPartThatAnotherRefreshOutdated(t *testing.T) {
	ctx := context.Background()
	tree, co := compiledTree(t)
	first := indexTarget(t)
	second := first
	second.TreeLock = filepath.Join(t.TempDir(), "tree.lock") // another machine's lock of its own copy
	a, b := openAt(t, first), openAt(t, second)
	if _, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "base", Compiled: compiledWith(co)}); err != nil {
		t.Fatal(err)
	}
	// B refreshes while A's exporter runs, after A's reference part has committed.
	var bErr error
	compiled := func(context.Context) (*CompiledOptions, error) {
		_, bErr = b.Refresh(ctx, tree, RefreshOptions{Commit: "two", Compiled: compiledWith(co)})
		return co, nil
	}
	_, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "three", Compiled: compiled})
	if bErr != nil {
		t.Fatalf("the second refresh: %v", bErr)
	}
	var temp *TempError
	if !errors.As(err, &temp) {
		t.Fatalf("a refresh outdated before its compiled step: %v; want a retry", err)
	}
}

// A landing hook decides whether the index already covers its commit before it fetches and builds. A slower
// landing of an earlier commit must not publish it after a later landing has published its own: the index would
// go backwards while both hooks reported success.
func TestASlowLandingDoesNotOverwriteALaterOne(t *testing.T) {
	ctx := context.Background()
	first := indexTarget(t)
	second := first
	second.TreeLock = filepath.Join(t.TempDir(), "tree.lock")
	a, b := openAt(t, first), openAt(t, second)
	tree := goldenTree(t)
	gitIn(t, tree.Source, "init", "-q")
	gitIn(t, tree.Source, "add", "-A")
	gitIn(t, tree.Source, "commit", "-q", "-m", "base")
	if _, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "c0"}); err != nil {
		t.Fatal(err)
	}
	// A lands c1: it decides that c0 does not cover c1, and then its fetch and build are slow. B lands c2 in
	// that time and finishes.
	var bErr error
	landed := func(context.Context) error {
		_, bErr = b.Refresh(ctx, tree, RefreshOptions{Commit: "c2", Landed: func(context.Context) error { return nil }})
		return nil
	}
	_, err := a.Refresh(ctx, tree, RefreshOptions{Commit: "c1", Landed: landed})
	if bErr != nil {
		t.Fatalf("the later landing: %v", bErr)
	}
	var temp *TempError
	if !errors.As(err, &temp) {
		t.Errorf("the slower landing of c1: %v; want a retry", err)
	}
	if commit, err := a.Meta(ctx, "commit"); err != nil || commit != "c2" {
		t.Errorf("the index is at %q, %v; want the later landing's commit", commit, err)
	}
}

// A landing hook skips its refresh when the index already covers its commit. An index that owes a compiled part
// is not covered, whatever its two commit markers say: every compiled reader refuses it, so a landing that
// skipped would report success while the index states no fact, run after run.
func TestALandingDoesNotSkipAnIndexThatOwesACompiledPart(t *testing.T) {
	ctx := context.Background()
	tree, co := compiledTree(t)
	gitIn(t, tree.Source, "init", "-q")
	gitIn(t, tree.Source, "add", "-A")
	gitIn(t, tree.Source, "commit", "-q", "-m", "base")
	s := openStore(t)
	const commit = "c0ffee"
	ok := compiledWith(co)
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: commit, Compiled: ok}); err != nil {
		t.Fatal(err)
	}
	// A refresh of the same commit whose exporter fails: the index keeps its rows and owes a compiled part.
	failing := func(context.Context) (*CompiledOptions, error) { return nil, errors.New("no toolchain now") }
	if _, err := s.Refresh(ctx, tree, RefreshOptions{Commit: commit, Compiled: failing}); err == nil {
		t.Fatal("a refresh whose compiled step failed reported no error")
	}
	if _, err := s.CompiledCommit(ctx, false); err == nil {
		t.Fatal("the index was accepted after its compiled step failed")
	}
	_, err := s.Refresh(ctx, tree, RefreshOptions{Commit: commit, Compiled: ok, Landed: func(context.Context) error { return nil }})
	if errors.Is(err, ErrAlreadyPast) {
		t.Fatal("a landing skipped an index that owes a compiled part")
	}
	if err != nil {
		t.Fatalf("the landing refresh: %v", err)
	}
	if _, err := s.CompiledCommit(ctx, false); err != nil {
		t.Errorf("the index after the landing refresh: %v", err)
	}
}
