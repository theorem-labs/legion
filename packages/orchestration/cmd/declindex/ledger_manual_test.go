package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func manualLedgerFixture(t *testing.T, tail, owners string) (config, dsn, tree string, l *ledger.Store) {
	t.Helper()
	config = fixture(t)
	tree = filepath.Join(filepath.Dir(config), "tree")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("repo: r\n"+tail)...)
	if err := os.WriteFile(config, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if owners != "" {
		if err := os.WriteFile(filepath.Join(tree, "owners.json"), []byte(owners), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, tree, "init", "-q")
	git(t, tree, "add", "-A")
	git(t, tree, "commit", "-q", "-m", "one")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("refresh: %+v", r)
	}
	dsn = testpg.Database(t, "declindex_ledger_command")
	l, err = ledger.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if _, err := l.EnsureRepo(context.Background(), "r", 3); err != nil {
		t.Fatal(err)
	}
	return config, dsn, tree, l
}

func invokeLedger(t *testing.T, config, dsn string, landCommit string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--config", config, "ledger"}, func(k string) string {
		switch k {
		case "LEDGER_DSN":
			return dsn
		case "LAND_COMMIT":
			return landCommit
		default:
			return ""
		}
	}, &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func declarationForLedger(t *testing.T, config, name string) declindex.Found {
	t.Helper()
	cfg, err := declindex.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := declindex.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	found, err := s.Resolve(context.Background(), name)
	if err != nil || len(found) != 1 {
		t.Fatalf("resolve %s: %+v %v", name, found, err)
	}
	return found[0]
}

const subContractRule = "owners: owners.json\nowners_format: cgraph/owners/v1\ncontracts:\n  paths: [\"Sub/**\"]\n  kinds: [def]\n"

const subOwnerMap = `{"format":"cgraph/owners/v1","owners":[{"id":"function:frame_at","contract":"Sub"}]}`

func TestLedgerManualContractResults(t *testing.T) {
	t.Run("registers a clean selected contract", func(t *testing.T) {
		config, dsn, _, _ := manualLedgerFixture(t, subContractRule, subOwnerMap)
		if r := invokeLedger(t, config, dsn, ""); r.code != 0 || !strings.Contains(r.out, `"registered": [`) {
			t.Fatalf("clean ledger: %+v", r)
		}
	})
	t.Run("reports unowned selected contracts", func(t *testing.T) {
		tail := "contracts:\n  paths: [\"Sub/**\"]\n  kinds: [def]\n"
		config, dsn, _, _ := manualLedgerFixture(t, tail, "")
		if r := invokeLedger(t, config, dsn, ""); r.code != 1 || !strings.Contains(r.out, `"unowned": [`) {
			t.Fatalf("unowned ledger: %+v", r)
		}
	})
	t.Run("reports rule ambiguity", func(t *testing.T) {
		config, dsn, _, _ := manualLedgerFixture(t, subContractRule, subOwnerMap)
		cfg, err := declindex.LoadConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", cfg.DB)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO decls (name, short, module, kind, line, stmt_hash)
			SELECT name, short, module, kind, line, stmt_hash FROM decls WHERE name = 'Gold2.FrameAt'`); err != nil {
			t.Fatal(err)
		}
		if r := invokeLedger(t, config, dsn, ""); r.code != 1 || !strings.Contains(r.out, `"ambiguous": [`) {
			t.Fatalf("ambiguous ledger: %+v", r)
		}
	})
	for _, test := range []struct {
		name, contract, hash string
		code                 int
	}{
		{"matches a ledger contract outside the rule", "Gold2.FrameAt", "", 0},
		{"reports a different ledger hash", "Gold.Box", "different", 1},
		{"reports a gone ledger contract", "Gold.Removed", "missing", 1},
		{"skips a metadata ledger contract", "spec:function:open_file", "metadata", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			tail := "contracts:\n  paths: [\"NoContracts/**\"]\n  kinds: [def]\n"
			config, dsn, _, l := manualLedgerFixture(t, tail, "")
			hash := test.hash
			if test.contract == "Gold2.FrameAt" || test.contract == "Gold.Box" {
				found := declarationForLedger(t, config, test.contract)
				if hash == "" {
					hash = found.StmtHash
				}
			}
			if _, err := l.RegisterContract(context.Background(), "r", test.contract, "function:frame_at", hash, false); err != nil {
				t.Fatal(err)
			}
			if r := invokeLedger(t, config, dsn, ""); r.code != test.code {
				t.Fatalf("ledger: %+v", r)
			}
		})
	}
	t.Run("refuses a landing commit that does not cover the index", func(t *testing.T) {
		config, dsn, tree, _ := manualLedgerFixture(t, subContractRule, subOwnerMap)
		first := git(t, tree, "rev-parse", "HEAD")
		git(t, tree, "checkout", "-q", "-b", "other")
		git(t, tree, "commit", "--allow-empty", "-q", "-m", "other")
		other := git(t, tree, "rev-parse", "HEAD")
		git(t, tree, "checkout", "-q", "--detach", first)
		if r := invokeLedger(t, config, dsn, other); r.code != 3 {
			t.Fatalf("ledger with unrelated LAND_COMMIT: %+v", r)
		}
	})
}
