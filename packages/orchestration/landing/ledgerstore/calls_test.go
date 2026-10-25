package ledgerstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// jsonFields lists the JSON names of a struct type's fields, with the fields of an embedded struct as its own.
func jsonFields(t reflect.Type) []string {
	var names []string
	for index := range t.NumField() {
		field := t.Field(index)
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			names = append(names, jsonFields(field.Type)...)
			continue
		}
		name, _, _ := cut(field.Tag.Get("json"))
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cut(tag string) (string, string, bool) {
	for index := range tag {
		if tag[index] == ',' {
			return tag[:index], tag[index+1:], true
		}
	}
	return tag, "", false
}

// The program's Land and Check requests have exactly the ledger's fields, by JSON name, so no field the ledger reads is
// left out and none the program sends is dropped.
func TestRequestsMatchTheLedgerFieldByField(t *testing.T) {
	for _, pair := range []struct {
		name           string
		program, owner reflect.Type
	}{
		{"LandRequest", reflect.TypeFor[landing.LandRequest](), reflect.TypeFor[ledger.LandRequest]()},
		{"CheckRequest", reflect.TypeFor[landing.CheckRequest](), reflect.TypeFor[ledger.CheckRequest]()},
		{"Review", reflect.TypeFor[landing.Review](), reflect.TypeFor[ledger.Review]()},
	} {
		if got, want := jsonFields(pair.program), jsonFields(pair.owner); !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %q, want the ledger's %q", pair.name, got, want)
		}
	}
}

// A check request reaches the ledger with every field it carries: the request the ledger received, encoded, is the
// program's request, encoded.
func TestCheckSendsEveryField(t *testing.T) {
	request := landing.CheckRequest{
		LandRequest: landing.LandRequest{
			Repo: "r", Migration: "M-1", Generation: 3, Commit: "commit-1", ChangedPaths: []string{"a.lean"},
			Current: map[string]string{"C": "c1"}, ChangedContracts: map[string]string{"C": "c2"}, Head: "head-1",
			CompatibilityProof: "Proof.compat", ReviewedHead: "head-0",
			Reviews:        []landing.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			ApprovedPinned: []string{"P"}, Upstream: "upstream-1",
		},
		DeferReads: true,
	}
	sent, err := json.Marshal(ledgerstore.CheckRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	program, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(sent, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(program, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the ledger receives %s, want %s", sent, program)
	}
}

// The deferred check of a clean member is incomplete and the full check passes; a member that writes outside its
// declared paths is refused with that violation's kind.
func TestCheckGivesTheLedgersDecision(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	member := readyMember(t, store, "M-1", "alice", "a.lean", nil)
	calls := ledgerstore.NewLedger(store, "r")
	base := landing.LandRequest{Repo: "r", Migration: "M-1", Generation: 1, ChangedPaths: member.Changed,
		Current: map[string]string{}, Head: member.Head}

	deferred, err := calls.Check(ctx, landing.CheckRequest{LandRequest: base, DeferReads: true})
	if err != nil || deferred.Outcome != landing.CheckIncomplete {
		t.Fatalf("deferred check = %+v, %v; want incomplete", deferred, err)
	}
	full := base
	full.Commit = "candidate-1"
	passed, err := calls.Check(ctx, landing.CheckRequest{LandRequest: full})
	if err != nil || passed.Outcome != landing.CheckPass || len(passed.Violations) != 0 {
		t.Fatalf("full check = %+v, %v; want pass", passed, err)
	}
	outside := full
	outside.ChangedPaths = []string{"outside.lean"}
	refused, err := calls.Check(ctx, landing.CheckRequest{LandRequest: outside})
	if err != nil || refused.Outcome != landing.CheckRefused || len(refused.Violations) != 1 ||
		refused.Violations[0].Kind != ledger.ReasonUndeclaredWrite || refused.Violations[0].Path != "outside.lean" {
		t.Fatalf("check of an undeclared write = %+v, %v; want refused with undeclared_write", refused, err)
	}
}

// A return records the round at the member's frozen ready event and clears that ready record; a retry of the same
// key replays. A return of a member taken over since is a refusal the runner reads as a completed drop.
func TestReturnMemberRecordsTheRoundOnce(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	member := readyMember(t, store, "M-1", "alice", "a.lean", nil)
	calls := ledgerstore.NewLedger(store, "r")
	round := landing.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Batch: "batch-1",
		Key: "batch-1/M-1/1", ReadyEvent: member.ReadyEvent, Detail: "red module"}
	for range 2 {
		if err := calls.ReturnMember(ctx, round); err != nil {
			t.Fatal(err)
		}
	}
	if m := must(store.Migration(ctx, "r", "M-1")); m.LandingRounds != 1 {
		t.Fatalf("landing rounds = %d, want one for the repeated key", m.LandingRounds)
	}
	if ready := must(store.ReadyMigrations(ctx, "r")); len(ready) != 0 {
		t.Fatalf("the returned member is still ready: %+v", ready)
	}

	must(store.TakeOver(ctx, "r", "M-1", "bob", 1))
	stale := round
	stale.Key = "batch-2/M-1/1"
	err := calls.ReturnMember(ctx, stale)
	var refusal *landing.LedgerRefusal
	if !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonStaleGeneration {
		t.Fatalf("a return at an old generation: %v, want a stale_generation refusal", err)
	}
}

// The contract list is the ledger's registered contracts, each with its statement hash.
func TestContractsListsTheRegisteredContracts(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	must(store.RegisterContract(ctx, "r", "B", "bob", "b1", false))
	must(store.RegisterContract(ctx, "r", "A", "alice", "a1", false))
	contracts, err := ledgerstore.NewLedger(store, "r").Contracts(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	want := []landing.Contract{{Name: "A", StatementHash: "a1"}, {Name: "B", StatementHash: "b1"}}
	if !reflect.DeepEqual(contracts, want) {
		t.Fatalf("contracts = %+v, want %+v", contracts, want)
	}
}

// An escalation enqueues the item for the owner once; a repeat for the same owner does nothing.
func TestEscalateEnqueuesOnce(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	calls := ledgerstore.NewLedger(store, "r")
	for range 2 {
		if err := calls.Escalate(ctx, "r", "landing:batch-1:hash@3", "steward"); err != nil {
			t.Fatal(err)
		}
	}
	events := must(store.Events(ctx, "r", 0, "landing:batch-1:hash@3"))
	if len(events) != 1 || events[0].Kind != "item.enqueued" {
		t.Fatalf("events of the item = %+v, want one item.enqueued", events)
	}
}
