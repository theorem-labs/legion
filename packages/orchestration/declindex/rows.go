package declindex

import (
	"context"
	"iter"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// querier reads the index: one transaction, or poolReader (Store.q is a store's own reader: its snapshot for a
// store opened read-only, poolReader for a writable one).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// dbtx reads and writes the index: one transaction or one connection.
type dbtx interface {
	querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// poolReader is the reader of a writable store: each read is a transaction of its own that first names the
// index's schema (namedSchema), because no connection keeps a schema of its own between transactions (openPool).
// The transaction ends when its rows are closed, or once its one row is scanned.
type poolReader struct {
	pool  *pgxpool.Pool
	index string
}

func (r poolReader) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, namedSchema(r.index)); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return tx, nil
}

func (r poolReader) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return &txRows{Rows: rows, tx: tx, ctx: ctx}, nil
}

func (r poolReader) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx, err := r.begin(ctx)
	if err != nil {
		return errRow{err}
	}
	return txRow{row: tx.QueryRow(ctx, sql, args...), tx: tx, ctx: ctx}
}

// txRows ends its read-only transaction when its rows are closed.
type txRows struct {
	pgx.Rows
	tx  pgx.Tx
	ctx context.Context
}

func (r *txRows) Close() {
	r.Rows.Close()
	_ = r.tx.Rollback(context.WithoutCancel(r.ctx))
}

// txRow ends its read-only transaction once its row is scanned.
type txRow struct {
	row pgx.Row
	tx  pgx.Tx
	ctx context.Context
}

func (r txRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	_ = r.tx.Rollback(context.WithoutCancel(r.ctx))
	return err
}

// errRow is a row whose read could not begin.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// eachRow calls scan on every row of a query's result and closes rows. err is the query's own error, so a call
// takes Query's two results. The first error wins: the query's, a scan's, then the iteration's; rows is closed on
// every path, as it must be before the next query of its connection runs.
func eachRow(rows pgx.Rows, err error, scan func(pgx.Rows) error) error {
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// copySeq is a pgx.CopyFromSource over a sequence of rows: CopyFrom reads one row, writes it to the server, and
// asks for the next, so a refresh of a whole project never holds its millions of rows at once. The sequence may
// give one slice over and over, since CopyFrom writes each row before it calls Next again.
type copySeq struct {
	next func() ([]any, bool)
	stop func()
	row  []any
}

func copyFrom(rows iter.Seq[[]any]) *copySeq {
	next, stop := iter.Pull(rows)
	return &copySeq{next: next, stop: stop}
}

func (c *copySeq) Next() bool {
	row, ok := c.next()
	c.row = row
	if !ok {
		c.stop()
	}
	return ok
}

func (c *copySeq) Values() ([]any, error) { return c.row, nil }

// Err is the error of reading the rows; a sequence of rows already in memory has none.
func (c *copySeq) Err() error { return nil }

const (
	// foundColumns are the columns of a Found that scanFound reads, of a declaration d and its module m (foundFrom).
	foundColumns = `d.name, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')`
	foundFrom    = ` FROM decls d LEFT JOIN modules m ON m.module = d.module`
	// foundSelect selects Founds; a query adds its WHERE clause.
	foundSelect = `SELECT ` + foundColumns + foundFrom
)

// scanFound reads a row of foundColumns and then of the further columns into more.
func scanFound(r pgx.Rows, more ...any) (Found, error) {
	var f Found
	var rel string
	if err := r.Scan(append([]any{&f.Name, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel}, more...)...); err != nil {
		return f, err
	}
	f.Path = sourcePath(rel, f.Module)
	return f, nil
}
