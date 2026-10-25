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
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaVersion changes whenever what an index holds changes: a table, a column, a meta key that a reader needs, or
// how a stored value is computed (a statement hash). An index of another version is rebuilt from nothing (Open), and
// OpenReadOnly refuses it.
// 18: 17 with edges_owner carrying what the reach walk reads, so that the walk reads the index alone.
// 17: the Postgres schema of 15 with the graph checks' consts columns of main's 14 (binders, conclusion). 16 was
// a head of this branch with another edges index, whose indexes must be rebuilt too.
const SchemaVersion = "18"

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
-- edges_owner is the index the reach walk reads: it visits a row for each edge out of its closure, and carries
-- every column that walk reads, so the walk never touches the table (24 % of the reach of the most-used
-- declaration of the copy's tree: 3.27 s to 2.63 s).
-- One entry of it therefore holds four names and a kind, and a btree entry holds at most 2704 bytes. A tree
-- whose edge would exceed that is refused as an index defect, which names it, before anything is written
-- (NameTooLongError, indexEntryLimit): the longest edge of the copy's index holds 474 bytes of names, so the
-- limit is about three times the longest name that tree has.
CREATE INDEX edges_owner ON edges (owner_module, owner) INCLUDE (dst, module, stmt, kind);
CREATE INDEX edges_target ON edges (target);
CREATE INDEX edges_closure ON edges (tmodule) WHERE by_closure;
-- What the Lean exporter read from the compiled module: every constant (generated ones included), its elaborated
-- type as printed, its compiled statement hash, the heads of its explicit binders' types (a JSON array, in order)
-- and the head of its conclusion (Constant.Binders and Constant.Conclusion).
CREATE TABLE consts (name text COLLATE "C" NOT NULL, module text COLLATE "C" NOT NULL, kind text COLLATE "C" NOT NULL, type text COLLATE "C" NOT NULL, cstmt text COLLATE "C" NOT NULL, binders text COLLATE "C" NOT NULL, conclusion text COLLATE "C" NOT NULL, PRIMARY KEY (name, module));
CREATE INDEX consts_module ON consts (module);
CREATE INDEX consts_cstmt ON consts (cstmt);
CREATE INDEX consts_conclusion ON consts (conclusion);
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

// Store is an open index: a pool to the index's database. Every statement is unqualified and runs in a
// transaction that names the index's schema first (namedSchema), since no connection keeps a schema of its own.
type Store struct {
	pool *pgxpool.Pool
	// index is the schema that holds this index, and the name every message of the store gives it.
	index string
	// treeLock is the file whose flock guards the indexed tree (Target.TreeLock); empty guards nothing.
	treeLock string
	// q is what every read of the store goes through: for a writable store a transaction for each read
	// (poolReader), for a store opened read-only its snapshot.
	q querier
	// release ends what a store opened read-only holds: its snapshot, the connection that carries it, and its
	// shared hold on the tree.
	release func()
	// refresh is the connection that a refresh or a schema rebuild writes on, while it holds the tree's lock.
	// Each write transaction on it takes the index's lock for itself (writeTx).
	refresh *pgxpool.Conn

	// mu guards derived, and serialises the reads of a store that two goroutines share: one connection runs one
	// query at a time.
	mu      sync.Mutex
	derived derived

	// afterRefreshLock is a test hook. Refresh runs it once it holds the tree's lock, before the landed sync.
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
	s := &Store{pool: pool, index: t.Index, treeLock: t.TreeLock}
	s.q = poolReader{pool: pool, index: t.Index}
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
	// checks out and builds, so it holds the tree's lock shared (Target.TreeLock): it waits for the whole
	// refresh, and a refresh waits for it. The ledger and the gate read so.
	IndexAndTree
)

// OpenReadOnly opens an index that a refresh made, without creating or changing anything in it: the gate opens it
// so that its reads cannot modify a project's tree, the post-landing commands because they only read, and the
// queries likewise.
//
// The store reads one state of the index for its whole life: one repeatable-read transaction, begun here and ended
// by Close, carries every read, and names the index's schema first. So a command never sees a refresh's write half
// done, nor rows of two refreshes. A refresh writes the reference part and the compiled part in two transactions;
// a reader between them reads the compiled part of the commit before, which its readers refuse (CompiledCommit).
//
// A reader of the tree (IndexAndTree) also holds the tree's lock shared, from before its transaction begins until
// Close, so that no refresh checks out or builds another commit of the tree while it reads. A reader of the index
// alone holds no lock: a refresh can commit while it reads, and the reader goes on reading the state its snapshot
// began with.
func OpenReadOnly(ctx context.Context, t Target, scope ReadScope) (*Store, error) {
	unlockTree := func() {}
	if scope == IndexAndTree && t.TreeLock != "" {
		var err error
		if unlockTree, err = lockTree(ctx, t.TreeLock, false); err != nil {
			return nil, err
		}
	}
	pool, err := openPool(ctx, t)
	if err != nil {
		unlockTree()
		return nil, err
	}
	fail := func(err error) (*Store, error) {
		pool.Close()
		unlockTree()
		return nil, fmt.Errorf("open the index %q read-only: %w", t.Index, err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fail(err)
	}
	// The snapshot lives until Close, not until ctx ends.
	snapshot, err := conn.BeginTx(context.WithoutCancel(ctx), pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		conn.Release()
		return fail(err)
	}
	if _, err := snapshot.Exec(ctx, namedSchema(t.Index)); err != nil {
		_ = snapshot.Rollback(context.WithoutCancel(ctx))
		conn.Release()
		return fail(err)
	}
	s := &Store{pool: pool, index: t.Index, treeLock: t.TreeLock, q: snapshot}
	s.release = func() {
		_ = snapshot.Rollback(context.WithoutCancel(ctx))
		conn.Release()
		unlockTree()
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

// init makes the index's schema, or rebuilds an index of another schema version, in one transaction that holds
// the index's lock: two Opens of one index never build it at once, and no refresh writes it while it is rebuilt.
// An index of this version needs no write, so Open of it writes nothing.
func (s *Store) init(ctx context.Context, reader string) error {
	if err := s.createSchema(ctx); err != nil {
		return err
	}
	version, err := s.schemaVersion(ctx)
	if err != nil || version == SchemaVersion {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect to the index %q: %w", s.index, err)
	}
	s.refresh = conn
	defer func() {
		s.refresh = nil
		conn.Release()
	}()
	name := pgx.Identifier{s.index}.Sanitize()
	return s.writeTx(ctx, func(tx pgx.Tx) error {
		// Another process's Open may have rebuilt it while this one waited for the lock.
		if version, err := schemaVersionIn(ctx, tx, s.index); err != nil || version == SchemaVersion {
			return err
		}
		// The transaction named the schema by its name, so the tables below go into the schema made again.
		if _, err := tx.Exec(ctx, `DROP SCHEMA `+name+` CASCADE; CREATE SCHEMA `+name+`;`+schema); err != nil {
			return fmt.Errorf("create the schema of index %q: %w", s.index, err)
		}
		if reader != "" {
			role := pgx.Identifier{reader}.Sanitize()
			if _, err := tx.Exec(ctx, `GRANT USAGE ON SCHEMA `+name+` TO `+role+`; GRANT SELECT ON ALL TABLES IN SCHEMA `+name+` TO `+role); err != nil {
				return fmt.Errorf("grant %s the reads of index %q: %w", reader, s.index, err)
			}
		}
		return setMeta(ctx, tx, "schema", SchemaVersion)
	})
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
	return schemaVersionIn(ctx, s.q, s.index)
}

// schemaVersionIn reads the schema version of index with every name qualified, so that it never reads another
// schema's meta: Open decides on it whether to drop and rebuild the index.
func schemaVersionIn(ctx context.Context, q querier, index string) (string, error) {
	meta := pgx.Identifier{index, "meta"}.Sanitize()
	var found *string
	if err := q.QueryRow(ctx, `SELECT to_regclass($1)::text`, meta).Scan(&found); err != nil {
		return "", err
	}
	if found == nil {
		return "", nil
	}
	var v string
	err := q.QueryRow(ctx, `SELECT v FROM `+meta+` WHERE k = 'schema'`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
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

// lockRefresh takes the tree's lock exclusive (Target.TreeLock), so that two refreshes never interleave and no
// reader of the tree reads while a refresh syncs, builds and reads it, and gives a connection to write on. It
// waits for the lock until ctx ends, and gives the release of both.
//
// The tree's lock is the machine's, not the server's: every route to the shared server pools transactions, so no
// lock of the server outlives the transaction that takes it. Each write transaction therefore takes the index's
// lock for itself (writeTx); a refresh from another machine is held back by that lock, one transaction at a time.
func (s *Store) lockRefresh(ctx context.Context) (func(), error) {
	unlockTree := func() {}
	if s.treeLock != "" {
		var err error
		if unlockTree, err = lockTree(ctx, s.treeLock, true); err != nil {
			return nil, err
		}
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		unlockTree()
		return nil, fmt.Errorf("connect to the index %q: %w", s.index, err)
	}
	s.refresh = conn
	return func() {
		s.refresh = nil
		conn.Release()
		unlockTree()
	}, nil
}

// writeTx runs f in one transaction of the index and commits it when f succeeds. Every write of the index runs
// so. The transaction names the index's schema and takes the index's lock before f runs (xactLock), so that two
// transactions that write one index run one after the other wherever they come from, and a lost connection rolls
// the transaction back with its lock.
func (s *Store) writeTx(ctx context.Context, f func(pgx.Tx) error) error {
	return s.writeTxFrom(ctx, -1, f)
}

// writeTxFrom is writeTx for a write whose content rests on reads made before the lock: it refuses when another
// writer has written the index since generation, which a refresh reads before it reads the index and the tree.
// On a pooled route no lock outlives its transaction, so this is what holds two refreshes of one index apart
// when they run on different machines: the second to write wins, and the first is told to run again (75).
func (s *Store) writeTxFrom(ctx context.Context, generation int64, f func(pgx.Tx) error) error {
	conn := s.refresh
	if conn == nil {
		return fmt.Errorf("write the index %q outside its refresh lock", s.index)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return unreachable(conn.Conn(), err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, namedSchema(s.index)); err != nil {
		return unreachable(conn.Conn(), err)
	}
	if err := xactLock(ctx, tx, s.index); err != nil {
		return unreachable(conn.Conn(), err)
	}
	if generation >= 0 {
		now, err := generationIn(ctx, tx)
		if err != nil {
			return unreachable(conn.Conn(), err)
		}
		if now != generation {
			return &TempError{Err: fmt.Errorf("another refresh wrote the index %q while this one read it (generation %d, now %d): run it again",
				s.index, generation, now)}
		}
	}
	if err := f(tx); err != nil {
		return unreachable(conn.Conn(), err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO meta (k, v) VALUES ('generation', '1')
		ON CONFLICT (k) DO UPDATE SET v = (meta.v::bigint + 1)::text`); err != nil {
		return unreachable(conn.Conn(), err)
	}
	return unreachable(conn.Conn(), tx.Commit(ctx))
}

// generation counts the write transactions of the index: every one of them raises it by one.
func (s *Store) generation(ctx context.Context) (int64, error) {
	return generationIn(ctx, s.q)
}

func generationIn(ctx context.Context, q querier) (int64, error) {
	var v string
	err := q.QueryRow(ctx, `SELECT v FROM meta WHERE k = 'generation'`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil || v == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the index's generation %q is no number: %w", v, err)
	}
	return n, nil
}

// analyze reads the statistics of the tables a refresh wrote: COPY leaves none, so the planner would read a
// declaration's reach with the wrong plan until autovacuum's nap ended, and the post-landing hooks and the owners'
// first queries run before that. VACUUM cannot run in a transaction block, so it is one statement of its own,
// after the writes commit, with every table named with its schema, since no transaction names it. It does not
// truncate: a reader that opened after the commit holds the tables a landing emptied rows of, and VACUUM would
// wait five seconds for each of them to give the lock back.
func (s *Store) analyze(ctx context.Context) error {
	conn := s.refresh
	if conn == nil {
		return fmt.Errorf("analyze the index %q outside its refresh lock", s.index)
	}
	tables := make([]string, len(analyzed))
	for i, t := range analyzed {
		tables[i] = pgx.Identifier{s.index, t}.Sanitize()
	}
	_, err := conn.Exec(ctx, "VACUUM (ANALYZE, TRUNCATE false) "+strings.Join(tables, ", "))
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
// join two commits' rows, so it is refused. A refresh that reads the compiled part records that the index owes
// one, so a first compiled step that never wrote anything is refused too, rather than answered as an index read
// without a compiled part. An index that owes none (lean.export off) has none: required refuses it; else it is
// accepted, and its compiled answers are empty.
func (s *Store) CompiledCommit(ctx context.Context, required bool) (string, error) {
	commit, err := s.Meta(ctx, "commit")
	if err != nil {
		return "", err
	}
	compiled, err := s.Meta(ctx, "compiled_commit")
	if err != nil {
		return "", err
	}
	owed, err := s.Meta(ctx, "compiled_owed")
	if err != nil {
		return "", err
	}
	switch {
	// Owed first: outside Git the commit is empty, and an empty compiled commit would pass as equal to it. An
	// index that owes a compiled part and holds an older one is named by both commits, as before.
	case owed == "1" && compiled != "":
		return "", fmt.Errorf("the index's compiled part is of %s, not of its commit %s: refresh the index", compiled, commit)
	case owed == "1":
		return "", fmt.Errorf("the index's last refresh read no compiled part of %q: its compiled step did not finish, so the index states no fact; refresh the index", commit)
	case compiled == commit:
		return commit, nil
	case compiled == "" && !required:
		return commit, nil
	case compiled == "":
		return "", errors.New("the index was read without its compiled part: refresh it with lean.export on")
	default:
		return "", fmt.Errorf("the index's compiled part is of %s, not of its commit %s: refresh the index", compiled, commit)
	}
}
