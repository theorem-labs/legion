package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

type roundEvent struct {
	Generation    int64  `json:"generation"`
	LandingRounds int    `json:"landing_rounds"`
	Reason        string `json:"reason"`
	Batch         string `json:"batch"`
	Key           string `json:"key"`
	Detail        string `json:"detail"`
	Log           string `json:"log"`
	ReadyCleared  bool   `json:"ready_cleared"`
}

func roundRequest(generation int64, key, reason string) ledger.RoundRequest {
	return ledger.RoundRequest{
		Repo: "r", ID: "M-1", Generation: generation, Reason: reason, Batch: "batch-a", Key: key,
		Detail: "detail for " + key, Log: "https://logs.example.test/" + key,
	}
}

func roundEvents(t *testing.T, s *ledger.Store) []roundEvent {
	t.Helper()
	var events []roundEvent
	for _, event := range must(s.Events(context.Background(), "r", 0, "M-1")) {
		if event.Kind != "migration.round_failed" {
			continue
		}
		var payload roundEvent
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		events = append(events, payload)
	}
	return events
}

func escalationEvents(t *testing.T, s *ledger.Store) int {
	t.Helper()
	count := 0
	for _, event := range must(s.Events(context.Background(), "r", 0, "M-1")) {
		if event.Kind == "migration.escalated" {
			count++
		}
	}
	return count
}

func TestRecordRoundRetryWithSameKeyCountsOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	request := roundRequest(1, "round-1", "build failed")

	first := must(s.RecordRound(ctx, request))
	retry := must(s.RecordRound(ctx, request))
	if first.LandingRounds != 1 || retry.LandingRounds != 1 {
		t.Fatalf("same key rounds: first=%+v retry=%+v", first, retry)
	}
	if events := roundEvents(t, s); len(events) != 1 {
		t.Fatalf("same key appended %d round events, want 1: %+v", len(events), events)
	}
}

// TestRecordRoundConcurrentRetryWithSameKeyCountsOnce catches a retry that reads round_keys before the first call
// commits: without the migration row lock, the retry would insert the same key and fail instead of returning.
func TestRecordRoundConcurrentRetryWithSameKeyCountsOnce(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	request := roundRequest(1, "round-1", "build failed")

	locker := must(pgx.Connect(ctx, dsn))
	defer locker.Close(ctx)
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	lock := must(locker.Begin(ctx))
	locked := true
	defer func() {
		if locked {
			_ = lock.Rollback(ctx)
		}
	}()
	// The first call stops at its event append, after it has inserted the key, until this lock is released.
	must(lock.Exec(ctx, `LOCK TABLE events IN SHARE MODE`))

	first := make(chan error, 1)
	go func() {
		_, err := s.RecordRound(ctx, request)
		first <- err
	}()
	waitForBlockedConnections(t, observer, 1)
	retry := make(chan error, 1)
	go func() {
		_, err := s.RecordRound(ctx, request)
		retry <- err
	}()
	waitForBlockedConnections(t, observer, 2)

	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-first; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := <-retry; err != nil {
		t.Fatalf("concurrent retry: %v", err)
	}
	if migration := must(s.Migration(ctx, "r", "M-1")); migration.LandingRounds != 1 {
		t.Fatalf("concurrent retry counted %d rounds, want 1", migration.LandingRounds)
	}
	if events := roundEvents(t, s); len(events) != 1 {
		t.Fatalf("concurrent retry appended %d round events, want 1", len(events))
	}
}

func TestRecordRoundRetryAfterTakeoverReturnsStoredResult(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	request := roundRequest(1, "round-1", "build failed")
	must(s.RecordRound(ctx, request))
	moved := must(s.TakeOver(ctx, "r", "M-1", "bob", 1))

	retry := must(s.RecordRound(ctx, request))
	if retry.Generation != 2 || retry.Owner != "bob" || retry.LandingRounds != 1 {
		t.Fatalf("retry after takeover: got %+v, current %+v", retry, moved)
	}
	if events := roundEvents(t, s); len(events) != 1 {
		t.Fatalf("retry after takeover appended round events: %+v", events)
	}
}

func TestRecordRoundDifferentKeysCountTwice(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))

	must(s.RecordRound(ctx, roundRequest(1, "round-1", "build failed")))
	second := must(s.RecordRound(ctx, roundRequest(1, "round-2", "test failed")))
	if second.LandingRounds != 2 {
		t.Fatalf("two keys counted %d rounds, want 2", second.LandingRounds)
	}
	if events := roundEvents(t, s); len(events) != 2 {
		t.Fatalf("two keys appended %d round events, want 2: %+v", len(events), events)
	}
}

func TestRecordRoundAtLimitEscalatesOnceAcrossRetry(t *testing.T) {
	s := newStore(t) // three rounds allowed
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(s.RecordRound(ctx, roundRequest(1, "round-1", "build failed")))
	must(s.RecordRound(ctx, roundRequest(1, "round-2", "test failed")))
	third := roundRequest(1, "round-3", "link failed")

	escalated := must(s.RecordRound(ctx, third))
	retry := must(s.RecordRound(ctx, third))
	if escalated.LandingRounds != 3 || escalated.EscalatedAt == nil || retry.LandingRounds != 3 || retry.EscalatedAt == nil {
		t.Fatalf("limit round: escalated=%+v retry=%+v", escalated, retry)
	}
	if got := escalationEvents(t, s); got != 1 {
		t.Fatalf("limit round and retry appended %d escalation events, want 1", got)
	}
}

func TestRecordRoundRefusesSameKeyWithDifferentBody(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ledger.RoundRequest)
	}{
		{"generation", func(request *ledger.RoundRequest) { request.Generation = 2 }},
		{"reason", func(request *ledger.RoundRequest) { request.Reason = "test failed" }},
		{"batch", func(request *ledger.RoundRequest) { request.Batch = "batch-b" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
			request := roundRequest(1, "round-1", "build failed")
			must(s.RecordRound(ctx, request))
			if test.name == "generation" {
				must(s.TakeOver(ctx, "r", "M-1", "bob", 1))
			}
			conflicting := request
			test.mutate(&conflicting)

			_, err := s.RecordRound(ctx, conflicting)
			wantRefusal(t, err, ledger.ReasonRoundKeyConflict)
			if migration := must(s.Migration(ctx, "r", "M-1")); migration.LandingRounds != 1 {
				t.Fatalf("conflicting key changed migration: %+v", migration)
			}
			if events := roundEvents(t, s); len(events) != 1 {
				t.Fatalf("conflicting key appended round events: %+v", events)
			}
		})
	}
}

func TestRecordRoundRequiresKey(t *testing.T) {
	s := newStore(t)
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	_, err := s.RecordRound(context.Background(), roundRequest(1, "", "build failed"))
	var invalid *ledger.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("empty key: want invalid input, got %v", err)
	}
}

// TestRecordRoundAfterCloseReplaysAndRefusesNewKey catches a round counted on a closed migration: after a landing or
// an abandonment, a retried key replays the closed migration with no change and no event, a new key is refused
// not_open, and the retried key with another reason is refused round_key_conflict.
func TestRecordRoundAfterCloseReplaysAndRefusesNewKey(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		close func(context.Context, *ledger.Store) error
	}{
		{"land", ledger.StateLanded, func(ctx context.Context, s *ledger.Store) error {
			_, err := s.Land(ctx, ledger.LandRequest{
				Repo: "r", Migration: "M-1", Generation: 1, Commit: "commit-1", ChangedPaths: []string{"a.lean"},
			})
			return err
		}},
		{"abandon", ledger.StateAbandoned, func(ctx context.Context, s *ledger.Store) error {
			_, err := s.Abandon(ctx, "r", "M-1", 1, "superseded")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
			request := roundRequest(1, "round-1", "build failed")
			must(s.RecordRound(ctx, request))
			if err := test.close(ctx, s); err != nil {
				t.Fatal(err)
			}
			before := len(must(s.Events(ctx, "r", 0, "M-1")))

			replay := must(s.RecordRound(ctx, request))
			if replay.State != test.state || replay.LandingRounds != 1 {
				t.Fatalf("replay after %s: %+v", test.name, replay)
			}
			_, err := s.RecordRound(ctx, roundRequest(1, "round-2", "build failed"))
			wantRefusal(t, err, ledger.ReasonNotOpen)
			_, err = s.RecordRound(ctx, roundRequest(1, "round-1", "test failed"))
			wantRefusal(t, err, ledger.ReasonRoundKeyConflict)

			if migration := must(s.Migration(ctx, "r", "M-1")); migration.State != test.state || migration.LandingRounds != 1 {
				t.Fatalf("migration after rounds on a closed migration: %+v", migration)
			}
			if after := len(must(s.Events(ctx, "r", 0, "M-1"))); after != before {
				t.Fatalf("rounds on a closed migration appended %d events", after-before)
			}
		})
	}
}

func TestRecordRoundEventHoldsEveryField(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(s.Ready(ctx, readyRequest("M-1", 1, "head-1")))
	request := ledger.RoundRequest{
		Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Batch: "batch-a", Key: "round-1",
		Detail: "compiler reported an error", Log: "https://logs.example.test/round-1",
	}

	must(s.RecordRound(ctx, request))
	events := roundEvents(t, s)
	want := roundEvent{
		Generation: 1, LandingRounds: 1, Reason: "build failed", Batch: "batch-a", Key: "round-1",
		Detail: "compiler reported an error", Log: "https://logs.example.test/round-1", ReadyCleared: true,
	}
	if len(events) != 1 || events[0] != want {
		t.Fatalf("round event: got %+v, want %+v", events, want)
	}
}
