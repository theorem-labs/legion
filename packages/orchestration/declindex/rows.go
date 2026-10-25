package declindex

import (
	"context"
	"iter"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier reads the index: the pool, one connection of it, or one transaction (Store.q is a store's own reader,
// the pool for a writable store and its snapshot for a store opened read-only).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// dbtx reads and writes the index: the pool, one connection of it, or one transaction.
type dbtx interface {
	querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

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
