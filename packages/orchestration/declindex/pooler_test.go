package declindex

import (
	"context"
	"encoding/binary"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"

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

// The refresh lock is a session's: a route that pools transactions holds it on a backend that the next
// transaction does not get, so the refresh would write with no lock, and nothing would stop a second refresh.
// Every write reads the backend again and refuses when it changed.
func TestAWriteRefusesABackendThatDoesNotHoldTheLock(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	s.refreshBackend++ // the backend that answers is not the one that took the lock
	err = s.writeTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("a write on a backend that holds no lock was allowed")
	}
	if !strings.Contains(err.Error(), "not one session") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}
