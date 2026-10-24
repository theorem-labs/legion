package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/declindex/confine"
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

// OpenExisting refuses a database that lacks the ledger schema without creating it.
func TestLedgerRefusesADatabaseWithoutTheLedgerSchema(t *testing.T) {
	dsn := testpg.Database(t, "declindex_noschema")
	config := fixture(t)
	body, _ := os.ReadFile(config)
	body = append(body, []byte("repo: r\ncontracts:\n  paths: [\"**/*.lean\"]\n  kinds: [def]\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	// The ledger compares the index's commit with the tree's, so the tree is a Git work tree.
	tree := filepath.Join(filepath.Dir(config), "tree")
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "one")
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
	if code != 3 || !strings.Contains(errOut.String(), "ledger schema is missing versions") {
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

// A module whose source moved since its build makes the index incomplete: refresh, queries and the ledger check
// are unable to check, so the ledger never calls a live contract gone.
func TestAModuleNewerThanItsBuildStopsQueriesAndTheLedger(t *testing.T) {
	config := fixture(t)
	body, _ := os.ReadFile(config)
	body = append(body, []byte("repo: r\ncontracts:\n  paths: [\"**/*.lean\"]\n  kinds: [def]\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(filepath.Dir(config), "tree", "Golden.lean")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(src, append([]byte("-- edited after the build\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 3 || !strings.Contains(r.errOut, "newer than its build") {
		t.Fatalf("refresh: %+v", r)
	}
	if r := invoke(t, "--config", config, "where", "FrameAt"); r.code != 3 {
		t.Fatalf("where: %+v", r)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger"}, func(k string) string {
		if k == "LEDGER_DSN" {
			return "postgres://nobody@127.0.0.1:1/none"
		}
		return ""
	}, &out, &errOut)
	if code != 3 || out.Len() != 0 {
		t.Fatalf("ledger: exit %d, %q %q", code, out.String(), errOut.String())
	}
}

// On a machine whose sandbox is unavailable (here Landlock ABI 2, which does not govern truncation), the gate is
// unable to decide before it starts anything: no Lean process, no exporter build, even with nothing cached.
func TestGateStartsNoLeanWithoutASandbox(t *testing.T) {
	restore := confine.SetLandlockABIForTest(2, nil)
	defer restore()
	config := fixture(t)
	dir := filepath.Dir(config)
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	registry := "version: 1\nconcepts:\n  - name: Gold.Aligned\n    home: Golden.lean\n    meaning: A word is 8-aligned.\n"
	if err := os.WriteFile(filepath.Join(dir, "tree", "registry.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(config)
	body = append(body, []byte("registry: registry.yaml\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(dir, "proposal.json")
	p := `{"id":"p1","concept":{"name":"Fresh","home":"New.lean","meaning":"A fresh concept."},` +
		`"source":"def Fresh (x : Nat) : Prop := x < 3\n"}`
	if err := os.WriteFile(proposal, []byte(p), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fake toolchain programs on PATH record any start.
	bin, marker := filepath.Join(dir, "fakebin"), filepath.Join(dir, "started")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lean", "lake", "elan"} {
		script := "#!/bin/sh\necho " + name + " >> " + marker + "\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := invoke(t, "--config", config, "gate", "--proposer", "role", proposal)
	if r.code != 3 || !strings.Contains(r.errOut, "sandbox") {
		t.Fatalf("gate without a sandbox: %+v", r)
	}
	if started, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the gate started %q before it found the sandbox unavailable", strings.Fields(string(started)))
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "exporter")); err == nil {
		t.Fatal("the gate began an exporter build before it found the sandbox unavailable")
	}
}
