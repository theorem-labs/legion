// Package ledger is the orchestration ledger: work-item leases, migrations and their change locks, the contract
// ledger and an append-only event log, all in Postgres. Every operation runs in one transaction.
package ledger

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// tableStatement finds a migration's statements that create or drop a table, at the start of a line.
var tableStatement = regexp.MustCompile(`(?im)^\s*(create|drop)\s+table\s+(?:if\s+(?:not\s+)?exists\s+)?([a-z_][a-z0-9_]*)`)

// migrationTables are the tables that the migration scripts, applied in order, leave: those they create, less
// those a later one drops. It does not follow a rename: the old name would stay required, and OpenExisting would
// refuse every database until the rename is added here.
func migrationTables(scripts ...string) []string {
	var tables []string
	for _, script := range scripts {
		for _, m := range tableStatement.FindAllStringSubmatch(script, -1) {
			name := strings.ToLower(m[2])
			tables = slices.DeleteFunc(tables, func(t string) bool { return t == name })
			if strings.EqualFold(m[1], "create") {
				tables = append(tables, name)
			}
		}
	}
	return tables
}

// schemaTables is every table the embedded ledger schema needs, so that OpenExisting refuses a database whose
// version record survived a damaged table.
func schemaTables() ([]string, error) {
	files, err := schemaFiles()
	if err != nil {
		return nil, err
	}
	scripts := make([]string, len(files))
	for i, file := range files {
		raw, err := schemaFS.ReadFile(file)
		if err != nil {
			return nil, err
		}
		scripts[i] = string(raw)
	}
	tables := migrationTables(scripts...)
	if len(tables) == 0 {
		return nil, errors.New("the embedded ledger schema creates no table")
	}
	return tables, nil
}

// Store is a connection pool to one ledger database.
type Store struct {
	pool *pgxpool.Pool
}

// SchemaError means an existing ledger database lacks an embedded schema version or a required ledger table.
// OpenExisting returns it without applying any migration.
type SchemaError struct {
	Missing []int
	Tables  []string
}

func (e *SchemaError) Error() string {
	if len(e.Tables) != 0 {
		return fmt.Sprintf("ledger schema is missing versions %v or tables %v", e.Missing, e.Tables)
	}
	return fmt.Sprintf("ledger schema is missing versions %v", e.Missing)
}

// Open connects to the database and applies any schema versions it lacks.
func Open(ctx context.Context, dsn string) (*Store, error) {
	return connect(ctx, dsn, (*Store).migrate)
}

// OpenExisting connects to an existing ledger database and verifies that it has every schema version embedded in
// this binary. It never applies a migration.
func OpenExisting(ctx context.Context, dsn string) (*Store, error) {
	return connect(ctx, dsn, (*Store).checkSchema)
}

// connect opens a pool to dsn and gives its store once ready accepts it; it closes the pool that ready refuses.
func connect(ctx context.Context, dsn string, ready func(*Store, context.Context) error) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := ready(s, ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// SchemaVersion is the highest schema version applied.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_versions`).Scan(&v)
	return v, err
}

func schemaFiles() ([]string, error) {
	files, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func schemaFileVersion(file string) (int, error) {
	base := strings.TrimPrefix(file, "schema/")
	n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
	if err != nil {
		return 0, fmt.Errorf("schema file %s: name must start with a version number", base)
	}
	return n, nil
}

func (s *Store) checkSchema(ctx context.Context) error {
	files, err := schemaFiles()
	if err != nil {
		return err
	}
	want := make([]int, len(files))
	for i, file := range files {
		if want[i], err = schemaFileVersion(file); err != nil {
			return err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_versions`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return &SchemaError{Missing: want}
		}
		return fmt.Errorf("read schema versions: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	var missing []int
	for _, version := range want {
		if !slices.Contains(versions, version) {
			missing = append(missing, version)
		}
	}
	if len(missing) != 0 {
		return &SchemaError{Missing: missing}
	}
	tables, err := schemaTables()
	if err != nil {
		return err
	}
	var missingTables []string
	for _, table := range tables {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
			return fmt.Errorf("check ledger table %s: %w", table, err)
		}
		if !exists {
			missingTables = append(missingTables, table)
		}
	}
	if len(missingTables) != 0 {
		return &SchemaError{Missing: want, Tables: missingTables}
	}
	return nil
}

// migrate applies schema/NNNN_*.sql files in order, each once, under an advisory lock so concurrent starts agree.

func (s *Store) migrate(ctx context.Context) error {
	files, err := schemaFiles()
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7243100109)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_versions (
			version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, f := range files {
			base := strings.TrimPrefix(f, "schema/")
			n, err := schemaFileVersion(f)
			if err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_versions WHERE version = $1)`, n).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			sql, err := schemaFS.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("apply schema %s: %w", base, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_versions (version, name) VALUES ($1, $2)`, n, base); err != nil {
				return err
			}
		}
		return nil
	})
}

// tx runs fn in one transaction. A serialization failure or deadlock is retried, so callers see either the
// operation's result or its refusal, never a transient conflict.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := pgx.BeginFunc(ctx, s.pool, fn)
		var pe *pgconn.PgError
		if attempt < 20 && errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01") {
			continue
		}
		return err
	}
}

// lockRepo takes the repository's row lock, which serialises every change to its migrations' locks, and returns
// the repository. A missing repository is NotFoundError.
func lockRepo(ctx context.Context, tx pgx.Tx, name string, forUpdate bool) (Repository, error) {
	q := `SELECT name, max_landing_rounds, created_at FROM repositories WHERE name = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	} else {
		q += ` FOR SHARE`
	}
	var c Repository
	err := tx.QueryRow(ctx, q, name).Scan(&c.Name, &c.MaxLandingRounds, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, &NotFoundError{What: "repository", ID: name}
	}
	return c, err
}
