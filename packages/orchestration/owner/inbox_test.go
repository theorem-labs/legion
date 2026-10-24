package owner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestBuildInboxOrdersItemsAndNamesTheirRecords(t *testing.T) {
	need := "need:function:caller->struct:state@r1"
	offer := "offer:function:caller->struct:state@r1"
	items := []ledger.Item{{ID: need, Owner: "struct:state"}, {ID: offer, Owner: "function:caller"}}
	priorities := map[string]Priority{
		need:  {StoppedInstructions: 10, CallHeight: 0},
		offer: {StoppedInstructions: 50, CallHeight: 1},
	}

	inbox, err := BuildInbox(items, Config{RecordsRoot: "skeleton"}, priorities, loadRuleFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{inbox[0].ID, inbox[0].Record, inbox[0].RecordOwner, inbox[1].ID, inbox[1].Record, inbox[1].RecordOwner}
	want := []string{
		offer, "skeleton/nodes/" + Key("struct:state") + "/spec.md", "struct:state",
		need, "skeleton/nodes/" + Key("function:caller") + "/needs/E" + Hex16("struct:state") + ".md", "function:caller",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox = %v, want %v", got, want)
	}
}

// BuildInbox gives the rule the items in the ledger's order, so items whose stopped instructions and call height tie
// stay in the order the ledger enqueued them, whatever their ids.
func TestBuildInboxKeepsTheLedgerOrderOfTiedItems(t *testing.T) {
	later, earlier := "need:function:caller->struct:state@r2", "need:function:caller->struct:state@r9"
	items := []ledger.Item{{ID: earlier, Owner: "struct:state"}, {ID: later, Owner: "struct:state"}}
	priorities := map[string]Priority{earlier: {StoppedInstructions: 10}, later: {StoppedInstructions: 10}}

	inbox, err := BuildInbox(items, Config{RecordsRoot: "skeleton"}, priorities, loadRuleFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{inbox[0].ID, inbox[1].ID}; !reflect.DeepEqual(got, []string{earlier, later}) {
		t.Fatalf("inbox order = %v, want the ledger's order %v", got, []string{earlier, later})
	}
}

func TestReturnFromEventReadsEveryC13Field(t *testing.T) {
	event := ledger.Event{ID: 9, Kind: "migration.round_failed", Data: json.RawMessage(`{"generation":1,"landing_rounds":1,"reason":"check_refused:skeleton","batch":"b-1","key":"b-1/m-2/1","detail":"edge proof uses sorry","log":"logs/b-1/check.log","ready_cleared":true}`)}
	got, err := ReturnFromEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	want := ReturnFields{Reason: "check_refused:skeleton", Detail: "edge proof uses sorry", Log: "logs/b-1/check.log", Batch: "b-1", Key: "b-1/m-2/1"}
	if got != want {
		t.Fatalf("ReturnFromEvent = %+v, want %+v", got, want)
	}
}

// The producer requires a round's reason and key: an event without either is refused, naming the field.
func TestReturnFromEventRefusesAReturnWithoutReasonOrKey(t *testing.T) {
	for field, data := range map[string]string{
		"key":    `{"generation":1,"landing_rounds":1,"reason":"build_red"}`,
		"reason": `{"generation":1,"landing_rounds":1,"key":"b-1/m-2/1","detail":"x","log":"y","batch":"b-1"}`,
	} {
		event := ledger.Event{ID: 9, Kind: "migration.round_failed", Data: json.RawMessage(data)}
		if _, err := ReturnFromEvent(event); err == nil || !strings.Contains(err.Error(), "field "+field) {
			t.Fatalf("ReturnFromEvent without %s error = %v", field, err)
		}
	}
}
