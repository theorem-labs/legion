package declindex

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// eachRow calls scan on every row of a query's result and closes rows. err is the query's own error, so a call
// takes QueryContext's two results. The first error wins: the query's, a scan's, the iteration's, then Close's;
// rows is closed on every path.
func eachRow(rows *sql.Rows, err error, scan func(*sql.Rows) error) error {
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

// maxInParams bounds the values of one `IN (…)` list, well below SQLite's limit on a statement's parameters.
const maxInParams = 500

// eachRowIn runs query, which ends in `IN`, once per chunk of at most maxInParams values with the chunk as its
// list, and calls scan on every row (see eachRow). No value runs no query.
func eachRowIn(ctx context.Context, q querier, query string, values []string, scan func(*sql.Rows) error) error {
	for chunk := range slices.Chunk(values, maxInParams) {
		rows, err := q.QueryContext(ctx, query+" ("+placeholders(len(chunk))+")", anys(chunk)...)
		if err := eachRow(rows, err, scan); err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

const (
	// foundColumns are the columns of a Found that scanFound reads, of a declaration d and its module m (foundFrom).
	foundColumns = `d.name, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')`
	foundFrom    = ` FROM decls d LEFT JOIN modules m ON m.module = d.module`
	// foundSelect selects Founds; a query adds its WHERE clause.
	foundSelect = `SELECT ` + foundColumns + foundFrom
)

// scanFound reads a row of foundColumns and then of the further columns into more.
func scanFound(r *sql.Rows, more ...any) (Found, error) {
	var f Found
	var rel string
	if err := r.Scan(append([]any{&f.Name, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel}, more...)...); err != nil {
		return f, err
	}
	f.Path = sourcePath(rel, f.Module)
	return f, nil
}
