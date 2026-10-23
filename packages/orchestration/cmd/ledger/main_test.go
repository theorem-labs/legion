package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

func TestHelpStartsWithOneLineUsage(t *testing.T) {
	var out, err bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, func(string) string { return "" }, &out, &err); code != 0 {
		t.Fatalf("help exit code %d: %s", code, err.String())
	}
	if first := strings.SplitN(out.String(), "\n", 2)[0]; !strings.HasPrefix(first, "usage: ledger ") {
		t.Fatalf("help first line %q", first)
	}
}

func TestEveryLedgerSubcommandRoundTrips(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, err bytes.Buffer
		if code := run(context.Background(), args, getenv, &out, &err); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, err.String())
		}
		return out.String()
	}

	invoke("repo", "ensure", "r", "3")
	invoke("claim", "r", "item", "alice", "1m")
	invoke("renew", "r", "item", "alice", "1", "1m")
	invoke("release", "r", "item", "alice", "1")
	invoke("claim", "r", "done", "alice", "1m")
	invoke("complete", "r", "done", "alice", "1", "result")
	invoke("expire", "r")
	item := invoke("--json", "item", "r", "done")
	if !json.Valid([]byte(item)) {
		t.Fatalf("item JSON %q", item)
	}

	invoke("open", "r", "M-1", "alice", "base", "branch", "a.lean")
	invoke("extend", "r", "M-1", "1", "b.lean")
	invoke("check", "r", "M-1", "1", "a.lean,b.lean")
	invoke("takeover", "r", "M-1", "bob", "1")
	invoke("round", "r", "M-1", "2", "failed")
	invoke("abandon", "r", "M-1", "2", "stopped")

	invoke("contract", "register", "r", "C", "alice", "hash-1", "false")
	contract := invoke("--json", "contract", "show", "r", "C")
	if !json.Valid([]byte(contract)) {
		t.Fatalf("contract JSON %q", contract)
	}
	invoke("contract", "transfer", "r", "C", "alice", "bob")
	invoke("agree", "r", "A-1", "bob", "C=1")
	invoke("open", "r", "M-2", "bob", "base", "branch", "c.lean", "C=hash-1")
	invoke("land", "r", "M-2", "1", "commit-1", "C=hash-2")
	stale := invoke("--json", "stale", "r")
	if !json.Valid([]byte(stale)) {
		t.Fatalf("stale JSON %q", stale)
	}
	status := invoke("--json", "status", "r")
	if !json.Valid([]byte(status)) {
		t.Fatalf("status JSON %q", status)
	}
	events := invoke("--json", "events", "r", "0")
	if !json.Valid([]byte(events)) {
		t.Fatalf("events JSON %q", events)
	}
}

func TestCommandExitCodes(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		return run(context.Background(), args, getenv, &out, &err), out.String()
	}
	if code, _ := call("claim"); code != 2 {
		t.Fatalf("usage exit code %d, want 2", code)
	}
	if code, _ := call("item", "r", "missing"); code != 3 {
		t.Fatalf("not found exit code %d, want 3", code)
	}
	if code, _ := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure exit code %d", code)
	}
	if code, _ := call("contract", "register", "r", "C", "alice", "hash-1", "false"); code != 0 {
		t.Fatalf("register exit code %d", code)
	}
	code, out := call("--json", "contract", "register", "r", "C", "alice", "hash-1", "false")
	if code != 1 || !strings.Contains(out, `"reason":"exists"`) {
		t.Fatalf("refusal code %d output %q", code, out)
	}
	file := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(file, []byte("postgres://invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--dsn-file", file, "repo", "ensure", "r", "0"}, getenv, &stdout, &stderr); code != 4 {
		t.Fatalf("database exit code %d, want 4", code)
	}
}
