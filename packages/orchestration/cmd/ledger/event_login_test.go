package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// ledger events prints, for each event, the login whose session appended it: two login roles each enqueue an item
// through the command line, and each item.enqueued event names its own role.
func TestEventsCommandPrintsTheLoginOfEachEvent(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_event_login")
	invoke := func(dsn string, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(ctx, args, func(key string) string {
			if key == "LEDGER_DSN" {
				return dsn
			}
			return ""
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		return stdout.String()
	}
	invoke(dsn, "repo", "ensure", "r", "3")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var admin string
	err = conn.QueryRow(ctx, `SELECT session_user`).Scan(&admin)
	_ = conn.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alice, aliceDSN := testpg.Login(t, dsn, "ledger_alice")
	bob, bobDSN := testpg.Login(t, dsn, "ledger_bob")
	invoke(aliceDSN, "enqueue", "r", "item-a", "struct:mount")
	invoke(bobDSN, "enqueue", "r", "item-b", "struct:mount")

	var events []struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
		Login   string `json:"login"`
	}
	if err := json.Unmarshal([]byte(invoke(dsn, "--json", "events", "r", "0")), &events); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, event := range events {
		got[event.Kind+" "+event.Subject] = event.Login
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
