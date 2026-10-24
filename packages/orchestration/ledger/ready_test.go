package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func readyRequest(id string, generation int64, head string) ledger.ReadyRequest {
	return ledger.ReadyRequest{Repo: "r", ID: id, Generation: generation, StatementRecords: ledger.StatementRecords{Head: head, ChangedContracts: map[string]string{"A.contract": "hash-2", "B.contract": "hash-3"}, CompatibilityProof: "A.compatibility", ReviewedHead: head, Reviews: []ledger.Review{
		{Contract: "A.contract", Reviewer: "alice", Head: head},
		{Contract: "B.contract", Reviewer: "bob", Head: head},
	}, ApprovedPinned: []string{"A.contract", "B.contract"}}}
}

func TestReadyRoundTripsEveryFieldOrdersByEventAndReplaces(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(open(t, s, "M-2", "bob", []string{"b.lean"}, nil))

	first := must(s.Ready(ctx, readyRequest("M-2", 1, "head-2")))
	second := must(s.Ready(ctx, readyRequest("M-1", 1, "head-1")))
	firstAgainRequest := readyRequest("M-2", 1, "head-2b")
	firstAgainRequest.CompatibilityProof = "B.compatibility"
	firstAgainRequest.ReviewedHead = "reviewed-2b"
	firstAgainRequest.Reviews = []ledger.Review{{Contract: "A.contract", Reviewer: "carol", Head: "head-2b"}}
	firstAgainRequest.ApprovedPinned = []string{"B.contract"}
	firstAgain := must(s.Ready(ctx, firstAgainRequest))

	if firstAgain.ReadyEvent <= second.ReadyEvent || firstAgain.ReadyEvent == first.ReadyEvent {
		t.Fatalf("re-ready event %d, first %d, second %d", firstAgain.ReadyEvent, first.ReadyEvent, second.ReadyEvent)
	}
	ready := must(s.ReadyMigrations(ctx, "r"))
	if len(ready) != 2 || ready[0].ID != "M-1" || ready[1].ID != "M-2" {
		t.Fatalf("ready migrations are not in ready-event order: %+v", ready)
	}
	got := ready[1]
	if got.Owner != "bob" || got.Generation != 1 || got.Head != firstAgainRequest.Head || got.ReadyEvent != firstAgain.ReadyEvent || got.ReadyAt.IsZero() ||
		!reflect.DeepEqual(got.ChangedContracts, firstAgainRequest.ChangedContracts) || got.CompatibilityProof != firstAgainRequest.CompatibilityProof ||
		got.ReviewedHead != firstAgainRequest.ReviewedHead || !reflect.DeepEqual(got.Reviews, firstAgainRequest.Reviews) ||
		!reflect.DeepEqual(got.ApprovedPinned, firstAgainRequest.ApprovedPinned) || got.Upstream != firstAgainRequest.Upstream {
		t.Fatalf("ready record did not round-trip request: got %+v, request %+v", got, firstAgainRequest)
	}

	events := must(s.Events(ctx, "r", 0, "M-2"))
	var payload struct {
		Generation         int64             `json:"generation"`
		Head               string            `json:"head"`
		ChangedContracts   map[string]string `json:"changed_contracts"`
		CompatibilityProof string            `json:"compatibility_proof"`
		ReviewedHead       string            `json:"reviewed_head"`
		Reviews            []ledger.Review   `json:"reviews"`
		ApprovedPinned     []string          `json:"approved_pinned"`
		Upstream           string            `json:"upstream"`
	}
	readyEvents := 0
	for _, event := range events {
		if event.Kind != "migration.ready" {
			continue
		}
		readyEvents++
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if readyEvents != 2 || payload.Generation != firstAgainRequest.Generation || payload.Head != firstAgainRequest.Head ||
		!reflect.DeepEqual(payload.ChangedContracts, firstAgainRequest.ChangedContracts) || payload.CompatibilityProof != firstAgainRequest.CompatibilityProof ||
		payload.ReviewedHead != firstAgainRequest.ReviewedHead || !reflect.DeepEqual(payload.Reviews, firstAgainRequest.Reviews) ||
		!reflect.DeepEqual(payload.ApprovedPinned, firstAgainRequest.ApprovedPinned) || payload.Upstream != firstAgainRequest.Upstream {
		t.Fatalf("latest migration.ready payload did not preserve request: %+v", payload)
	}
}

func TestReadyRefusesClosedStaleAndEscalatedMigrations(t *testing.T) {
	ctx := context.Background()

	t.Run("not open", func(t *testing.T) {
		s := newStore(t)
		must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
		must(s.Abandon(ctx, "r", "M-1", 1, "stopped"))
		_, err := s.Ready(ctx, readyRequest("M-1", 1, "head"))
		wantRefusal(t, err, ledger.ReasonNotOpen)
	})
	t.Run("stale generation", func(t *testing.T) {
		s := newStore(t)
		must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
		must(s.TakeOver(ctx, "r", "M-1", "bob", 1))
		_, err := s.Ready(ctx, readyRequest("M-1", 1, "head"))
		wantRefusal(t, err, ledger.ReasonStaleGeneration)
	})
	t.Run("escalated", func(t *testing.T) {
		s := newStore(t)
		must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
		for _, key := range []string{"round-1", "round-2", "round-3"} {
			must(s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "failed", Key: key}))
		}
		_, err := s.Ready(ctx, readyRequest("M-1", 1, "head"))
		wantRefusal(t, err, ledger.ReasonEscalated)
	})
}

func TestReadyValidatesStoredRecordShape(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*ledger.ReadyRequest)
	}{
		{"changed contract name has whitespace", func(request *ledger.ReadyRequest) {
			request.ChangedContracts = map[string]string{" A.contract": "hash"}
		}},
		{"approved pinned name is empty", func(request *ledger.ReadyRequest) { request.ApprovedPinned = []string{""} }},
		{"compatibility proof has whitespace", func(request *ledger.ReadyRequest) { request.CompatibilityProof = " proof" }},
		{"review needs all fields", func(request *ledger.ReadyRequest) {
			request.Reviews = []ledger.Review{{Contract: "A.contract", Reviewer: "", Head: "head"}}
		}},
		{"reviewer has whitespace", func(request *ledger.ReadyRequest) {
			request.Reviews = []ledger.Review{{Contract: "A.contract", Reviewer: " alice", Head: "head"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
			request := readyRequest("M-1", 1, "head")
			test.mutate(&request)
			_, err := s.Ready(ctx, request)
			var invalid *ledger.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("want invalid record, got %v", err)
			}
		})
	}
}

func TestReadyClearsWhenMigrationMovesOn(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		move func(*testing.T, *ledger.Store)
		kind string
	}{
		{"takeover", func(t *testing.T, s *ledger.Store) { must(s.TakeOver(ctx, "r", "M-1", "bob", 1)) }, "migration.taken_over"},
		{"extend", func(t *testing.T, s *ledger.Store) {
			must(s.ExtendMigration(ctx, "r", "M-1", 1, []string{"b.lean"}, nil))
		}, "migration.extended"},
		{"abandon", func(t *testing.T, s *ledger.Store) { must(s.Abandon(ctx, "r", "M-1", 1, "stopped")) }, "migration.abandoned"},
		{"round", func(t *testing.T, s *ledger.Store) {
			must(s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "failed", Key: "round-1"}))
		}, "migration.round_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
			must(s.Ready(ctx, readyRequest("M-1", 1, "head")))
			test.move(t, s)
			if records := must(s.ReadyMigrations(ctx, "r")); len(records) != 0 {
				t.Fatalf("ready record remains after %s: %+v", test.name, records)
			}
			assertReadyCleared(t, s, test.kind, true)
		})
	}

	t.Run("no record", func(t *testing.T) {
		s := newStore(t)
		must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
		must(s.TakeOver(ctx, "r", "M-1", "bob", 1))
		assertReadyCleared(t, s, "migration.taken_over", false)
	})
}

func assertReadyCleared(t *testing.T, s *ledger.Store, kind string, want bool) {
	t.Helper()
	events := must(s.Events(context.Background(), "r", 0, "M-1"))
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		var data struct {
			ReadyCleared bool `json:"ready_cleared"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.ReadyCleared != want {
			t.Fatalf("%s ready_cleared=%t, want %t: %+v", kind, data.ReadyCleared, want, event)
		}
		return
	}
	t.Fatalf("missing %s event", kind)
}
