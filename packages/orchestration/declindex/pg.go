package declindex

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"
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
	// TreeLock is the file whose flock guards the indexed tree on its machine (lockTree): a refresh holds it
	// exclusive for its whole run, and a reader of the tree (IndexAndTree) shared. It is in the index's state
	// directory, outside the tree. Empty guards nothing, for a caller that runs alone.
	TreeLock string
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
	// A connection the driver has already closed, a half-read answer, a closed socket: the question never
	// reached the server, or its answer never came back.
	if errors.Is(err, errUnreachable) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, pgconn.ErrConnClosed) {
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
// server.
//
// No connection keeps state of its own between transactions. The shared server pools transactions on every
// route it offers (Cloud SQL's managed connection pooling, behind the Auth Proxy too): each transaction may get
// another server backend, a startup parameter it does not carry is refused ("unsupported startup parameter:
// search_path", SQLSTATE 08P01), and a session's SET would reach another client. So the store names its schema
// inside each transaction (SET LOCAL search_path), takes its lock inside the transaction that writes
// (pg_advisory_xact_lock), and prepares no statement by name, because a named statement would live on a backend
// that the next transaction does not get.
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
	// A connection string may carry its own search_path; a pooler refuses it, and the store names its own.
	delete(cfg.ConnConfig.RuntimeParams, "search_path")
	if cfg.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement {
		// The driver's default caches prepared statements by name. A connection string that names another
		// mode keeps it, for a pooler that does carry prepared statements.
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	// A refresh holds one connection for its writes and reads the index on another. A pool of one would
	// deadlock: the refresh would wait for a connection it holds itself.
	if cfg.MaxConns < 2 {
		cfg.MaxConns = 2
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the index %q: %w", t.Index, err)
	}
	return pool, nil
}

// namedSchema is the statement that begins every transaction of the store: it names the index's schema for that
// transaction alone.
func namedSchema(index string) string {
	return "SET LOCAL search_path = " + pgx.Identifier{index}.Sanitize()
}

// xactLock takes the index's advisory lock for the rest of tx, exclusive, and waits for it until ctx ends (pgx
// cancels the waiting statement). The lock lives with the transaction, as everything on a pooled route must:
// two transactions that write one index run one after the other, and the commit or the rollback gives it back.
func xactLock(ctx context.Context, tx pgx.Tx, index string) error {
	k1, k2 := lockKeys(index)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, k1, k2); err != nil {
		// A wait that ctx ended is that, whatever the driver made of the cancelled statement.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("wait for the lock of index %q: %w", index, ctxErr)
		}
		return fmt.Errorf("wait for the lock of index %q: %w", index, err)
	}
	return nil
}

// lockTree takes the flock on path, exclusive (creating the file) or shared, and waits for it until ctx ends: a
// blocking flock would not notice a signal. It guards the indexed tree on the machine that holds it, which no
// database lock can: a refresh fetches, checks out and builds the tree and then reads it, and a gate and a ledger
// check read the tree too, each for a whole run that is many transactions long. A shared lock opens the file
// read-only when it is there, so that it works in a read-only state directory, and creates it otherwise.
func lockTree(ctx context.Context, path string, exclusive bool) (func(), error) {
	flag, how := os.O_RDONLY, unix.LOCK_SH
	if exclusive {
		flag, how = os.O_CREATE|os.O_RDWR, unix.LOCK_EX
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("the lock of the indexed tree: %w", err)
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if !exclusive && errors.Is(err, os.ErrNotExist) {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err != nil {
		return nil, fmt.Errorf("the lock of the indexed tree: %w", err)
	}
	for wait := time.Millisecond; ; wait = min(2*wait, 250*time.Millisecond) {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("the lock of the indexed tree %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("wait for the lock of the indexed tree %s: %w", path, ctx.Err())
		case <-time.After(wait):
		}
	}
}
