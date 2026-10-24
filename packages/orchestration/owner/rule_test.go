package owner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func loadRuleFixture(t *testing.T) Rule {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "RULE.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rule, err := ParseRule(data)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestRuleOrdersStoppedInstructionsThenCallHeight(t *testing.T) {
	items := []PrioritizedItem{
		{ID: "10/0", StoppedInstructions: 10, CallHeight: 0, EnqueuedAt: time.Date(2026, 10, 23, 12, 1, 0, 0, time.UTC)},
		{ID: "50/2", StoppedInstructions: 50, CallHeight: 2, EnqueuedAt: time.Date(2026, 10, 23, 12, 2, 0, 0, time.UTC)},
		{ID: "50/1", StoppedInstructions: 50, CallHeight: 1, EnqueuedAt: time.Date(2026, 10, 23, 12, 3, 0, 0, time.UTC)},
	}

	loadRuleFixture(t).Sort(items)
	got := []string{items[0].ID, items[1].ID, items[2].ID}
	if want := []string{"50/1", "50/2", "10/0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("work order = %v, want %v", got, want)
	}
}

func TestParseRuleRefusesUnknownKeyAndDirection(t *testing.T) {
	for _, data := range []string{
		"version: 1\norder:\n  - {key: popularity, direction: descending}\n",
		"version: 1\norder:\n  - {key: call_height, direction: sideways}\n",
		"version: 1\norder: []\n",
	} {
		if _, err := ParseRule([]byte(data)); err == nil {
			t.Errorf("ParseRule(%q) accepted an invalid rule", data)
		}
	}
}

func TestRulePrioritiesFollowRuleDefinitions(t *testing.T) {
	fixture := loadRoutingFixture(t)
	// skeleton.node_instructions is keyed by node id (the plan's score schema, P6): each node's Lean directory's
	// image instructions.
	inputs := RuleInputs{
		Index:      fixture.index,
		Owners:     fixture.owners,
		Active:     map[string]bool{"function:caller": true, "function:second@kernel/second.c": true, "function:callee": true, "struct:state": true, "subsystem:cluster": true},
		FactUsers:  map[string][]string{"fact-stack-alignment": fixture.facts["stack-alignment"].Users},
		NodeCounts: map[string]int{"function:caller": 7, "function:second@kernel/second.c": 3, "function:callee": 5},
	}
	for _, tc := range []struct {
		item, owner string
		want        Priority
	}{
		{"need:function:caller->struct:state@r1", "struct:state", Priority{StoppedInstructions: 7, CallHeight: 1}},
		{"start:function:callee@1", "function:callee", Priority{StoppedInstructions: 15}},
		{"start:struct:state@1", "struct:state", Priority{StoppedInstructions: 10}},
		{"start:subsystem:cluster@1", "subsystem:cluster", Priority{StoppedInstructions: 10}},
		{"start:fact-stack-alignment@1", "fact-stack-alignment", Priority{StoppedInstructions: 7}},
		{"upstream-review:0123456789ab:spec:struct:state", "struct:state", Priority{StoppedInstructions: 15}},
	} {
		got, err := inputs.Priority(ledger.QueuedItem{Item: ledger.Item{ID: tc.item, Owner: tc.owner}})
		if err != nil {
			t.Fatalf("Priority(%s): %v", tc.item, err)
		}
		if got != tc.want {
			t.Errorf("Priority(%s) = %+v, want %+v", tc.item, got, tc.want)
		}
	}
}

func TestRulePrioritiesRefuseInterfaceItemsWithoutMeasures(t *testing.T) {
	fixture := loadRoutingFixture(t)
	inputs := RuleInputs{Index: fixture.index, Owners: fixture.owners}
	_, err := inputs.Priority(ledger.QueuedItem{Item: ledger.Item{ID: "interface:" + Hex16("StateIf") + "@1", Owner: "struct:state"}})
	if err == nil || !strings.Contains(err.Error(), "measures") {
		t.Fatalf("Priority without measures error = %v", err)
	}
}
