package config

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// StandingOwner is one named standing owner (a librarian, a publisher): a ledger key of its own
// that the daemon starts only while its ledger queue has work.
type StandingOwner struct {
	Name       string
	PromptFile string
	Harness    runtime.Backend
	MaxCostUSD float64
}

// OwnerTemplate is how the daemon launches the owner of a graph node of one kind: its prompt, its
// harness, and the cost each attempt reserves.
type OwnerTemplate struct {
	PromptFile string
	Harness    runtime.Backend
	MaxCostUSD float64
}

// StandingOwners is the configuration for the daemon's standing-owner supervisor. Owners are the
// named owners; every other owner is a graph node the ledger names (an open item's owner, such as
// `function:get_mountpoint`), launched by the template of its kind — the part of its id before the
// colon. NeverStart are owners the daemon never starts, whose items stay open for `owner status`
// (an escalation queue a person reads). LedgerTimeout bounds each ledger call owner admission
// makes: the connection, its schema check and each read. Reconcile is how often the safety sweep
// reads every open owner, beside the wake each ledger event sends.
type StandingOwners struct {
	LedgerDSNFile string
	Repository    string
	MaxRunning    int
	BudgetUSD     float64
	LedgerTimeout time.Duration
	Reconcile     time.Duration
	Owners        []StandingOwner
	Templates     map[string]OwnerTemplate
	NeverStart    []string
}

// TemplateFor is the template of a graph node id's kind, the part before its first colon.
func (o StandingOwners) TemplateFor(node string) (OwnerTemplate, bool) {
	kind, _, isNode := strings.Cut(node, ":")
	if !isNode {
		return OwnerTemplate{}, false
	}
	template, ok := o.Templates[kind]
	return template, ok
}

const (
	// defaultLedgerTimeout bounds a ledger call when standing_owners.ledger_timeout_seconds is unset.
	defaultLedgerTimeout = 5 * time.Second
	// defaultReconcile is the safety sweep's period when standing_owners.reconcile_seconds is unset.
	defaultReconcile = 5 * time.Minute
)

func readStandingOwners(value *yaml.Node, key string) (*StandingOwners, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	members, err := members(value, key, "ledger_dsn_file", "repository", "max_running", "budget_usd", "ledger_timeout_seconds",
		"reconcile_seconds", "owners", "templates", "never_start")
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
	timeout, err := readSeconds(members, key, "ledger_timeout_seconds", defaultLedgerTimeout)
	if err != nil {
		return nil, err
	}
	reconcile, err := readSeconds(members, key, "reconcile_seconds", defaultReconcile)
	if err != nil {
		return nil, err
	}
	var owners []StandingOwner
	if ownersNode, ok := members["owners"]; ok {
		if ownersNode.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%s.owners must be an array", key)
		}
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
	}
	templates := map[string]OwnerTemplate{}
	if templatesNode, ok := members["templates"]; ok {
		if templatesNode.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s.templates must be a mapping of node kinds", key)
		}
		for i := 0; i+1 < len(templatesNode.Content); i += 2 {
			kind := templatesNode.Content[i].Value
			if !slices.Contains(claim.NodeKinds, kind) {
				return nil, fmt.Errorf("%s.templates: %q is not a node kind; a node id's kind is one of %s", key, kind, strings.Join(claim.NodeKinds, ", "))
			}
			template, err := readOwnerLaunch(templatesNode.Content[i+1], key+".templates."+kind, "prompt_file", "harness", "max_cost_usd")
			if err != nil {
				return nil, err
			}
			templates[kind] = OwnerTemplate(template)
		}
	}
	if len(owners) == 0 && len(templates) == 0 {
		return nil, fmt.Errorf("%s needs owners or templates: with neither, no owner can start", key)
	}
	neverStart, err := readNeverStart(members["never_start"], key+".never_start", owners)
	if err != nil {
		return nil, err
	}
	return &StandingOwners{LedgerDSNFile: ledgerDSNFile, Repository: repository, MaxRunning: *maxRunning, BudgetUSD: budget,
		LedgerTimeout: timeout, Reconcile: reconcile, Owners: owners, Templates: templates, NeverStart: neverStart}, nil
}

// readNeverStart reads the owners the daemon never starts: owner keys, each once, none an owner
// the same file names to start.
func readNeverStart(value *yaml.Node, key string, owners []StandingOwner) ([]string, error) {
	if value == nil || value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array of owner keys", key)
	}
	var keys []string
	for index, node := range value.Content {
		owner, err := requiredString(node, fmt.Sprintf("%s[%d]", key, index), "")
		if err != nil {
			return nil, err
		}
		switch {
		case !claim.IsOwnerKey(owner):
			return nil, fmt.Errorf("%s[%d] %q is not an owner key: a graph node id (%s, a colon, then no whitespace) or a named role", key, index, owner, strings.Join(claim.NodeKinds, "|"))
		case slices.Contains(keys, owner):
			return nil, fmt.Errorf("%s names %s twice", key, owner)
		case slices.ContainsFunc(owners, func(o StandingOwner) bool { return o.Name == owner }):
			return nil, fmt.Errorf("%s names %s, which owners also configures to start", key, owner)
		}
		keys = append(keys, owner)
	}
	return keys, nil
}

// readSeconds reads an optional positive count of seconds, fallback when absent.
func readSeconds(members map[string]*yaml.Node, key, name string, fallback time.Duration) (time.Duration, error) {
	node, ok := members[name]
	if !ok {
		return fallback, nil
	}
	seconds, err := readPositiveInteger(node, key+"."+name, 0)
	if err != nil {
		return 0, err
	}
	if seconds == nil {
		return 0, fmt.Errorf("%s.%s must be a positive integer", key, name)
	}
	return time.Duration(*seconds) * time.Second, nil
}

func readStandingOwner(value *yaml.Node, key string) (StandingOwner, error) {
	if value.Kind != yaml.MappingNode {
		return StandingOwner{}, fmt.Errorf("%s must be a mapping", key)
	}
	name, err := requiredString(memberOf(value, "name"), key+".name", "")
	if err != nil {
		return StandingOwner{}, err
	}
	if strings.Contains(name, ":") {
		return StandingOwner{}, fmt.Errorf("%s.name %q is shaped like a graph node id; a node's owner is configured by its kind's template", key, name)
	}
	if _, err := claim.OwnerToken("owner", name); err != nil {
		return StandingOwner{}, fmt.Errorf("%s.name: %w", key, err)
	}
	launch, err := readOwnerLaunch(value, key, "name", "prompt_file", "harness", "max_cost_usd")
	if err != nil {
		return StandingOwner{}, err
	}
	return StandingOwner{Name: name, PromptFile: launch.PromptFile, Harness: launch.Harness, MaxCostUSD: launch.MaxCostUSD}, nil
}

// readOwnerLaunch reads the launch every owner has, named or templated: its prompt file, a harness
// that runs standing owners, and a finite positive cost.
func readOwnerLaunch(value *yaml.Node, key string, allowed ...string) (OwnerTemplate, error) {
	if value.Kind != yaml.MappingNode {
		return OwnerTemplate{}, fmt.Errorf("%s must be a mapping", key)
	}
	members, err := members(value, key, allowed...)
	if err != nil {
		return OwnerTemplate{}, err
	}
	prompt, err := requiredString(members["prompt_file"], key+".prompt_file", "")
	if err != nil {
		return OwnerTemplate{}, err
	}
	harnessName, err := requiredString(members["harness"], key+".harness", "")
	if err != nil {
		return OwnerTemplate{}, err
	}
	harness := runtime.Backend(harnessName)
	info, err := runtime.LookupBackend(harness)
	if err != nil {
		return OwnerTemplate{}, fmt.Errorf("%s.harness: %w", key, err)
	}
	if !info.Runs("owner") {
		return OwnerTemplate{}, fmt.Errorf("%s.harness is %s (%s), which does not run standing owners", key, harness, info.Name)
	}
	cost, err := readCost(members["max_cost_usd"], key+".max_cost_usd")
	if err != nil {
		return OwnerTemplate{}, err
	}
	return OwnerTemplate{PromptFile: prompt, Harness: harness, MaxCostUSD: cost}, nil
}

// memberOf is the value of a mapping's key, nil when it has none.
func memberOf(value *yaml.Node, name string) *yaml.Node {
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == name {
			return value.Content[i+1]
		}
	}
	return nil
}

func readCost(value *yaml.Node, key string) (float64, error) {
	if value == nil || value.Tag == "!!null" || (value.Tag != "!!int" && value.Tag != "!!float") {
		return 0, fmt.Errorf("%s must be a positive number", key)
	}
	var cost float64
	if err := value.Decode(&cost); err != nil || cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, fmt.Errorf("%s must be a finite positive number", key)
	}
	return cost, nil
}
