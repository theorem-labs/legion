// Package declindex is the index of declarations of a Lean project, read from its compiled proofs: where each
// declaration is, its kind, type and statement hashes, which declarations use which (from Lean's reference files,
// `.ilean`), and which facts each statement states (from the compiled `.olean` files, through the Lean exporter
// in lean/). It lives in a schema of a shared Postgres server, one schema for each indexed module, so that a
// landing rewrites only what its build changed and every machine reads one index over the network.
package declindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion changes whenever what an index holds changes: a table, a column, a meta key that a reader needs, or
// how a stored value is computed (a statement hash). An index of another version is rebuilt from nothing (Open), and
// OpenReadOnly refuses it.
const SchemaVersion = "15"

// schema is the index's tables. Every text column is COLLATE "C": the index's answers order and group names by
// bytes, as Lean's own name order does, whatever collation the server's database was created with. The hashes,
// kinds and blobs do not use a range scan now, but have the same collation so any future comparison is byte order.
const schema = `
CREATE TABLE meta (k text COLLATE "C" PRIMARY KEY, v text COLLATE "C" NOT NULL);
CREATE TABLE sources (path text COLLATE "C" PRIMARY KEY, blob text COLLATE "C" NOT NULL);
-- One row for each module whose reference file the index read. ilean_key and olean_key identify the build output
-- it was read from (Lake's content hash, or size and mtime without one).
CREATE TABLE modules (rel text COLLATE "C" PRIMARY KEY, module text COLLATE "C" NOT NULL, ilean_key text COLLATE "C" NOT NULL, olean_key text COLLATE "C" NOT NULL DEFAULT '');
CREATE INDEX modules_module ON modules (module);
-- The modules that each module the index read imports directly, from its reference file.
CREATE TABLE imports (module text COLLATE "C" NOT NULL, imported text COLLATE "C" NOT NULL);
CREATE INDEX imports_module ON imports (module);
CREATE INDEX imports_imported ON imports (imported);
-- A declaration is its module and its name: two modules that are never imported together can declare one name.
CREATE TABLE decls (name text COLLATE "C" NOT NULL, short text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, kind text COLLATE "C" NOT NULL, line integer NOT NULL, stmt_hash text COLLATE "C" NOT NULL);
CREATE INDEX decls_name ON decls (name);
CREATE INDEX decls_short ON decls (short);
CREATE INDEX decls_module ON decls (module);
-- One use of a constant: target and tmodule are its name and the module that Lean recorded for it; module, line,
-- col and parent are where the use is.
CREATE TABLE uses (target text COLLATE "C" NOT NULL, tmodule text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, line integer NOT NULL, col integer NOT NULL, parent text COLLATE "C" NOT NULL, cls text COLLATE "C" NOT NULL);
CREATE INDEX uses_target ON uses (target);
CREATE INDEX uses_module ON uses (module);
-- target, tmodule: the used constant as Lean recorded it; dst, module: the user. owner, owner_module: the declaration
-- that the used constant belongs to (ownerResolver), resolved when the row is written and again when it changes;
-- by_closure: the index's imports decided that resolution: it went through the import closure of tmodule, or found
-- no declaration of the constant's names, so that a change of tmodule's closure can move the owner.
CREATE TABLE edges (target text COLLATE "C" NOT NULL, tmodule text COLLATE "C" NOT NULL, dst text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, stmt boolean NOT NULL, kind text COLLATE "C" NOT NULL, owner text COLLATE "C" NOT NULL, owner_module text COLLATE "C" NOT NULL, by_closure boolean NOT NULL);
CREATE INDEX edges_module ON edges (module);
-- edges_owner is the index the reach walk reads: it visits a row for each edge out of its closure. It carries the
-- two names it is keyed by and no more: a btree row must stay under 2704 bytes, and a declaration's name has no
-- bound, so an index that also carried the user's name and module would refuse an edge of two long names that
-- this schema can hold (the longest edge of the copy's index takes 474 bytes for its four names).
CREATE INDEX edges_owner ON edges (owner_module, owner);
CREATE INDEX edges_target ON edges (target);
CREATE INDEX edges_closure ON edges (tmodule) WHERE by_closure;
-- What the Lean exporter read from the compiled module: every constant (generated ones included), its elaborated
-- type as printed, and its compiled statement hash.
CREATE TABLE consts (name text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, kind text COLLATE "C" NOT NULL, type text COLLATE "C" NOT NULL, cstmt text COLLATE "C" NOT NULL, PRIMARY KEY (name, module));
CREATE INDEX consts_module ON consts (module);
CREATE INDEX consts_cstmt ON consts (cstmt);
-- The relation atoms of each constant's statement, by canonical hash: who else states the same fact.
CREATE TABLE atoms (name text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, atom text COLLATE "C" NOT NULL);
CREATE INDEX atoms_atom ON atoms (atom);
CREATE INDEX atoms_name ON atoms (name);
CREATE INDEX atoms_module ON atoms (module);
-- The registry concepts each constant's statement states: how is direct, via:<definition> or uses.
CREATE TABLE facts (name text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, concept text COLLATE "C" NOT NULL, how text COLLATE "C" NOT NULL);
CREATE INDEX facts_concept ON facts (concept);
CREATE INDEX facts_name ON facts (name);
CREATE INDEX facts_module ON facts (module);
-- The modules that the compiled step read without the registry's forms (FormsClash), each with a name that kept it
-- from them: one row for each module (declarer) that declares the name in the module's import closure (forms false)
-- or in the forms' imports' closure (forms true). A declarer can be on both sides, when the two sides hold two axioms.
CREATE TABLE without_forms (module text COLLATE "C" NOT NULL, name text COLLATE "C" NOT NULL, declarer text COLLATE "C" NOT NULL, forms boolean NOT NULL, PRIMARY KEY (module, name, forms, declarer));
-- Each refresh that moved the index to another commit: landed, the commit it read, and previous, the commit it
-- replaced ('' for the first). The post-landing registry check of a landing compares those two commits.
CREATE TABLE landings (landed text COLLATE "C" PRIMARY KEY, previous text COLLATE "C" NOT NULL);
`

// analyzed are the tables a refresh reads statistics of again when it has written them: every table a query
// reads rows from. meta and landings hold a handful of rows that no plan turns on.
var analyzed = []string{"sources", "modules", "imports", "decls", "uses", "edges", "consts", "atoms", "facts", "without_forms"}

// Store is an open index: a pool to the index's database, whose connections resolve every unqualified name in the
// index's schema.
type Store struct {
	pool *pgxpool.Pool
	// index is the schema that holds this index, and the name every message of the store gives it.
	index string
	// q is what every read of the store goes through: the pool, or for a store opened read-only its snapshot.
	q querier
	// release ends what a store opened read-only holds: its snapshot, its shared hold on the index's lock, and
	// the connection that carries both.
	release func()
	// refresh is the connection that holds the index's lock while a refresh or a schema rebuild runs: its advisory
	// lock, and its write transactions, live and die with it (writeTx).
	refresh *pgxpool.Conn

	// mu guards derived, and serialises the reads of a store that two goroutines share: one connection runs one
	// query at a time.
	mu      sync.Mutex
	derived derived

	// afterRefreshLock is a test hook. Refresh runs it once it holds the index's lock, before the landed sync.
	afterRefreshLock func(context.Context) error
}

// derived is what a store reads from its index once and keeps for its answers: each module's source path. forget
// drops it.
type derived struct {
	paths map[string]string
}

// forget drops what the store derived from the index: a refresh has written it anew.
func (s *Store) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.derived = derived{}
}

// Open opens the index of t, creating its schema if the database has none and rebuilding it when it is of another
// schema version, so that the next refresh fills it from nothing. Only a refresh and the tests open an index so;
// every other command opens it read-only.
func Open(ctx context.Context, t Target) (*Store, error) {
	pool, err := openPool(ctx, t)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, index: t.Index}
	s.q = pool
	if err := s.init(ctx, t.Reader); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// ReadScope is what a store opened read-only reads besides the index, and so which part of a running refresh a
// reader waits for.
type ReadScope int

const (
	// IndexOnly reads nothing that a refresh's sync changes. It takes no lock: its snapshot holds one committed
	// state of the index while a refresh writes the next. land-hash reads so.
	IndexOnly ReadScope = iota
	// IndexAndTree also reads the configured tree (its HEAD, its commits or its build), which a refresh's sync
	// checks out and builds, so it holds the index's lock shared: it waits for the whole refresh, and a refresh
	// waits for it. The ledger and the gate read so.
	IndexAndTree
)

// OpenReadOnly opens an index that a refresh made, without creating or changing anything in it: the gate opens it
// so that its reads cannot modify a project's tree, the post-landing commands because they only read, and the
// queries likewise.
//
// The store reads one state of the index for its whole life: one repeatable-read transaction, begun here and ended
// by Close, carries every read. So a command never sees a refresh's write half done, nor rows of two refreshes. A
// refresh writes the reference part and the compiled part in two transactions; a reader between them reads the
// compiled part of the commit before, which its readers refuse (CompiledCommit).
//
// A reader of the tree (IndexAndTree) also holds the index's advisory lock shared, on the same connection and
// before its transaction begins, so that no refresh checks out or builds another commit of the tree while it
// reads. A reader of the index alone holds no lock: a refresh can commit while it reads, and the reader goes on
// reading the state its snapshot began with.
func OpenReadOnly(ctx context.Context, t Target, scope ReadScope) (*Store, error) {
	pool, err := openPool(ctx, t)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool, index: t.Index}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open the index %q read-only: %w", t.Index, err)
	}
	shared := scope == IndexAndTree
	if shared {
		if err := lock(ctx, conn, t.Index, false); err != nil {
			conn.Release()
			pool.Close()
			return nil, err
		}
	}
	// The snapshot lives until Close, not until ctx ends; its first read below begins it.
	snapshot, err := conn.BeginTx(context.WithoutCancel(ctx), pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		if shared {
			releaseLock(ctx, conn, t.Index, false)
		} else {
			conn.Release()
		}
		pool.Close()
		return nil, fmt.Errorf("open the index %q read-only: %w", t.Index, err)
	}
	s.q = snapshot
	s.release = func() {
		_ = snapshot.Rollback(context.WithoutCancel(ctx))
		if shared {
			releaseLock(context.WithoutCancel(ctx), conn, t.Index, false)
		} else {
			conn.Release()
		}
	}
	version, err := s.schemaVersion(ctx)
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("open the index %q read-only: %w", t.Index, err)
	}
	if version == "" {
		_ = s.Close()
		return nil, fmt.Errorf("the index %q was never refreshed: run declindex refresh", t.Index)
	}
	if version != SchemaVersion {
		_ = s.Close()
		return nil, fmt.Errorf("index %q has schema %q, and this declindex reads schema %q: run declindex refresh, which rebuilds it", t.Index, version, SchemaVersion)
	}
	return s, nil
}

// init makes the index's schema, or rebuilds an index of another schema version, under the index's lock: two
// Opens of one index never build it at once, and no refresh writes it while it is rebuilt. An index of this
// version needs no write, so Open of it takes no lock.
func (s *Store) init(ctx context.Context, reader string) error {
	if err := s.createSchema(ctx); err != nil {
		return err
	}
	version, err := s.schemaVersion(ctx)
	if err != nil || version == SchemaVersion {
		return err
	}
	unlock, err := s.lockRefresh(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Another process's Open may have rebuilt it while this one waited for the lock.
	if version, err = s.schemaVersion(ctx); err != nil || version == SchemaVersion {
		return err
	}
	name := pgx.Identifier{s.index}.Sanitize()
	// One statement, so one implicit transaction: an index is of its old version or of the new one, never of
	// half of it.
	if _, err := s.refresh.Exec(ctx, `DROP SCHEMA `+name+` CASCADE; CREATE SCHEMA `+name+`;`+schema); err != nil {
		return fmt.Errorf("create the schema of index %q: %w", s.index, err)
	}
	if reader != "" {
		role := pgx.Identifier{reader}.Sanitize()
		if _, err := s.refresh.Exec(ctx, `GRANT USAGE ON SCHEMA `+name+` TO `+role+`; GRANT SELECT ON ALL TABLES IN SCHEMA `+name+` TO `+role); err != nil {
			return fmt.Errorf("grant %s the reads of index %q: %w", reader, s.index, err)
		}
	}
	return setMeta(ctx, s.refresh, "schema", SchemaVersion)
}

// createSchema makes the index's schema when the database has none. Two Opens of a new index can run CREATE
// SCHEMA IF NOT EXISTS at once, and the one that loses the race is told the schema is there (Postgres checks the
// name before it inserts it): that is what it asked for.
func (s *Store) createSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{s.index}.Sanitize())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "42P06") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create the index %q: %w", s.index, err)
	}
	return nil
}

// schemaVersion is the index's schema version, empty for a schema that holds no index: one no refresh ever wrote.
func (s *Store) schemaVersion(ctx context.Context) (string, error) {
	var meta *string
	if err := s.q.QueryRow(ctx, `SELECT to_regclass('meta')::text`).Scan(&meta); err != nil {
		return "", err
	}
	if meta == nil {
		return "", nil
	}
	return s.Meta(ctx, "schema")
}

// Close closes the index: it ends a read-only store's snapshot, gives back its shared hold on the index's lock,
// and closes the pool.
func (s *Store) Close() error {
	if s.release != nil {
		s.release()
		s.release = nil
	}
	s.pool.Close()
	return nil
}

func setMeta(ctx context.Context, x dbtx, k, v string) error {
	_, err := x.Exec(ctx, `INSERT INTO meta (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v`, k, v)
	return err
}

// Meta reads one value the index keeps about itself (commit, built, schema, errors); empty when unset.
func (s *Store) Meta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.q.QueryRow(ctx, `SELECT v FROM meta WHERE k = $1`, k).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
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
		return fmt.Errorf("the index %q was never refreshed: run declindex refresh", s.index)
	}
	n, err := s.Meta(ctx, "errors")
	if err != nil {
		return err
	}
	if n != "0" {
		return fmt.Errorf("the index %q is incomplete: %s modules' reference files could not be read", s.index, n)
	}
	return nil
}

// lockRefresh takes the index's advisory lock exclusive, on a connection of its own, so that two refreshes never
// interleave, and gives the release of both. It waits for the lock until ctx ends.
//
// The whole refresh runs on that connection: its lock, and the two transactions that write the index. If the
// connection drops, the server gives up its lock and rolls back its open transaction together, so a refresh that
// loses its connection has written nothing and holds nothing.
func (s *Store) lockRefresh(ctx context.Context) (func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to the index %q: %w", s.index, err)
	}
	if err := lock(ctx, conn, s.index, true); err != nil {
		conn.Release()
		return nil, err
	}
	s.refresh = conn
	return func() {
		s.refresh = nil
		releaseLock(ctx, conn, s.index, true)
	}, nil
}

// writeTx runs f in one transaction of the index on the connection that holds its lock, and commits it when f
// succeeds. Every write of the index runs so: a writer holds the lock, and a lost connection takes the lock and
// the transaction with it.
func (s *Store) writeTx(ctx context.Context, f func(pgx.Tx) error) error {
	conn := s.refresh
	if conn == nil {
		return fmt.Errorf("write the index %q outside its refresh lock", s.index)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return unreachable(conn.Conn(), err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := f(tx); err != nil {
		return unreachable(conn.Conn(), err)
	}
	return unreachable(conn.Conn(), tx.Commit(ctx))
}

// analyze reads the statistics of the tables a refresh wrote, and marks their pages all-visible, on the
// connection that holds the refresh lock: COPY leaves neither, so the planner would read a declaration's reach
// with the wrong plan until autovacuum's nap ended, and the post-landing hooks and the owners' first queries run
// before that. VACUUM cannot run in a transaction block, so this is one statement of its own, after the writes
// commit.
func (s *Store) analyze(ctx context.Context) error {
	conn := s.refresh
	if conn == nil {
		return fmt.Errorf("analyze the index %q outside its refresh lock", s.index)
	}
	_, err := conn.Exec(ctx, "VACUUM (ANALYZE) "+strings.Join(analyzed, ", "))
	return unreachable(conn.Conn(), err)
}

// deleteCompiled drops what the exporter read from the modules, and their clashes with the forms.
func deleteCompiled(ctx context.Context, x dbtx, modules []string) error {
	if len(modules) == 0 {
		return nil
	}
	for _, t := range []string{"consts", "atoms", "facts", "without_forms"} {
		if _, err := x.Exec(ctx, "DELETE FROM "+t+" WHERE module = ANY($1)", modules); err != nil {
			return err
		}
	}
	return nil
}

// ModuleCount is the number of modules the index read.
func (s *Store) ModuleCount(ctx context.Context) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT COUNT(*) FROM modules`).Scan(&n)
	return n, err
}

// Landing gives the commit that the refresh to landed replaced (empty for the index's first commit), and whether a
// refresh recorded landed at all.
func (s *Store) Landing(ctx context.Context, landed string) (previous string, recorded bool, err error) {
	err = s.q.QueryRow(ctx, `SELECT previous FROM landings WHERE landed = $1`, landed).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
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
