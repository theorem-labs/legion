// Package testpg gives a test a Postgres database of its own on the Postgres that LEGION_TEST_PG_DSN
// names, dropped when the test ends, so no test sees another's rows. A test is skipped without it:
// there is nothing to run against. It follows packages/daemon/internal/testpg, which this module
// cannot import.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Database is the URL of a new, empty database named <prefix>_<random>, dropped at the test's end.
func Database(t testing.TB, prefix string) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. Start one and set it:\n" +
			"  docker run -d --name orchestration-pg -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 mirror.gcr.io/library/postgres:17\n" +
			"  port=$(docker port orchestration-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://postgres:test@127.0.0.1:$port/postgres go test ./...")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	admin := *base
	admin.Path = "/postgres"
	conn, err := pgx.Connect(context.Background(), admin.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := prefix + "_" + hex.EncodeToString(suffix[:])
	if _, err := conn.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	database := *base
	database.Path = "/" + name
	return database.String()
}

// Login creates a login role named <prefix>_<random> that may read and write every table of the database dsn names,
// and returns the role and a URL of that database that connects as it. A test uses it to see which login made a
// write. The role is dropped at the test's end; roles belong to the whole server, so the random suffix keeps
// concurrent tests apart.
func Login(t testing.TB, dsn, prefix string) (string, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	var suffix, secret [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	role, password := prefix+"_"+hex.EncodeToString(suffix[:]), hex.EncodeToString(secret[:])
	quoted := pgx.Identifier{role}.Sanitize()
	// The role's privileges live in this database, so DROP OWNED runs here, before the role itself goes.
	t.Cleanup(func() {
		if _, err := conn.Exec(ctx, "drop owned by "+quoted); err != nil {
			t.Errorf("drop what %s owns: %v", role, err)
		}
		if _, err := conn.Exec(ctx, "drop role "+quoted); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	for _, statement := range []string{
		"create role " + quoted + " login password '" + password + "'",
		// Opening a ledger runs CREATE TABLE IF NOT EXISTS, which needs CREATE on the schema even when the table exists.
		"grant usage, create on schema public to " + quoted,
		"grant all on all tables in schema public to " + quoted,
		"grant all on all sequences in schema public to " + quoted,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	login, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", dsn, err)
	}
	login.User = url.UserPassword(role, password)
	return role, login.String()
}
