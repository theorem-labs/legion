package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerEmitsRegistrationsFromContractsFile(t *testing.T) {
	config := fixture(t)
	tree := filepath.Join(filepath.Dir(config), "tree")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("repo: r\nowners: owners.json\nowners_format: cgraph/owners/v1\ncontracts:\n  paths: [\"Sub/**\"]\n  kinds: [def]\n  name: \"^[A-Z]\"\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	owners := `{"format":"cgraph/owners/v1","owners":[{"id":"function:frame_at","contract":"Sub"}]}`
	if err := os.WriteFile(filepath.Join(tree, "owners.json"), []byte(owners), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "one")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	contracts := filepath.Join(filepath.Dir(config), "contracts.json")
	if err := os.WriteFile(contracts, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emit := filepath.Join(filepath.Dir(config), "operations.jsonl")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger", "--contracts-file", contracts, "--emit", emit}, func(k string) string {
		if k == "LEDGER_DSN" {
			return "postgres://127.0.0.1:1/unused"
		}
		return ""
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("ledger emit: code %d, stdout %s, stderr %s", code, out.String(), errOut.String())
	}
	var report struct {
		Commit    string `json:"commit"`
		Contracts struct {
			Registered []string `json:"registered"`
		} `json:"contracts"`
		Registry *json.RawMessage `json:"registry"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Commit == "" || len(report.Contracts.Registered) != 1 || report.Contracts.Registered[0] != "Gold2.FrameAt" || report.Registry != nil {
		t.Fatalf("ledger report %+v", report)
	}
	var operation struct {
		Op            string `json:"op"`
		Name          string `json:"name"`
		Owner         string `json:"owner"`
		StatementHash string `json:"statement_hash"`
		VersionRead   int64  `json:"version_read"`
	}
	raw, err := os.ReadFile(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Op != "register" || operation.Name != "Gold2.FrameAt" || operation.Owner != "function:frame_at" || operation.StatementHash == "" || operation.VersionRead != 0 {
		t.Fatalf("operation %+v", operation)
	}
}

func TestLedgerClassifiesConnectionFailureAsTemporary(t *testing.T) {
	config := fixture(t)
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("repo: r\ncontracts:\n  paths: [\"Sub/**\"]\n  kinds: [def]\n")...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger"}, func(k string) string {
		if k == "LEDGER_DSN" {
			return "postgres://127.0.0.1:1/ledger"
		}
		return ""
	}, &out, &errOut)
	if code != 75 {
		t.Fatalf("ledger on an unreachable database exited %d, want 75: stdout %s stderr %s", code, out.String(), errOut.String())
	}
}

func TestLedgerRequiresADatabaseSource(t *testing.T) {
	config := fixture(t)
	if r := invoke(t, "--config", config, "ledger"); r.code != 2 {
		t.Fatalf("ledger without a DSN or contracts file: %+v", r)
	}
}
