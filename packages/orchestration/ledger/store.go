// Package ledger is the orchestration ledger: work-item leases, migrations and their change locks, the contract
// ledger and an append-only event log, all in Postgres. Every operation runs in one transaction.
package ledger

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Store is a connection pool to one ledger database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database and applies any schema versions it lacks.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
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

// migrate applies schema/NNNN_*.sql files in order, each once, under an advisory lock so concurrent starts agree.
func (s *Store) migrate(ctx context.Context) error {
	files, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
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
			n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("schema file %s: name must start with a version number", base)
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
