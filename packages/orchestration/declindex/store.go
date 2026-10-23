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
	"os"
	"path/filepath"
	"sync"
	"syscall"

	_ "modernc.org/sqlite" // the database/sql driver "sqlite"
)

// SchemaVersion changes when the tables change; an index of another version is rebuilt from nothing.
const SchemaVersion = "3"

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
-- One row for each module whose reference file the index read. ilean_key and olean_key identify the build output
-- it was read from (Lake's content hash, or size and mtime without one).
CREATE TABLE IF NOT EXISTS modules (
    rel       TEXT PRIMARY KEY,
    module    TEXT NOT NULL,
    ilean_key TEXT NOT NULL,
    olean_key TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS modules_module ON modules (module);
CREATE TABLE IF NOT EXISTS imports (
    module   TEXT NOT NULL,
    imported TEXT NOT NULL,
    PRIMARY KEY (module, imported)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS imports_imported ON imports (imported);
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
`

var tables = []string{"meta", "modules", "imports", "decls", "uses", "edges", "consts", "atoms", "facts"}

// Store is an open index: the SQLite file at path, and the declaration graph beside it at path + ".graph".
type Store struct {
	db   *sql.DB
	path string

	graphOnce sync.Once
	g         *graph
	graphErr  error

	// beforeRefreshLock is a test hook. It runs immediately before RefreshAt attempts the writer lock.
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

func (s *Store) init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create the index schema: %w", err)
	}
	v, err := s.Meta(ctx, "schema")
	if err != nil {
		return err
	}
	if v == SchemaVersion {
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
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

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

// lock takes the index's exclusive writer lock, a file beside it, so that two refreshes never interleave.
func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
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
