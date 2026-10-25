package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// writeConfigFile writes cfg as the JSON configuration file that the program reads and returns its path.
func writeConfigFile(t *testing.T, cfg config) string {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, string(body))
	return path
}

// land serve refuses to start without an escalations owner, because an escalation enqueued for an empty owner
// wakes nobody. It is refused before the ledger is opened or anything runs.
func TestServeRefusesAnEmptyEscalationsOwner(t *testing.T) {
	for _, owner := range []string{"", "  "} {
		cfg := publicationConfig(t)
		cfg.Escalations.Owner = owner
		var output bytes.Buffer
		err := run(context.Background(), []string{"serve", "--config", writeConfigFile(t, cfg)}, &output)
		if err == nil || !strings.Contains(err.Error(), "escalations.owner") {
			t.Fatalf("serve with escalations.owner %q: error = %v, want it refused at start", owner, err)
		}
		if output.Len() != 0 {
			t.Fatalf("serve with an empty owner wrote %q, want nothing", output.String())
		}
	}
}

// The configuration's escalations.owner and publishing.retry_limit are decoded and reach the runner.
func TestConfigurationFillsEscalateToAndRetryLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"escalations": {"owner": "landing-operator"}, "publishing": {"retry_limit": 7}, "ledger": {"dsn_file": "/run/ledger.dsn"}}`)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalations.Owner != "landing-operator" || cfg.Publishing.RetryLimit != 7 || cfg.Ledger.DSNFile != "/run/ledger.dsn" {
		t.Fatalf("decoded %+v / %+v / %+v", cfg.Escalations, cfg.Publishing, cfg.Ledger)
	}
	full := withLedger(t, publicationConfig(t))
	full.Publishing.RetryLimit = 7
	built, err := buildProgram(t, full)
	if err != nil {
		t.Fatalf("program() error = %v", err)
	}
	if built.runner.EscalateTo != "landing-operator" || built.runner.PublishingRetryLimit != 7 {
		t.Fatalf("runner escalations owner %q, retry limit %d", built.runner.EscalateTo, built.runner.PublishingRetryLimit)
	}
}

// The program keeps its batches in the ledger, so a configuration without ledger.dsn_file is refused, with or without
// GitHub.
func TestConfigurationWithoutTheLedgerIsRefused(t *testing.T) {
	for _, publishing := range []bool{true, false} {
		cfg := publicationConfig(t)
		if !publishing {
			cfg.GitHub = githubConfig{}
		}
		cfg.Ledger.DSNFile = ""
		if _, err := buildProgram(t, cfg); err == nil || !strings.Contains(err.Error(), "ledger.dsn_file is required") {
			t.Fatalf("program() with publishing=%v error = %v, want the missing ledger refused", publishing, err)
		}
	}
}

// The program opens the ledger on ledger.dsn_file and keeps its batches there: the batch store, the member calls and
// the batch calls are all the ledger's, and a batch the program creates is the ledger's record, which the status
// command lists.
func TestLedgerDSNFileOpensTheLedgerBackedStore(t *testing.T) {
	ctx := context.Background()
	cfg := withLedger(t, publicationConfig(t))
	cfg.GitHub = githubConfig{}
	built, err := buildProgram(t, cfg)
	if err != nil {
		t.Fatalf("program() error = %v", err)
	}
	if _, ok := built.runner.Store.(*ledgerstore.Batches); !ok {
		t.Fatalf("batch store = %T, want *ledgerstore.Batches", built.runner.Store)
	}
	if _, ok := built.runner.Admission.(*ledgerstore.Ledger); !ok {
		t.Fatalf("batch ledger = %T, want *ledgerstore.Ledger", built.runner.Admission)
	}
	if _, ok := built.runner.Ledger.(*ledgerstore.Ledger); !ok {
		t.Fatalf("member ledger = %T, want *ledgerstore.Ledger", built.runner.Ledger)
	}
	body, err := os.ReadFile(cfg.Ledger.DSNFile)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := ledger.Open(ctx, strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if _, err := direct.EnsureRepo(ctx, "owner/repo", 3); err != nil {
		t.Fatal(err)
	}

	created := landing.Batch{ID: "batch-1", Repository: "owner/repo", Base: "base", BaseBranch: "trunk", State: landing.Frozen}
	if _, err := built.runner.Store.Create(ctx, &created); err != nil {
		t.Fatalf("create through the configured store: %v", err)
	}
	stored, err := direct.LoadBatch(ctx, "owner/repo", "batch-1")
	if err != nil || stored.Phase != "frozen" || stored.RecordVersion != 1 {
		t.Fatalf("the ledger holds %+v (%v), want batch-1 frozen at version 1", stored, err)
	}

	var output bytes.Buffer
	if err := run(ctx, []string{"status", "--config", writeConfigFile(t, cfg)}, &output); err != nil {
		t.Fatalf("status error = %v", err)
	}
	if !strings.Contains(output.String(), `"id":"batch-1"`) {
		t.Fatalf("status output = %q, want the ledger's batch-1", output.String())
	}
}
