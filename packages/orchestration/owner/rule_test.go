package owner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
		{ID: "10/0", StoppedInstructions: 10, CallHeight: 0, Position: 0},
		{ID: "50/2", StoppedInstructions: 50, CallHeight: 2, Position: 1},
		{ID: "50/1", StoppedInstructions: 50, CallHeight: 1, Position: 2},
	}

	loadRuleFixture(t).Sort(items)
	got := []string{items[0].ID, items[1].ID, items[2].ID}
	if want := []string{"50/1", "50/2", "10/0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("work order = %v, want %v", got, want)
	}
}

// The rule's keys decide the order. Items they cannot tell apart keep the order the ledger lists them in, which is
// the order it enqueued them (Store.OpenItems), and not the order of their ids.
func TestRuleKeepsTheLedgerOrderOfItemsItsKeysTie(t *testing.T) {
	items := []PrioritizedItem{{ID: "start:struct:z@1"}, {ID: "start:struct:a@1"}, {ID: "start:struct:m@1"}}

	loadRuleFixture(t).Sort(items)
	got := []string{items[0].ID, items[1].ID, items[2].ID}
	if want := []string{"start:struct:z@1", "start:struct:a@1", "start:struct:m@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("work order = %v, want the ledger's order %v", got, want)
	}
}

// enqueued_at orders by the ledger's enqueue order, which is each item's position in Store.OpenItems' list: a rule that
// takes the newest first reverses it.
func TestRuleEnqueuedAtIsTheLedgerOrder(t *testing.T) {
	rule, err := ParseRule([]byte("version: 1\norder:\n  - {key: enqueued_at, direction: descending}\n"))
	if err != nil {
		t.Fatal(err)
	}
	items := []PrioritizedItem{{ID: "first", Position: 0}, {ID: "second", Position: 1}, {ID: "third", Position: 2}}

	rule.Sort(items)
	if got := []string{items[0].ID, items[1].ID, items[2].ID}; !reflect.DeepEqual(got, []string{"third", "second", "first"}) {
		t.Fatalf("newest-first order = %v, want third, second, first", got)
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
		got, err := inputs.Priority(ledger.Item{ID: tc.item, Owner: tc.owner})
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
	_, err := inputs.Priority(ledger.Item{ID: "interface:" + Hex16("StateIf") + "@1", Owner: "struct:state"})
	if err == nil || !strings.Contains(err.Error(), "measures") {
		t.Fatalf("Priority without measures error = %v", err)
	}
}
