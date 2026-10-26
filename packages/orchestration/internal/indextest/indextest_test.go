package indextest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A reader role logs in, reads what it is granted, and is gone after its test, grants and all.
func TestReaderRoleLogsInAndIsDropped(t *testing.T) {
	ctx := context.Background()
	dsn := DSN(t)
	var role string
	t.Run("reader", func(t *testing.T) {
		var readerDSN string
		role, readerDSN = ReaderRole(t, dsn)
		admin, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = admin.Close(ctx) }()
		name := pgx.Identifier{role}.Sanitize()
		for _, stmt := range []string{
			"CREATE SCHEMA idx",
			"CREATE TABLE idx.meta (k text PRIMARY KEY, v text NOT NULL)",
			"INSERT INTO idx.meta VALUES ('schema', '14')",
			"GRANT USAGE ON SCHEMA idx TO " + name,
			"GRANT SELECT ON ALL TABLES IN SCHEMA idx TO " + name,
		} {
			if _, err := admin.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		reader, err := pgx.Connect(ctx, readerDSN)
		if err != nil {
			t.Fatalf("the reader cannot log in: %v", err)
		}
		defer func() { _ = reader.Close(ctx) }()
		var who, v string
		if err := reader.QueryRow(ctx, "SELECT current_user, (SELECT v FROM idx.meta WHERE k = 'schema')").Scan(&who, &v); err != nil {
			t.Fatal(err)
		}
		if who != role || v != "14" {
			t.Fatalf("reader read (%q, %q), want (%q, %q)", who, v, role, "14")
		}
		if _, err := reader.Exec(ctx, "INSERT INTO idx.meta VALUES ('x', 'y')"); err == nil {
			t.Fatal("the reader wrote to the index")
		}
	})
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname = $1", role).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("role %s outlived its test", role)
	}
}
