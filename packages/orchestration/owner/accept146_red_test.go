package owner

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// The C13 producer, `ledger round` at be7a3559, takes --batch, --detail and --log as optional, and writes each one it
// is not given as an empty string. Acceptance 2026-10-24 (Test146): `ledger round <repo> <m> 1 head_unavailable --key
// <k>` wrote event 65 with exactly the data below, and `owner returned returned:<m>@65` exited 4 with "event 65 lacks
// the C13 field detail": the owner could not read the reason of its own return, so it cannot act on the item. The
// event's reason and key are present; an owner must be able to read them.
func TestReturnFromEventReadsAReturnThatTheC13ProducerWroteWithoutDetailOrLog(t *testing.T) {
	event := ledger.Event{ID: 65, Kind: "migration.round_failed", Data: json.RawMessage(
		`{"key":"b-t146/m:K1f1ee8e7a5ab89a3:92/1","log":"","batch":"","detail":"","reason":"head_unavailable","generation":1,"ready_cleared":true,"landing_rounds":1}`)}
	got, err := ReturnFromEvent(event)
	if err != nil {
		t.Fatalf("ReturnFromEvent refused an event the C13 producer at be7a3559 writes: %v", err)
	}
	if got.Reason != "head_unavailable" || got.Key != "b-t146/m:K1f1ee8e7a5ab89a3:92/1" {
		t.Fatalf("ReturnFromEvent = %+v, want reason head_unavailable and the round's key", got)
	}
}
