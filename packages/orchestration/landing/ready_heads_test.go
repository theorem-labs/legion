package landing

import (
	"context"
	"errors"
	"testing"
)

// headsFake fetches every head except those in unavailable, and records each fetch.
type headsFake struct {
	unavailable map[string]bool
	fetched     []string
}

func (h *headsFake) FetchHead(_ context.Context, _ string, sha string) error {
	h.fetched = append(h.fetched, sha)
	if h.unavailable[sha] {
		return errors.New("not our ref " + sha)
	}
	return nil
}

func readyFixture(heads *headsFake) (Runner, *ledgerFake, []Member) {
	ready := []Member{
		{Migration: "first", Generation: 1, Head: "head-first", ReadyEvent: 11},
		{Migration: "second", Generation: 2, Head: "head-second", ReadyEvent: 12},
		{Migration: "third", Generation: 1, Head: "head-third", ReadyEvent: 13},
	}
	ledger := &ledgerFake{}
	return Runner{Admission: &readyBatchLedger{ready: ready}, Ledger: ledger, Git: newGitFake(), Heads: heads}, ledger, ready
}

// Each ready head is fetched by its SHA before the batch forms. A head that cannot be fetched is returned to its
// owner with head_unavailable, at the round key of the batch the ready list names and with the ready record it was
// read at; the batch forms from the others and keeps the id of the ready list as read, so a restart names it again.
func TestReadyInputReturnsAMemberWhoseHeadCannotBeFetched(t *testing.T) {
	ctx := context.Background()
	runner, _, _ := readyFixture(&headsFake{})
	whole, err := runner.ReadyInput(ctx, "repo", "trunk", 0)
	if err != nil {
		t.Fatal(err)
	}

	heads := &headsFake{unavailable: map[string]bool{"head-second": true}}
	runner, ledger, _ := readyFixture(heads)
	input, err := runner.ReadyInput(ctx, "repo", "trunk", 0)
	if err != nil {
		t.Fatalf("ReadyInput() error = %v", err)
	}
	if !sameStrings(heads.fetched, []string{"head-first", "head-second", "head-third"}) {
		t.Fatalf("fetched %v, want every ready head by its SHA", heads.fetched)
	}
	if len(input.Members) != 2 || input.Members[0].Migration != "first" || input.Members[1].Migration != "third" {
		t.Fatalf("members = %+v, want first and third", input.Members)
	}
	if input.ID != whole.ID {
		t.Fatalf("batch id = %q, want %q, the id of the ready list as read", input.ID, whole.ID)
	}
	if len(ledger.rounds) != 1 {
		t.Fatalf("rounds = %+v, want second returned once", ledger.rounds)
	}
	round := ledger.rounds[0]
	if round.ID != "second" || round.Reason != "head_unavailable" || round.Generation != 2 || round.ReadyEvent != 12 ||
		round.Batch != input.ID || round.Key != input.ID+"/second/2" || round.Repo != "repo" {
		t.Fatalf("round = %+v, want second returned with head_unavailable at %s/second/2", round, input.ID)
	}
}

// When no ready head can be fetched, every member is returned and the round forms nothing.
func TestReadyInputWithNoFetchableHeadFormsNothing(t *testing.T) {
	heads := &headsFake{unavailable: map[string]bool{"head-first": true, "head-second": true, "head-third": true}}
	runner, ledger, _ := readyFixture(heads)
	if _, err := runner.ReadyInput(context.Background(), "repo", "trunk", 0); !errors.Is(err, ErrNoReadyMembers) {
		t.Fatalf("ReadyInput() error = %v, want no ready member", err)
	}
	if len(ledger.rounds) != 3 {
		t.Fatalf("rounds = %+v, want all three returned", ledger.rounds)
	}
}

// A return that the ledger refuses because the member moved on (taken over or closed) is a completed return; one it
// refuses because another admitted batch holds the member leaves the member out of this round without returning it.
// Neither stops the round.
func TestReadyInputToleratesAMemberThatMovedOn(t *testing.T) {
	heads := &headsFake{unavailable: map[string]bool{"head-first": true, "head-second": true}}
	runner, _, _ := readyFixture(heads)
	runner.Ledger = &refusingReturns{reasons: map[string]string{"first": "stale_generation", "second": "batch_admitted"}}
	input, err := runner.ReadyInput(context.Background(), "repo", "trunk", 0)
	if err != nil {
		t.Fatalf("ReadyInput() error = %v", err)
	}
	if len(input.Members) != 1 || input.Members[0].Migration != "third" {
		t.Fatalf("members = %+v, want third alone", input.Members)
	}
}

// refusingReturns refuses the return of each named member with the reason given.
type refusingReturns struct {
	ledgerFake
	reasons map[string]string
}

func (l *refusingReturns) ReturnMember(ctx context.Context, round RoundRequest) error {
	if reason := l.reasons[round.ID]; reason != "" {
		return &LedgerRefusal{Reason: reason, Message: "refused"}
	}
	return l.ledgerFake.ReturnMember(ctx, round)
}

// A head that the ledger cannot return (a transport error) is an error of the round: the member is not dropped
// silently.
func TestReadyInputStopsOnAReturnThatFails(t *testing.T) {
	heads := &headsFake{unavailable: map[string]bool{"head-first": true}}
	runner, _, _ := readyFixture(heads)
	runner.Ledger = &failingReturns{}
	if _, err := runner.ReadyInput(context.Background(), "repo", "trunk", 0); err == nil || errors.Is(err, ErrNoReadyMembers) {
		t.Fatalf("ReadyInput() error = %v, want the return's failure", err)
	}
}

type failingReturns struct{ ledgerFake }

func (*failingReturns) ReturnMember(context.Context, RoundRequest) error {
	return errors.New("connection reset")
}
