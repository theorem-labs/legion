package declindex

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"regexp"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Target is where one index lives: a Postgres database, and a schema of it that holds this index alone. Many
// indexes of many repositories share one server, each keyed by its schema, so that what a landing writes touches
// nothing else.
type Target struct {
	// DSN is a pgx connection string. It comes from the environment or from a file the operator names, never from
	// a file in the indexed tree.
	DSN string
	// Index is the schema (ValidIndexName).
	Index string
	// Reader is the read-only role that Open grants USAGE on the schema and SELECT on its tables; empty grants
	// nothing.
	Reader string
}

// indexNamePattern is what a schema name of an index may be: lower-case, so that no quoted name differs from an
// unquoted one, and short enough for Postgres's 63-byte limit with any suffix a server adds.
var indexNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// ValidIndexName refuses a schema name that is not an index's: one that Postgres reserves (pg_*,
// information_schema), the database's shared schema (public), or one outside the pattern above.
func ValidIndexName(name string) error {
	if !indexNamePattern.MatchString(name) {
		return fmt.Errorf("the index name %q is not 1 to 48 characters of a lower-case letter followed by lower-case letters, digits and underscores", name)
	}
	if strings.HasPrefix(name, "pg_") {
		return fmt.Errorf("the index name %q is reserved: Postgres keeps the pg_ schemas for itself", name)
	}
	if name == "public" || name == "information_schema" {
		return fmt.Errorf("the index name %q is a schema of the database itself: give the index a schema of its own", name)
	}
	return nil
}

// lockClass is the first key of every advisory lock of an index: 0x44434958, "DCIX". The second key is the index
// name's FNV-1a hash, so that two indexes of one server lock apart. The ledger takes advisory locks of the
// one-key form, which Postgres never confuses with the two-key form.
const lockClass int32 = 0x44434958

// lockKeys are the two keys of the index's advisory lock, computed here rather than in SQL so that the server
// needs no function of ours.
func lockKeys(index string) (int32, int32) {
	h := fnv.New32a()
	_, _ = io.WriteString(h, index)
	return lockClass, int32(h.Sum32())
}

// errUnreachable marks the errors this package raises about a connection that is already gone, which the driver
// reports with an error of no kind of its own ("conn closed").
var errUnreachable = errors.New("the index's database is unreachable")

// Unreachable reports whether err is a failed connection to the index's database, rather than an answer from it:
// the database could not be reached, the connection dropped, or the server shut down or cancelled the session. A
// command exits 75 (temporary failure) for such an error, so that a post-landing hook is retried rather than
// counted as a defect of what it checked.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errUnreachable) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &opErr) || errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// A net.Error alone says little: syscall.Errno has Timeout and Temporary too, so a missing file would
		// count here as a database that cannot be reached, and a command would exit 75 instead of 3.
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			return true
		}
	}
	if pgconn.Timeout(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 08 is a connection exception. The rest are the server ending the session under it: 57P01 to
		// 57P03 shut down, cancel or refuse it; 25P03 and 25P04 end a transaction that idled or ran too long,
		// and 57P05 an idle session. Each ends the connection, so the question was never answered.
		switch {
		case strings.HasPrefix(pgErr.Code, "08"):
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03", pgErr.Code == "57P05":
			return true
		case pgErr.Code == "25P03", pgErr.Code == "25P04":
			return true
		}
		return false
	}
	return false
}

// unreachable wraps err as a failed connection when conn is closed. A statement that meets the end of its
// connection is told SQLSTATE 57P01 or a network error, which Unreachable knows; one that runs on a connection
// closed before it reaches the server is told only that it is closed.
func unreachable(conn *pgx.Conn, err error) error {
	if err == nil || Unreachable(err) || conn == nil || !conn.IsClosed() {
		return err
	}
	return fmt.Errorf("%w: %w", errUnreachable, err)
}

// openPool connects to the index's database. Every statement of the store is unqualified and resolved by the
// search_path, which names the index's schema alone, so that one connection string serves every index of a
// server. How the search_path is set depends on the route:
//
// A writer (session) holds one connection for its whole run, for its advisory lock and its writes, so it needs a
// route that keeps a session: the VM's Cloud SQL Auth Proxy, or the server itself. It names its schema once for
// each connection.
//
// A reader may go through a transaction pooler, which gives each transaction another server connection: the
// owners' pods read the shared server that way. Such a pooler refuses a connection that asks for a startup
// parameter it does not carry ("unsupported startup parameter: search_path", SQLSTATE 08P01), and a SET of a
// session would reach another client's transaction. So a reader sends no parameter of its own and names its
// schema inside its own transaction (OpenReadOnly), and it prepares no statement by name, because the named
// statement would live on a server connection that the next transaction does not get.
func openPool(ctx context.Context, t Target, session bool) (*pgxpool.Pool, error) {
	if err := ValidIndexName(t.Index); err != nil {
		return nil, err
	}
	if t.DSN == "" {
		return nil, errors.New("no database for the index: set DECLINDEX_DSN or pass --index-dsn-file")
	}
	cfg, err := pgxpool.ParseConfig(t.DSN)
	if err != nil {
		return nil, fmt.Errorf("read the index's connection string: %w", err)
	}
	if session {
		schema := pgx.Identifier{t.Index}.Sanitize()
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET search_path = "+schema)
			return err
		}
	} else if cfg.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement {
		// The driver's default caches prepared statements by name. A connection string that names another
		// mode keeps it, for a pooler that does carry prepared statements.
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	// A refresh holds one connection for its whole run, for its advisory lock and its writes, and reads the
	// index on another. A pool of one would deadlock: the refresh would wait for a connection it holds itself.
	if cfg.MaxConns < 2 {
		cfg.MaxConns = 2
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the index %q: %w", t.Index, err)
	}
	return pool, nil
}

// lock takes the index's advisory lock on conn, exclusive for a refresh and shared for a reader of the tree, and
// waits for it until ctx ends (pgx cancels the waiting statement). The lock lives with the connection: a
// connection that drops gives it up, with any transaction it carried.
//
// It gives the server backend that holds the lock. A route that pools transactions hands the next transaction
// another backend, which holds neither the lock nor what the session set, so every user of the lock reads the
// backend again before it acts (sameBackend).
func lock(ctx context.Context, conn *pgxpool.Conn, index string, exclusive bool) (uint32, error) {
	k1, k2 := lockKeys(index)
	q := `SELECT pg_advisory_lock_shared($1, $2)`
	if exclusive {
		q = `SELECT pg_advisory_lock($1, $2)`
	}
	if _, err := conn.Exec(ctx, q, k1, k2); err != nil {
		// A wait that ctx ended is that, whatever the driver made of the cancelled statement.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, fmt.Errorf("wait for the lock of index %q: %w", index, ctxErr)
		}
		return 0, fmt.Errorf("wait for the lock of index %q: %w", index, err)
	}
	pid, err := backendPID(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("the lock of index %q: %w", index, err)
	}
	return pid, nil
}

// backendPID is the server backend that answers on q now.
func backendPID(ctx context.Context, q querier) (uint32, error) {
	var pid uint32
	err := q.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	return pid, err
}

// sameBackend refuses to go on when the backend that answers is not the one that took the index's lock: the
// connection went through something that pools transactions, so the lock is held by a backend this command no
// longer has, and what the session set (its schema) is not there either. A refresh, a gate and a ledger check
// must reach the server over a route that keeps a session, such as the build machine's Cloud SQL Auth Proxy.
func sameBackend(ctx context.Context, q querier, index string, locked uint32) error {
	pid, err := backendPID(ctx, q)
	if err != nil {
		return err
	}
	if pid != locked {
		return fmt.Errorf("the connection to index %q is not one session: the server backend changed from %d to %d, "+
			"so the index's lock is no longer held; a refresh, a gate and a ledger check need a route that keeps a "+
			"session, not one that pools transactions", index, locked, pid)
	}
	return nil
}

// unlock gives back a lock that lock took. Postgres returns false when this session did not hold it. A failed
// unlock leaves the session uncertain, so its caller must close that physical connection instead of returning it
// to the pool.
func unlock(ctx context.Context, conn *pgxpool.Conn, index string, exclusive bool) error {
	k1, k2 := lockKeys(index)
	q := `SELECT pg_advisory_unlock_shared($1, $2)`
	if exclusive {
		q = `SELECT pg_advisory_unlock($1, $2)`
	}
	var released bool
	if err := conn.QueryRow(context.WithoutCancel(ctx), q, k1, k2).Scan(&released); err != nil {
		return fmt.Errorf("release the lock of index %q: %w", index, err)
	}
	if !released {
		return fmt.Errorf("release the lock of index %q: this connection did not hold it", index)
	}
	return nil
}

// releaseLock gives back the lock and returns a clean connection to the pool, or closes a connection whose lock
// state could not be proved clean. Closing ends the Postgres session, which releases every session advisory lock.
func releaseLock(ctx context.Context, conn *pgxpool.Conn, index string, exclusive bool) {
	if err := unlock(ctx, conn, index, exclusive); err != nil {
		_ = conn.Conn().Close(context.WithoutCancel(ctx))
	}
	conn.Release()
}
