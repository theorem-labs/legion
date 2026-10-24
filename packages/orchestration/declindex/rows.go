package declindex

import "database/sql"

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
