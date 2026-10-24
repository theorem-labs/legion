package owner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestBuildInboxOrdersItemsAndNamesTheirRecords(t *testing.T) {
	need := "need:function:caller->struct:state@r1"
	offer := "offer:function:caller->struct:state@r1"
	items := []ledger.QueuedItem{
		{Item: ledger.Item{ID: need, Owner: "struct:state"}, Owner: "struct:state", EnqueuedAt: time.Date(2026, 10, 23, 12, 1, 0, 0, time.UTC)},
		{Item: ledger.Item{ID: offer, Owner: "function:caller"}, Owner: "function:caller", EnqueuedAt: time.Date(2026, 10, 23, 12, 2, 0, 0, time.UTC)},
	}
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

func TestReturnFromEventRefusesMissingC13Field(t *testing.T) {
	event := ledger.Event{ID: 9, Kind: "migration.round_failed", Data: json.RawMessage(`{"generation":1,"landing_rounds":1,"reason":"build_red"}`)}
	if _, err := ReturnFromEvent(event); err == nil || !strings.Contains(err.Error(), "detail") {
		t.Fatalf("ReturnFromEvent missing field error = %v", err)
	}
}
