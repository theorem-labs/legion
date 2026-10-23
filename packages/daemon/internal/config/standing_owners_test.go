package config

import (
	"fmt"
	"path/filepath"
	"slices"
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

// Node owners come from the ledger, so they are configured by node kind, not by name: one template
// per kind (the prefix before the colon of a node id), and the static list holds only named roles.
// The slow reconciliation sweep is configurable, five minutes by default.
func TestStandingOwnerTemplatesAreConfiguredPerNodeKind(t *testing.T) {
	path := writeConfigFile(t, minimalFile+`standing_owners:
  ledger_dsn_file: credentials/ledger-dsn
  repository: example/repository
  max_running: 4
  budget_usd: 100
  reconcile_seconds: 60
  templates:
    function:
      prompt_file: prompts/function.md
      harness: omp
      max_cost_usd: 2
    struct:
      prompt_file: /srv/prompts/struct.md
      harness: omp
      max_cost_usd: 3
`)
	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	owners := cfg.StandingOwners
	if owners == nil || len(owners.Owners) != 0 || owners.Reconcile != time.Minute || len(owners.Templates) != 2 {
		t.Fatalf("StandingOwners = %+v", owners)
	}
	if got := owners.Templates["function"]; got.PromptFile != filepath.Join(filepath.Dir(path), "prompts/function.md") ||
		got.Harness != runtime.BackendOmp || got.MaxCostUSD != 2 {
		t.Errorf("function template = %+v", got)
	}
	if got := owners.Templates["struct"]; got.PromptFile != "/srv/prompts/struct.md" || got.MaxCostUSD != 3 {
		t.Errorf("struct template = %+v", got)
	}
	def, err := Load(writeConfigFile(t, minimalFile+standingOwnersYAML("1", "1", "owner-a")), noEnv)
	if err != nil || def.StandingOwners.Reconcile != 5*time.Minute {
		t.Fatalf("default reconcile = %v, %v", def.StandingOwners, err)
	}
}

func TestStandingOwnerConfigurationRefusesWhatCannotRun(t *testing.T) {
	head := minimalFile + "standing_owners:\n  ledger_dsn_file: d\n  repository: r\n  max_running: 1\n  budget_usd: 1\n"
	template := func(kind string) string {
		return "  templates:\n    " + kind + ":\n      prompt_file: p\n      harness: omp\n      max_cost_usd: 1\n"
	}
	for name, body := range map[string]string{
		"neither owners nor templates":       head,
		"a template kind with a colon":       head + template(`"function:x"`),
		"an uppercase template kind":         head + template("Function"),
		"a kind no node id has":              head + template("file"),
		"a named owner shaped like a node":   head + "  owners:\n    - name: function:get_mountpoint\n      prompt_file: p\n      harness: omp\n      max_cost_usd: 1\n",
		"a zero reconciliation":              head + template("function") + "  reconcile_seconds: 0\n",
		"a named owner outside the alphabet": head + "  owners:\n    - name: Owner_A\n      prompt_file: p\n      harness: omp\n      max_cost_usd: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfigFile(t, body), noEnv); err == nil {
				t.Fatal("Load accepted it")
			}
		})
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

// never_start names owners whose items stay open for `owner status` and that the daemon never
// starts: named roles or node ids, each an owner key, each once, and none an owner it is also told
// to start.
func TestStandingOwnersNeverStartListsOwnerKeys(t *testing.T) {
	body := strings.Replace(standingOwnersYAML("1", "1", "owner-a"), "  max_running: 1\n",
		"  max_running: 1\n  never_start: [escalations, \"function:mnt_get_writers@fs/namespace.c\"]\n", 1)
	cfg, err := Load(writeConfigFile(t, minimalFile+body), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.StandingOwners.NeverStart; !slices.Equal(got, []string{"escalations", "function:mnt_get_writers@fs/namespace.c"}) {
		t.Fatalf("NeverStart = %q", got)
	}
	for name, list := range map[string]string{
		"a key outside the grammar": "[\"file:x\"]",
		"a key twice":               "[escalations, escalations]",
		"a configured owner":        "[owner-a]",
		"not a list":                "escalations",
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(body, "[escalations, \"function:mnt_get_writers@fs/namespace.c\"]", list, 1)
			if _, err := Load(writeConfigFile(t, minimalFile+bad), noEnv); err == nil || !strings.Contains(err.Error(), "never_start") {
				t.Fatalf("Load = %v, want a never_start refusal", err)
			}
		})
	}
}
