// Package declindex is the index of declarations of a Lean project, read from its compiled proofs: where each
// declaration is, its kind, type and statement hashes, which declarations use which (from Lean's reference files,
// `.ilean`), and which facts each statement states (from the compiled `.olean` files, through the Lean exporter
// in lean/). It lives in one SQLite file next to the build, so that a query needs no network and takes
// milliseconds, and it is brought up to date after each landing by re-reading only the modules whose build
// output changed.
package declindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // the database/sql driver "sqlite"
)

// SchemaVersion changes whenever what an index holds changes: a table, a column, or a meta key that a reader needs.
// An index of another version is rebuilt from nothing (Open), and OpenReadOnly refuses it. Version 4 added the
// landings table and the compiled_commit and source_prefix keys.
const SchemaVersion = "4"

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS sources (
    path TEXT PRIMARY KEY,
    blob TEXT NOT NULL
) WITHOUT ROWID;
-- One row for each module whose reference file the index read. ilean_key and olean_key identify the build output
-- it was read from (Lake's content hash, or size and mtime without one).
CREATE TABLE IF NOT EXISTS modules (
    rel       TEXT PRIMARY KEY,
    module    TEXT NOT NULL,
    ilean_key TEXT NOT NULL,
    olean_key TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS modules_module ON modules (module);
CREATE TABLE IF NOT EXISTS decls (
    name      TEXT NOT NULL,
    short     TEXT NOT NULL,
    module    TEXT NOT NULL,
    kind      TEXT NOT NULL,
    line      INTEGER NOT NULL,
    stmt_hash TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS decls_name ON decls (name);
CREATE INDEX IF NOT EXISTS decls_short ON decls (short);
CREATE INDEX IF NOT EXISTS decls_module ON decls (module);
CREATE TABLE IF NOT EXISTS uses (
    target TEXT NOT NULL,
    module TEXT NOT NULL,
    line   INTEGER NOT NULL,
    col    INTEGER NOT NULL,
    parent TEXT NOT NULL,
    cls    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS uses_target ON uses (target);
CREATE INDEX IF NOT EXISTS uses_module ON uses (module);
-- Declaration-level edges, used -> user: target is the name the use records (a declaration, or a part Lean
-- generated from one, such as a field). The graph counts a generated part as its declaration. stmt is 1 when some
-- use is in the user's statement; kind is the user's kind.
CREATE TABLE IF NOT EXISTS edges (
    target TEXT NOT NULL,
    dst    TEXT NOT NULL,
    module TEXT NOT NULL,
    stmt   INTEGER NOT NULL,
    kind   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS edges_module ON edges (module);
-- What the Lean exporter read from the compiled module: every constant (generated ones included), its elaborated
-- type as printed, and its compiled statement hash.
CREATE TABLE IF NOT EXISTS consts (
    name   TEXT PRIMARY KEY,
    module TEXT NOT NULL,
    kind   TEXT NOT NULL,
    type   TEXT NOT NULL,
    cstmt  TEXT NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS consts_module ON consts (module);
CREATE INDEX IF NOT EXISTS consts_cstmt ON consts (cstmt);
-- The relation atoms of each constant's statement, by canonical hash: who else states the same fact.
CREATE TABLE IF NOT EXISTS atoms (name TEXT NOT NULL, module TEXT NOT NULL, atom TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS atoms_atom ON atoms (atom);
CREATE INDEX IF NOT EXISTS atoms_name ON atoms (name);
CREATE INDEX IF NOT EXISTS atoms_module ON atoms (module);
-- The registry concepts each constant's statement states: how is direct, via:<definition> or uses.
CREATE TABLE IF NOT EXISTS facts (name TEXT NOT NULL, module TEXT NOT NULL, concept TEXT NOT NULL, how TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS facts_concept ON facts (concept);
CREATE INDEX IF NOT EXISTS facts_name ON facts (name);
CREATE INDEX IF NOT EXISTS facts_module ON facts (module);
-- Each refresh that moved the index to another commit: landed, the commit it read, and previous, the commit it
-- replaced ('' for the first). The post-landing registry check of a landing compares those two commits.
CREATE TABLE IF NOT EXISTS landings (landed TEXT PRIMARY KEY, previous TEXT NOT NULL) WITHOUT ROWID;
`

var tables = []string{"meta", "sources", "modules", "decls", "uses", "edges", "consts", "atoms", "facts", "landings"}

// The index's two lock files, beside it. A refresh holds the refresh lock exclusive across its sync (fetch,
// checkout and build) and its reads of the tree, so that two refreshes never interleave. Every write of the index
// holds the write lock exclusive, only while it writes (Store.write). A reader in a read-only directory holds,
// shared, the locks of what it reads (OpenReadOnly). A process that takes both takes the refresh lock first.
const (
	refreshLockSuffix = ".lock"
	writeLockSuffix   = ".write.lock"
)

// Store is an open index: the SQLite file at path, and the declaration graph beside it at path + ".graph".
type Store struct {
	db   *sql.DB
	path string
	// release gives back the shared holds on the locks of a store opened in a read-only directory.
	release func()

	graphOnce sync.Once
	g         *graph
	graphErr  error

	// beforeRefreshLock is a test hook. It runs immediately before RefreshAt attempts the refresh lock.
	beforeRefreshLock func()
}

// Open opens (creating it if needed) the index at path. An index of another schema version is emptied, so the
// next refresh rebuilds it.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(120000)&_pragma=synchronous(NORMAL)" +
		"&_pragma=temp_store(MEMORY)&_pragma=cache_size(-262144)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// ReadScope is what a store opened read-only reads besides the index, and so which part of a running refresh a
// reader in a read-only directory waits for.
type ReadScope int

const (
	// IndexOnly reads nothing that a refresh's sync changes. It waits only while a refresh writes the index, and
	// reads the index of the refresh before while the refresh fetches, checks out and builds. land-hash reads so.
	IndexOnly ReadScope = iota
	// IndexAndTree also reads the configured tree (its HEAD, its commits or its build), which a refresh's sync
	// checks out and builds, so it waits for the whole refresh, and a refresh waits for it. The ledger and the gate
	// read so.
	IndexAndTree
)

// OpenReadOnly opens an existing, initialized index without creating files, initializing the schema, or enabling
// WAL: the gate opens it so that its reads cannot modify a project's tree, and the post-landing commands because
// the landing program mounts their index directory read-only.
//
// SQLite reads a WAL database only through its shared-memory file, which it cannot create in a read-only
// directory. There the index is opened immutable, reading the database file alone, while the store holds, shared,
// the locks of what scope reads until it is closed: the write lock always, so that no write of the index runs
// meanwhile and each write before it has checkpointed its write-ahead log into the file; and for IndexAndTree the
// refresh lock before it, so that no refresh changes the tree meanwhile. A non-empty log left beside the file (a
// write that did not finish) is refused, since the file alone would then be out of date. In a writable directory
// the store holds no lock, and SQLite reads each query from one committed state of the index.
func OpenReadOnly(ctx context.Context, path string, scope ReadScope) (*Store, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if info.IsDir() {
		return nil, fmt.Errorf("index %s is a directory", path)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(120000)"
	var release func()
	if unix.Access(filepath.Dir(path), unix.W_OK) != nil {
		locks := []string{path + writeLockSuffix}
		if scope == IndexAndTree {
			locks = []string{path + refreshLockSuffix, path + writeLockSuffix}
		}
		var err error
		if release, err = lockShared(ctx, locks); err != nil {
			return nil, fmt.Errorf("the read-only index %s: %w", path, err)
		}
		if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
			release()
			return nil, fmt.Errorf("the read-only index %s has a write-ahead log that only a writable directory can read: refresh it once", path)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			release()
			return nil, err
		}
		dsn = "file:" + path + "?mode=ro&immutable=1&_pragma=query_only(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	s := &Store{db: db, path: path, release: release}
	version, err := s.Meta(ctx, "schema")
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open the index read-only: %w", err)
	}
	if version != SchemaVersion {
		_ = s.Close()
		return nil, fmt.Errorf("index %s has schema %q, and this declindex reads schema %q: run declindex refresh, which rebuilds it", path, version, SchemaVersion)
	}
	return s, nil
}

// lockShared takes the flock of each of paths shared, in order, and gives the release of them all. A lock file that
// is missing is one that no refresh of this declindex made.
func lockShared(ctx context.Context, paths []string) (func(), error) {
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, path := range paths {
		r, err := lockFile(ctx, path, false)
		if err != nil {
			release()
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("it has no lock file %s, which a refresh of this declindex makes: refresh it", path)
			}
			return nil, fmt.Errorf("hold its lock %s: %w", path, err)
		}
		releases = append(releases, r)
	}
	return release, nil
}

// init creates a new index's schema, or empties an index of another schema version, under the write lock. An index
// of this version needs no write, so Open of it takes no lock.
func (s *Store) init(ctx context.Context) error {
	version, err := s.schemaVersion(ctx)
	if err != nil || version == SchemaVersion {
		return err
	}
	return s.write(ctx, func() error {
		if _, err := s.db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("create the index schema: %w", err)
		}
		v, err := s.Meta(ctx, "schema")
		if err != nil {
			return err
		}
		if v == SchemaVersion {
			// Another process's Open made it while this one waited for the lock.
			return nil
		}
		if v != "" {
			for _, t := range tables {
				if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+t); err != nil {
					return err
				}
			}
			if _, err := s.db.ExecContext(ctx, schema); err != nil {
				return err
			}
		}
		return s.setMeta(ctx, s.db, "schema", SchemaVersion)
	})
}

// schemaVersion is the index's schema version, empty for a file without a meta table: a new one.
func (s *Store) schemaVersion(ctx context.Context) (string, error) {
	var meta int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&meta)
	if err != nil || meta == 0 {
		return "", err
	}
	return s.Meta(ctx, "schema")
}

// Close closes the index, and gives back the read-only store's holds on its locks.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.release != nil {
		s.release()
		s.release = nil
	}
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) setMeta(ctx context.Context, x execer, k, v string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// Meta reads one value the index keeps about itself (commit, built, schema, errors); empty when unset.
func (s *Store) Meta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// CheckComplete refuses an index that no refresh has read, or whose last refresh could not read every module's
// reference file: its answers would be incomplete.
func (s *Store) CheckComplete(ctx context.Context) error {
	built, err := s.Meta(ctx, "built")
	if err != nil {
		return err
	}
	if built == "" {
		return fmt.Errorf("the index at %s was never refreshed: run declindex refresh", s.path)
	}
	n, err := s.Meta(ctx, "errors")
	if err != nil {
		return err
	}
	if n != "0" {
		return fmt.Errorf("the index at %s is incomplete: %s modules' reference files could not be read", s.path, n)
	}
	return nil
}

// lockRefresh takes the index's refresh lock exclusive, so that two refreshes never interleave. It waits for the
// lock until ctx ends.
func (s *Store) lockRefresh(ctx context.Context) (func(), error) {
	return lockFile(ctx, s.path+refreshLockSuffix, true)
}

// write runs f, a write of the index, while it holds the index's write lock exclusive, and then checkpoints the
// write-ahead log into the database file. A reader in a read-only directory holds that lock shared and reads the
// database file alone, so it reads the index as it was before f or as f left it, never during f. When f fails,
// write leaves the log to the store's close, which checkpoints it; such a reader refuses a non-empty log meanwhile.
func (s *Store) write(ctx context.Context, f func() error) error {
	unlock, err := lockFile(ctx, s.path+writeLockSuffix, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := f(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint the index: %w", err)
	}
	return nil
}

// lockFile takes the flock on path, exclusive (creating the file) or shared (opening it read-only, so that it
// works in a read-only directory), and waits for it until ctx ends: a blocking flock would not notice a signal.
func lockFile(ctx context.Context, path string, exclusive bool) (func(), error) {
	flag, how := os.O_RDONLY, unix.LOCK_SH
	if exclusive {
		flag, how = os.O_CREATE|os.O_RDWR, unix.LOCK_EX
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// deleteCompiled drops what the exporter read from one module.
func deleteCompiled(ctx context.Context, x execer, module string) error {
	for _, t := range []string{"consts", "atoms", "facts"} {
		if _, err := x.ExecContext(ctx, "DELETE FROM "+t+" WHERE module = ?", module); err != nil {
			return err
		}
	}
	return nil
}

// ModuleCount is the number of modules the index read.
func (s *Store) ModuleCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM modules`).Scan(&n)
	return n, err
}

// Landing gives the commit that the refresh to landed replaced (empty for the index's first commit), and whether a
// refresh recorded landed at all.
func (s *Store) Landing(ctx context.Context, landed string) (previous string, recorded bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT previous FROM landings WHERE landed = ?`, landed).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the landing of %s: %w", landed, err)
	}
	return previous, true, nil
}
