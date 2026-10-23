package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// StandingOwner is one durable, non-workflow owner the daemon starts only while its ledger queue has work.
type StandingOwner struct {
	Name       string
	PromptFile string
	Harness    runtime.Backend
	MaxCostUSD float64
}

// StandingOwners is the configuration for the daemon's standing-owner supervisor.
type StandingOwners struct {
	LedgerDSNFile string
	Repository    string
	MaxRunning    int
	BudgetUSD     float64
	Owners        []StandingOwner
}

func readStandingOwners(value *yaml.Node, key string) (*StandingOwners, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	members, err := members(value, key, "ledger_dsn_file", "repository", "max_running", "budget_usd", "owners")
	if err != nil {
		return nil, err
	}
	readRequired := func(name string) (string, error) {
		return requiredString(members[name], key+"."+name, "")
	}
	ledgerDSNFile, err := readRequired("ledger_dsn_file")
	if err != nil {
		return nil, err
	}
	repository, err := readRequired("repository")
	if err != nil {
		return nil, err
	}
	maxRunning, err := readPositiveInteger(members["max_running"], key+".max_running", 0)
	if err != nil || maxRunning == nil {
		if err == nil {
			err = fmt.Errorf("%s.max_running is required", key)
		}
		return nil, err
	}
	budget, err := readCost(members["budget_usd"], key+".budget_usd")
	if err != nil {
		return nil, err
	}
	ownersNode, ok := members["owners"]
	if !ok || ownersNode.Kind != yaml.SequenceNode || len(ownersNode.Content) == 0 {
		return nil, fmt.Errorf("%s.owners must be a non-empty array", key)
	}
	owners := make([]StandingOwner, 0, len(ownersNode.Content))
	seen := map[string]bool{}
	for index, node := range ownersNode.Content {
		owner, err := readStandingOwner(node, fmt.Sprintf("%s.owners[%d]", key, index))
		if err != nil {
			return nil, err
		}
		if seen[owner.Name] {
			return nil, fmt.Errorf("%s names owner %s twice", key, owner.Name)
		}
		seen[owner.Name] = true
		owners = append(owners, owner)
	}
	return &StandingOwners{LedgerDSNFile: ledgerDSNFile, Repository: repository, MaxRunning: *maxRunning, BudgetUSD: budget, Owners: owners}, nil
}

func readStandingOwner(value *yaml.Node, key string) (StandingOwner, error) {
	if value.Kind != yaml.MappingNode {
		return StandingOwner{}, fmt.Errorf("%s must be a mapping", key)
	}
	members, err := members(value, key, "name", "prompt_file", "harness", "max_cost_usd")
	if err != nil {
		return StandingOwner{}, err
	}
	name, err := requiredString(members["name"], key+".name", "")
	if err != nil {
		return StandingOwner{}, err
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return StandingOwner{}, fmt.Errorf("%s.name must not contain whitespace", key)
	}
	prompt, err := requiredString(members["prompt_file"], key+".prompt_file", "")
	if err != nil {
		return StandingOwner{}, err
	}
	harnessName, err := requiredString(members["harness"], key+".harness", "")
	if err != nil {
		return StandingOwner{}, err
	}
	harness := runtime.Backend(harnessName)
	info, err := runtime.LookupBackend(harness)
	if err != nil {
		return StandingOwner{}, fmt.Errorf("%s.harness: %w", key, err)
	}
	if !info.Runs("owner") {
		return StandingOwner{}, fmt.Errorf("%s.harness is %s (%s), which does not run standing owners", key, harness, info.Name)
	}
	cost, err := readCost(members["max_cost_usd"], key+".max_cost_usd")
	if err != nil {
		return StandingOwner{}, err
	}
	return StandingOwner{Name: name, PromptFile: prompt, Harness: harness, MaxCostUSD: cost}, nil
}

func readCost(value *yaml.Node, key string) (float64, error) {
	if value == nil || value.Tag == "!!null" || (value.Tag != "!!int" && value.Tag != "!!float") {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	var cost float64
	if err := value.Decode(&cost); err != nil || cost <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	return cost, nil
}
