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
