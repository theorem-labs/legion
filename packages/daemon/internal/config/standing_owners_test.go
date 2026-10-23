package config

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestLoadReadsStandingOwners(t *testing.T) {
	path := writeConfigFile(t, minimalFile+`standing_owners:
  ledger_dsn_file: credentials/ledger-dsn
  repository: example/repository
  max_running: 2
  budget_usd: 12.5
  owners:
    - name: proof-a
      prompt_file: prompts/proof-a.md
      harness: omp
      max_cost_usd: 3.5
    - name: proof-b
      prompt_file: /srv/prompts/proof-b.md
      harness: omp
      max_cost_usd: 4
`)

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StandingOwners == nil {
		t.Fatal("StandingOwners = nil, want configured owners")
	}
	owners := cfg.StandingOwners
	if owners.LedgerDSNFile != filepath.Join(filepath.Dir(path), "credentials/ledger-dsn") ||
		owners.Repository != "example/repository" || owners.MaxRunning != 2 || owners.BudgetUSD != 12.5 {
		t.Fatalf("StandingOwners = %+v", owners)
	}
	if len(owners.Owners) != 2 {
		t.Fatalf("owners = %+v, want two", owners.Owners)
	}
	if got := owners.Owners[0]; got.Name != "proof-a" || got.PromptFile != filepath.Join(filepath.Dir(path), "prompts/proof-a.md") ||
		got.Harness != runtime.BackendOmp || got.MaxCostUSD != 3.5 {
		t.Errorf("first owner = %+v", got)
	}
	if got := owners.Owners[1]; got.Name != "proof-b" || got.PromptFile != "/srv/prompts/proof-b.md" ||
		got.Harness != runtime.BackendOmp || got.MaxCostUSD != 4 {
		t.Errorf("second owner = %+v", got)
	}
}
