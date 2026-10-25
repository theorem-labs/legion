package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/conceptgate"
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

// The gate compiles a proposal with the rights of the user who runs it, after Lean's parser allow-list, on any
// machine where Lean runs: it refuses an exact duplicate, sends a near match to the librarian and adds a new concept,
// and leaves the tree clean.
func TestTheGateDecidesWithTheRightsOfItsCaller(t *testing.T) {
	dir, tree, config := leanGateFixture(t, map[string]string{
		"Fx.SpAligned.yaml": "name: Fx.SpAligned\nhome: Fixture.lean\nmeaning: Register x2 of a register file is 8-aligned.\n" +
			"forms:\n  - \"fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64\"\n",
	})
	invocations := filepath.Join(dir, "librarian-invocations")
	librarian := filepath.Join(dir, "record-librarian")
	writeTestFile(t, librarian, "#!/bin/sh\nprintf '%s\\n' \"$2\" >> "+strconv.Quote(invocations)+"\n")
	if err := os.Chmod(librarian, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, config, string(body)+"librarian:\n  - "+librarian+"\n")
	decide := func(id, name, source string) (result, conceptgate.Verdict) {
		t.Helper()
		raw, err := json.Marshal(conceptgate.Proposal{
			ID:      id,
			Concept: conceptgate.Concept{Name: name, Home: "New.lean", Meaning: "A proposed concept."},
			Source:  "import Fixture\n\n" + source + "\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".json")
		writeTestFile(t, path, string(raw))
		r := invoke(t, "--config", config, "gate", "--proposer", "role", path)
		var v conceptgate.Verdict
		if r.out != "" {
			if err := json.Unmarshal([]byte(r.out), &v); err != nil {
				t.Fatalf("%s: %v: %+v", id, err, r)
			}
		}
		return r, v
	}
	if r, v := decide("copy", "MySp", "def MySp (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"); r.code != 1 || v.Existing == "" {
		t.Fatalf("an exact duplicate: want exit 1 naming the declaration it copies, got %+v", r)
	}
	r, v := decide("near", "MyFrame", "def MyFrame (R : BitVec 5 → BitVec 64) (n : Nat) : Prop := n < 3 ∧ R 2#5 &&& 7#64 = 0#64")
	if r.code != 4 {
		t.Fatalf("a near match: want exit 4, got %+v", r)
	}
	if got, _ := os.ReadFile(invocations); strings.TrimSpace(string(got)) != v.VerdictID {
		t.Fatalf("the librarian received %q, want the one near verdict %s", got, v.VerdictID)
	}
	if r, v := decide("fresh", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1"); r.code != 0 || v.Addition == nil {
		t.Fatalf("a new concept: want exit 0 with an addition, got %+v", r)
	}
	if status := git(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("the gate dirtied the tree: %q", status)
	}
}
