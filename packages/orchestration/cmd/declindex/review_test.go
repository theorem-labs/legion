package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// A module whose reference file cannot be read leaves the index incomplete: every query is unable to check.
func TestQueriesRefuseAnIndexWithUnreadableModules(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	wrap := filepath.Join(filepath.Dir(config), "tree", "Sub", "Wrap.ilean")
	if err := os.WriteFile(wrap, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 3 {
		t.Fatalf("refresh with an unreadable module: %+v", r)
	}
	for _, args := range [][]string{{"users", "Frame"}, {"where", "Frame"}, {"hash", "Gold.Frame"}, {"states", "X"}} {
		if r := invoke(t, append([]string{"--config", config}, args...)...); r.code != 3 {
			t.Errorf("%v on an incomplete index: %+v", args, r)
		}
	}
}

// The ledger check writes only to an existing ledger: it never creates the ledger's schema.
func TestLedgerRefusesADatabaseWithoutTheLedgerSchema(t *testing.T) {
	dsn := testpg.Database(t, "declindex_noschema")
	config := fixture(t)
	body, _ := os.ReadFile(config)
	body = append(body, []byte("repo: r\ncontracts:\n  paths: [\"**/*.lean\"]\n  kinds: [def]\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger"}, func(k string) string {
		if k == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}, &out, &errOut)
	if code != 3 {
		t.Fatalf("ledger on an empty database exited %d: %s %s", code, out.String(), errOut.String())
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var tables int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("the ledger check created %d tables", tables)
	}
}

// A source edited after its build moves every declaration away from the position its reference file recorded:
// the landing hasher cannot answer for it.
func TestLandHashRefusesADeclarationWhoseSourceIsNewerThanItsBuild(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	src := filepath.Join(tree, "Golden.lean")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(src, append([]byte("-- edited after the build\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "one")
	invoke(t, "--config", config, "refresh")
	j, _ := json.Marshal([]string{"Gold.frame_lo"})
	env := map[string]string{"LAND_TREE": git(t, tree, "rev-parse", "HEAD^{tree}"), "LAND_NAMES": string(j)}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "land-hash"}, func(k string) string { return env[k] }, &out, &errOut)
	if code != 3 || out.Len() != 0 {
		t.Fatalf("land-hash of a stale declaration: exit %d, %q %q", code, out.String(), errOut.String())
	}
}
