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
		// Class 08 is a connection exception; 57P01 to 57P03 are the server ending or refusing the session.
		return strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03"
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
// connection's search_path, which names the index's schema alone, so that one connection string serves every
// index of a server.
func openPool(ctx context.Context, t Target) (*pgxpool.Pool, error) {
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
	cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{t.Index}.Sanitize()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the index %q: %w", t.Index, err)
	}
	return pool, nil
}

// lock takes the index's advisory lock on conn, exclusive for a refresh and shared for a reader of the tree, and
// waits for it until ctx ends (pgx cancels the waiting statement). The lock lives with the connection: a
// connection that drops gives it up, with any transaction it carried.
func lock(ctx context.Context, conn *pgxpool.Conn, index string, exclusive bool) error {
	k1, k2 := lockKeys(index)
	q := `SELECT pg_advisory_lock_shared($1, $2)`
	if exclusive {
		q = `SELECT pg_advisory_lock($1, $2)`
	}
	if _, err := conn.Exec(ctx, q, k1, k2); err != nil {
		// A wait that ctx ended is that, whatever the driver made of the cancelled statement.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("wait for the lock of index %q: %w", index, ctxErr)
		}
		return fmt.Errorf("wait for the lock of index %q: %w", index, err)
	}
	return nil
}

// unlock gives back a lock that lock took. A connection that dropped meanwhile has given it up already, so the
// error is not worth reporting.
func unlock(ctx context.Context, conn *pgxpool.Conn, index string, exclusive bool) {
	k1, k2 := lockKeys(index)
	q := `SELECT pg_advisory_unlock_shared($1, $2)`
	if exclusive {
		q = `SELECT pg_advisory_unlock($1, $2)`
	}
	_, _ = conn.Exec(context.WithoutCancel(ctx), q, k1, k2)
}
