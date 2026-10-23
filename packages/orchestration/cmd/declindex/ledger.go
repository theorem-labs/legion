package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// cmdLedger runs after a landing: it registers new contracts in the ledger and reports every contract whose
// ledger hash is not the index's. Exit code 1 when it finds a defect.
func cmdLedger(ctx context.Context, e *env, args []string) int {
	dsn := e.getenv("LEDGER_DSN")
	for i := 0; i < len(args); i++ {
		if args[i] != "--dsn-file" || i+1 >= len(args) {
			return fail(e.stderr, &usageError{"ledger takes only --dsn-file PATH"})
		}
		raw, err := os.ReadFile(args[i+1])
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		dsn = strings.TrimSpace(string(raw))
		i++
	}
	if dsn == "" {
		return fail(e.stderr, &usageError{"LEDGER_DSN or --dsn-file is required"})
	}
	if e.cfg.Repo == "" {
		return fail(e.stderr, &usageError{"the configuration names no repo"})
	}
	owners := declindex.Owners{}
	if e.cfg.Owners != "" {
		var err error
		if owners, err = declindex.LoadOwners(e.cfg.Owners, e.cfg.OwnersFormat); err != nil {
			return fail(e.stderr, &unableError{err})
		}
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	if c := e.staleNote(ctx); c != treeHead(e.cfg.Tree) {
		return fail(e.stderr, &unableError{fmt.Errorf("the index is at %q, not the tree's commit: refresh it after the landing first", c)})
	}
	if err := requireLedger(ctx, dsn, e.cfg.Repo); err != nil {
		return fail(e.stderr, &unableError{err})
	}
	l, err := ledger.Open(ctx, dsn)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	defer l.Close()
	r, err := e.store.CheckLedger(ctx, l, e.cfg.Repo, e.cfg.Contracts, owners)
	if err != nil {
		var refusal *ledger.Refusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(e.stderr, "declindex: the ledger refused: %v\n", refusal)
		}
		return fail(e.stderr, &unableError{err})
	}
	if c := e.printJSON(r); c != 0 {
		return c
	}
	if len(r.Defects) > 0 {
		return 1
	}
	return 0
}

// requireLedger checks, reading only, that the database holds the ledger's schema and the repository. The check
// writes to an existing ledger; it never creates one (ledger.Open would apply the schema to an empty database).
func requireLedger(ctx context.Context, dsn, repo string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to the ledger: %w", err)
	}
	defer conn.Close(ctx)
	var schema bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_versions') IS NOT NULL AND to_regclass('contracts') IS NOT NULL
		AND to_regclass('repositories') IS NOT NULL`).Scan(&schema); err != nil {
		return err
	}
	if !schema {
		return fmt.Errorf("the database has no ledger schema; the ledger check does not create one")
	}
	var known bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name = $1)`, repo).Scan(&known); err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("the ledger has no repository %s", repo)
	}
	return nil
}
