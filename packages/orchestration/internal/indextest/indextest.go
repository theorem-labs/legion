// Package indextest gives a declaration-index test a Postgres database of its own and, when it needs
// one, a read-only role on it. It does not import declindex, so the package's own tests can use it: a
// test builds declindex.Target{DSN: indextest.DSN(t), Index: "idx"}.
package indextest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// DSN is the connection string of a new, empty database, dropped when the test ends. Outside CI a test
// without LEGION_TEST_PG_DSN is skipped; in CI (CI set) it fails, so a missing server never passes as a
// skip.
func DSN(t testing.TB) string {
	t.Helper()
	if os.Getenv("CI") != "" && os.Getenv("LEGION_TEST_PG_DSN") == "" {
		t.Fatal("CI is set but LEGION_TEST_PG_DSN is not: the declaration index tests need a Postgres server")
	}
	return testpg.Database(t, "declindex")
}

// ReaderRole makes a LOGIN role with a password of its own on the server dsn names, and returns its
// name and a DSN that connects to dsn's database as that role. The role is dropped, with every
// privilege granted to it in that database, when the test ends. Call it after DSN, so its cleanup runs
// before the database is dropped.
func ReaderRole(t testing.TB, dsn string) (role string, readerDSN string) {
	t.Helper()
	ctx := context.Background()
	role = "declindex_reader_" + randomHex(t)
	password := randomHex(t) + randomHex(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to make a reader role: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	name := pgx.Identifier{role}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE ROLE "+name+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatalf("create role %s: %v", role, err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Errorf("connect to drop role %s: %v", role, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		for _, stmt := range []string{"DROP OWNED BY " + name, "DROP ROLE " + name} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
				return
			}
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	u.User = url.UserPassword(role, password)
	return role, u.String()
}

func randomHex(t testing.TB) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}
