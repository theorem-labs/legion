package ledger_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// sessionUser is the login that dsn connects as.
func sessionUser(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	var login string
	if err := conn.QueryRow(ctx, `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	return login
}

// applySchemas applies the named schema files in order on conn.
func applySchemas(t *testing.T, conn *pgx.Conn, paths ...string) {
	t.Helper()
	for _, path := range paths {
		schema, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := conn.Exec(context.Background(), string(schema)); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
}

// TestEventsRecordTheLoginThatWroteThem catches an event log that names no login, or the wrong one. Two login roles
// each append an event through a Store of their own, and each event names the role whose session wrote it.
func TestEventsRecordTheLoginThatWroteThem(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	admin := sessionUser(t, dsn)
	alice, aliceDSN := testpg.Login(t, dsn, "ledger_alice")
	bob, bobDSN := testpg.Login(t, dsn, "ledger_bob")
	for _, writer := range []struct{ dsn, item string }{{aliceDSN, "item-a"}, {bobDSN, "item-b"}} {
		store := must(ledger.Open(ctx, writer.dsn))
		err := store.Enqueue(ctx, "r", writer.item, "struct:mount")
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	for _, e := range must(s.Events(ctx, "r", 0, "")) {
		got[e.Kind+" "+e.Subject] = e.Login
	}
	want := map[string]string{
		"repository.configured r": admin,
		"item.enqueued item-a":    alice,
		"item.enqueued item-b":    bob,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event logins = %v, want %v", got, want)
	}
}

// TestEventLoginMigrationNamesTheMigratingLogin catches schema 0006 leaving the events it finds without a login, or
// the new column without its default: an event appended before the schema names the login that applied it, one
// appended after names the login that appended it, and no event can have none.
func TestEventLoginMigrationNamesTheMigratingLogin(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_event_login_schema")
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	applySchemas(t, conn,
		"schema/0001_init.sql",
		"schema/0002_work_item_owner.sql",
		"schema/0003_ready_upstream_rounds.sql",
		"schema/0004_contract_last_change.sql",
		"schema/0005_round_keys_body.sql",
	)
	if _, err := conn.Exec(ctx, `INSERT INTO events (repo, kind, subject) VALUES ('r', 'old.event', 'before')`); err != nil {
		t.Fatal(err)
	}
	applySchemas(t, conn, "schema/0006_batches.sql")
	if _, err := conn.Exec(ctx, `INSERT INTO events (repo, kind, subject) VALUES ('r', 'new.event', 'after')`); err != nil {
		t.Fatal(err)
	}
	migrator := sessionUser(t, dsn)
	rows := must(conn.Query(ctx, `SELECT subject, login FROM events ORDER BY id`))
	type login struct{ subject, login string }
	got := must(pgx.CollectRows(rows, func(row pgx.CollectableRow) (login, error) {
		var l login
		err := row.Scan(&l.subject, &l.login)
		return l, err
	}))
	if want := []login{{"before", migrator}, {"after", migrator}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event logins = %+v, want %+v", got, want)
	}
	_, err := conn.Exec(ctx, `INSERT INTO events (repo, kind, subject, login) VALUES ('r', 'null.login', 'x', NULL)`)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23502" {
		t.Fatalf("an event with no login: %v, want a not-null violation", err)
	}
}
