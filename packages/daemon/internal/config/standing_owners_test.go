package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		owners.Repository != "example/repository" || owners.MaxRunning != 2 || owners.BudgetUSD != 12.5 ||
		owners.LedgerTimeout != 5*time.Second {
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

func TestStandingOwnersLedgerTimeoutIsConfigurable(t *testing.T) {
	body := strings.Replace(standingOwnersYAML("1", "1", "owner-a"), "  max_running: 1\n",
		"  max_running: 1\n  ledger_timeout_seconds: 2\n", 1)
	cfg, err := Load(writeConfigFile(t, minimalFile+body), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.StandingOwners.LedgerTimeout; got != 2*time.Second {
		t.Fatalf("LedgerTimeout = %s, want 2s", got)
	}
	bad := strings.Replace(body, "ledger_timeout_seconds: 2", "ledger_timeout_seconds: 0", 1)
	if _, err := Load(writeConfigFile(t, minimalFile+bad), noEnv); err == nil {
		t.Fatal("Load accepted a zero ledger timeout")
	}
}

func standingOwnersYAML(budget, cost, owner string) string {
	return fmt.Sprintf(`standing_owners:
  ledger_dsn_file: credentials/ledger-dsn
  repository: example/repository
  max_running: 1
  budget_usd: %s
  owners:
    - name: %s
      prompt_file: prompts/owner.md
      harness: omp
      max_cost_usd: %s
`, budget, owner, cost)
}

func TestStandingOwnersRefuseInvalidValuesBeforeDaemonBoot(t *testing.T) {
	for _, tc := range []struct {
		name, budget, cost, owner string
	}{
		{"a NaN budget", ".nan", "1", "owner-a"},
		{"an infinite budget", ".inf", "1", "owner-a"},
		{"a NaN maximum cost", "1", ".nan", "owner-a"},
		{"an infinite maximum cost", "1", ".inf", "owner-a"},
		{"an invalid owner token", "1", "1", "Owner_A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+standingOwnersYAML(tc.budget, tc.cost, tc.owner)), noEnv)
			if err == nil {
				t.Fatalf("Load accepted standing-owner configuration: %s", tc.name)
			}
		})
	}
}

func TestStandingOwnersAreTmuxOnly(t *testing.T) {
	body := kubernetesFile + standingOwnersYAML("1", "1", "owner-a")
	_, err := LoadForValidation(writeConfigFile(t, body), noEnv)
	if err == nil || !strings.Contains(err.Error(), "standing_owners") || !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("LoadForValidation = %v, want standing_owners tmux refusal", err)
	}
}
