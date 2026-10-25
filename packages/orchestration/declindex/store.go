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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // the database/sql driver "sqlite"
)

// SchemaVersion changes whenever what an index holds changes: a table, a column, a meta key that a reader needs, or
// how a stored value is computed (a statement hash). An index of another version is rebuilt from nothing (Open), and
// OpenReadOnly refuses it.
const SchemaVersion = "13"

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
-- The modules that each module the index read imports directly, from its reference file.
CREATE TABLE IF NOT EXISTS imports (module TEXT NOT NULL, imported TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS imports_module ON imports (module);
-- A declaration is its module and its name: two modules that are never imported together can declare one name.
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
-- One use of a constant: target and tmodule are its name and the module that Lean recorded for it; module, line,
-- col and parent are where the use is.
CREATE TABLE IF NOT EXISTS uses (
    target  TEXT NOT NULL,
    tmodule TEXT NOT NULL,
    module  TEXT NOT NULL,
    line    INTEGER NOT NULL,
    col     INTEGER NOT NULL,
    parent  TEXT NOT NULL,
    cls     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS uses_target ON uses (target);
CREATE INDEX IF NOT EXISTS uses_module ON uses (module);
-- Declaration-level edges, used -> user: target and tmodule are the constant the use records (a declaration, or a
-- part Lean generated from one, such as a field), dst and module the user. The graph counts a generated part as its
-- declaration. stmt is 1 when some use is in the user's statement; kind is the user's kind.
CREATE TABLE IF NOT EXISTS edges (
    target  TEXT NOT NULL,
    tmodule TEXT NOT NULL,
    dst     TEXT NOT NULL,
    module  TEXT NOT NULL,
    stmt    INTEGER NOT NULL,
    kind    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS edges_module ON edges (module);
-- What the Lean exporter read from the compiled module: every constant (generated ones included), its elaborated
-- type as printed, and its compiled statement hash.
CREATE TABLE IF NOT EXISTS consts (
    name   TEXT NOT NULL,
    module TEXT NOT NULL,
    kind   TEXT NOT NULL,
    type   TEXT NOT NULL,
    cstmt  TEXT NOT NULL,
    PRIMARY KEY (name, module)
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
-- The modules that the compiled step read without the registry's forms (FormsClash), each with a name that kept it
-- from them: one row for each module (declarer) that declares the name in the module's import closure (forms 0) or in
-- the forms' imports' closure (forms 1). A declarer can be on both sides, when the two sides hold two axioms.
CREATE TABLE IF NOT EXISTS without_forms (
    module   TEXT NOT NULL,
    name     TEXT NOT NULL,
    declarer TEXT NOT NULL,
    forms    INTEGER NOT NULL,
    PRIMARY KEY (module, name, forms, declarer)
) WITHOUT ROWID;
-- Each refresh that moved the index to another commit: landed, the commit it read, and previous, the commit it
-- replaced ('' for the first). The post-landing registry check of a landing compares those two commits.
CREATE TABLE IF NOT EXISTS landings (landed TEXT PRIMARY KEY, previous TEXT NOT NULL) WITHOUT ROWID;
`

var tables = []string{"meta", "sources", "modules", "imports", "decls", "uses", "edges", "consts", "atoms", "facts", "without_forms", "landings"}

// The index's two lock files, beside it. A refresh holds the refresh lock exclusive across its sync (fetch,
// checkout and build) and its reads of the tree, so that two refreshes never interleave. Every write of the index
// holds the write lock exclusive, only while it writes (Store.write). A reader in a read-only directory holds,
// shared, the locks of what it reads (OpenReadOnly). A process that takes both takes the refresh lock first.
const (
	refreshLockSuffix = ".lock"
	writeLockSuffix   = ".write.lock"
)

// busyTimeoutMS is how long, in milliseconds, a statement waits for a lock that another connection holds.
const busyTimeoutMS = "120000"

// Store is an open index: the SQLite file at path, and the declaration graph beside it at path + ".graph".
type Store struct {
	db *sql.DB
	// q is what every read of the store goes through: the database, or for a store opened read-only its snapshot.
	q    dbReader
	path string
	// release ends what a store opened read-only holds besides its files: its snapshot, and its shared holds on the
	// locks.
	release func()

	// mu guards graphFile and derived.
	mu sync.Mutex
	// graphFile is the graph file that the store's answers read: for a store opened read-only the one in place when
	// its snapshot began, and for a writable store the one in place when it opened or its last refresh wrote one.
	graphFile *os.File
	derived   derived

	// afterRefreshLock is a test hook. Refresh runs it once it holds the refresh lock, before the landed sync.
	afterRefreshLock func(context.Context) error
}

// derived is what a store reads from its index once and keeps for its answers: the declaration graph (or why it
// could not), and each module's source path. reload drops it.
type derived struct {
	graph    *graph
	graphErr error
	paths    map[string]string
}

// reload holds the graph file now in place and drops what the store derived from the index before. Open and
// OpenReadOnly call it once the index is open, and a refresh once it has rewritten the modules and the graph.
func (s *Store) reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graphFile != nil {
		_ = s.graphFile.Close()
	}
	f, err := os.Open(s.path + ".graph")
	s.graphFile, s.derived = f, derived{}
	if err != nil {
		s.graphFile, s.derived.graphErr = nil, graphUnreadable(err)
	}
}

// Open opens (creating it if needed) the index at path. An index of another schema version is emptied, so the
// next refresh rebuilds it.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// The file is created here, through a symlink when path is one, so that resolveIndex names the file itself.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if path, err = resolveIndex(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(" + busyTimeoutMS + ")" +
		"&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-262144)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, q: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.reload()
	return s, nil
}

// resolveIndex resolves the symlinks of the existing index file at path. An index's lock files and graph are named
// from the path of the file itself, as SQLite names its -wal and -shm files, so that every path that names one index
// (a symlink to it, or the resolved path that the gate opens, Config.GateIndexPath) takes the same locks and reads
// the same graph.
func resolveIndex(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve the index %s: %w", path, err)
	}
	return resolved, nil
}

// ReadScope is what a store opened read-only reads besides the index, and so which part of a running refresh a
// reader in a read-only directory waits for.
type ReadScope int

const (
	// IndexOnly reads nothing that a refresh's sync changes. It waits only while a refresh writes the index, and
	// reads the index of the refresh before while the refresh fetches, checks out and builds. land-hash reads so.
	IndexOnly ReadScope = iota
	// IndexAndTree also reads the configured tree (its HEAD, its commits or its build), which a refresh's sync
	// checks out and builds, so in every directory it holds the refresh lock shared: it waits for the whole
	// refresh, and a refresh waits for it. The ledger and the gate read so.
	IndexAndTree
)

// OpenReadOnly opens an existing, initialized index without creating files, initializing the schema, or enabling
// WAL: the gate opens it so that its reads cannot modify a project's tree, the post-landing commands because the
// landing program mounts their index directory read-only, and the queries because they only read.
//
// The store reads one state of the index for its whole life: one read transaction, begun here and ended by Close,
// carries every read, and the graph file it reads is the one in place when that transaction began (graph refuses a
// later one). So a command never sees a refresh's write half done, nor rows of two refreshes. A refresh writes the
// reference part and the compiled part in two transactions; a reader between them reads the compiled part of the
// commit before, which its readers refuse (CompiledCommit).
//
// SQLite reads a WAL database only through its shared-memory file, which it cannot create in a read-only
// directory. There the index is opened immutable, reading the database file alone, while the store holds, shared,
// the locks of what scope reads until it is closed: the write lock always, so that no write of the index runs
// meanwhile and each write before it has checkpointed its write-ahead log into the file; and for IndexAndTree the
// refresh lock before it, so that no refresh changes the tree meanwhile. A non-empty log left beside the file (a
// write that did not finish) is refused, since the file alone would then be out of date. In a writable directory
// the snapshot alone keeps the index consistent: an IndexOnly store holds no lock, and a refresh can commit while it
// reads, its checkpoint waiting for the store to close; an IndexAndTree store holds the refresh lock shared, because
// the tree and its build are not in the snapshot.
func OpenReadOnly(ctx context.Context, path string, scope ReadScope) (*Store, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if info.IsDir() {
		return nil, fmt.Errorf("index %s is a directory", path)
	}
	path, err := resolveIndex(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(" + busyTimeoutMS + ")"
	var release func()
	if !directoryWritable(filepath.Dir(path)) {
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
	if release == nil && scope == IndexAndTree {
		// The directory is writable, so a missing lock file (an index that no refresh of this declindex made) is
		// created rather than refused.
		f, err := os.OpenFile(path+refreshLockSuffix, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		if release, err = lockShared(ctx, []string{path + refreshLockSuffix}); err != nil {
			return nil, fmt.Errorf("the index %s: %w", path, err)
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	s := &Store{db: db, q: db, path: path, release: release}
	// One read transaction for the store's life, so that every read of it sees one committed state of the index. It
	// lives until Close, not until ctx ends; its first read below begins the snapshot.
	snapshot, err := db.BeginTx(context.WithoutCancel(ctx), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open the index read-only: %w", err)
	}
	s.q = snapshot
	s.release = func() {
		_ = snapshot.Rollback()
		if release != nil {
			release()
		}
	}
	version, err := s.Meta(ctx, "schema")
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open the index read-only: %w", err)
	}
	if version != SchemaVersion {
		_ = s.Close()
		return nil, fmt.Errorf("index %s has schema %q, and this declindex reads schema %q: run declindex refresh, which rebuilds it", path, version, SchemaVersion)
	}
	// The graph file of this state: a refresh replaces it just before its commit, so this opens a later one only when
	// a refresh replaced it after the snapshot's first read above, and graph() refuses that one. A reader keeps the
	// file it opened when a later refresh replaces it.
	s.reload()
	return s, nil
}

// directoryWritable reports whether this process can create files in dir, as SQLite must to read a WAL database. A
// test replaces it to stand for a read-only directory whatever its user: a directory's mode does not stop root.
var directoryWritable = func(dir string) bool { return unix.Access(dir, unix.W_OK) == nil }

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
	return s.write(ctx, nil, func() error {
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
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&meta)
	if err != nil || meta == 0 {
		return "", err
	}
	return s.Meta(ctx, "schema")
}

// Close closes the index: it ends a read-only store's snapshot and gives back its holds on the locks, and closes the
// graph file and the database.
func (s *Store) Close() error {
	if s.release != nil {
		s.release()
		s.release = nil
	}
	if s.graphFile != nil {
		_ = s.graphFile.Close()
		s.graphFile = nil
	}
	return s.db.Close()
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
	err := s.q.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
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

// writeTx runs f in one transaction of the index under the write lock (write), and commits it when f succeeds.
func (s *Store) writeTx(ctx context.Context, log io.Writer, f func(*sql.Tx) error) error {
	return s.write(ctx, log, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := f(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// write runs f, a write of the index, while it holds the index's write lock exclusive, and then moves the
// write-ahead log into the database file (checkpoint). A reader in a read-only directory holds that lock shared and
// reads the database file alone, so it reads the index as it was before f or as f left it, never during f. When f
// fails, write leaves the log to the store's close, which checkpoints it; such a reader refuses a non-empty log
// meanwhile.
func (s *Store) write(ctx context.Context, log io.Writer, f func() error) error {
	unlock, err := lockFile(ctx, s.path+writeLockSuffix, true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := f(); err != nil {
		return err
	}
	return s.checkpoint(ctx, log)
}

// checkpoint moves the whole write-ahead log into the database file and empties it. A reader in a writable directory
// reads one state of the index for its whole life (OpenReadOnly), and while it reads an earlier state from the log,
// the log cannot be emptied: checkpoint waits for such readers, until ctx ends, and notes the wait on log once.
// Readers that begin meanwhile read the database file and do not hold it up.
func (s *Store) checkpoint(ctx context.Context, log io.Writer) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Without a busy timeout, a checkpoint that a reader holds up says so at once, and the wait below sees ctx end.
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA busy_timeout = `+busyTimeoutMS) }()
	noted := false
	for wait := time.Millisecond; ; wait = min(2*wait, 250*time.Millisecond) {
		var busy, frames, moved int
		if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &moved); err != nil {
			return fmt.Errorf("checkpoint the index: %w", err)
		}
		if busy == 0 {
			return nil
		}
		if !noted {
			noted = true
			logf(log, "index: waiting for readers of an earlier state of the index to finish, to move the write-ahead log into the database file")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("checkpoint the index while readers read an earlier state of it: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
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

// deleteCompiled drops what the exporter read from one module, and its clashes with the forms.
func deleteCompiled(ctx context.Context, x execer, module string) error {
	for _, t := range []string{"consts", "atoms", "facts", "without_forms"} {
		if _, err := x.ExecContext(ctx, "DELETE FROM "+t+" WHERE module = ?", module); err != nil {
			return err
		}
	}
	return nil
}

// ModuleCount is the number of modules the index read.
func (s *Store) ModuleCount(ctx context.Context) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM modules`).Scan(&n)
	return n, err
}

// Landing gives the commit that the refresh to landed replaced (empty for the index's first commit), and whether a
// refresh recorded landed at all.
func (s *Store) Landing(ctx context.Context, landed string) (previous string, recorded bool, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT previous FROM landings WHERE landed = ?`, landed).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the landing of %s: %w", landed, err)
	}
	return previous, true, nil
}

// CompiledCommit is the index's commit, when the index's compiled part (types, compiled hashes, atoms and facts) is
// of that commit. A refresh writes the compiled part after the reference part, in a second transaction, and a
// compiled step that fails leaves the compiled part of an earlier commit; a reader that joined the two parts would
// join two commits' rows, so it is refused. An index read without its compiled part (lean.export off) has none:
// required refuses it; else it is accepted, and its compiled answers are empty.
func (s *Store) CompiledCommit(ctx context.Context, required bool) (string, error) {
	commit, err := s.Meta(ctx, "commit")
	if err != nil {
		return "", err
	}
	compiled, err := s.Meta(ctx, "compiled_commit")
	if err != nil {
		return "", err
	}
	switch {
	case compiled == commit, compiled == "" && !required:
		return commit, nil
	case compiled == "":
		return "", errors.New("the index was read without its compiled part: refresh it with lean.export on")
	default:
		return "", fmt.Errorf("the index's compiled part is of %s, not of its commit %s: refresh the index", compiled, commit)
	}
}
