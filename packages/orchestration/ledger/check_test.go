package ledger_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// TestDeferredCheckRefusesAStaleContractVersion catches a pre-build check that leaves to the build a read the ledger
// already knows is stale: M-1 read C at version 1, another migration advanced C, and a check with deferred reads
// refuses with stale_read instead of reporting incomplete.
func TestDeferredCheckRefusesAStaleContractVersion(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	writer := must(open(t, s, "M-writer", "alice", []string{"w.lean"}, map[string]string{"C": "hash-1"}))
	must(s.Land(ctx, landRequest(writer, "writer", []string{"w.lean"}, map[string]string{"C": "hash-1"},
		map[string]string{"C": "hash-2"}, nil)))

	request := checkRequestFor(landRequest(m, "", []string{"a.lean"}, nil, nil, nil))
	request.DeferReads = true
	res := must(s.Check(ctx, request))
	if res.Outcome != ledger.CheckOutcomeRefused || len(res.Violations) != 1 || res.Violations[0].Kind != ledger.ReasonStaleRead {
		t.Fatalf("deferred check of a stale contract version: %+v", res)
	}
}

// TestCheckRetriedAfterADeadlockReportsOneAttempt catches a retried check that keeps the violations of the attempt the
// database aborted. The check holds contract C and waits for the repository's event lock, which another session holds;
// that session then asks for C, so the check, which has waited longer, is the deadlock victim and is retried. Its
// result and its migration.checked event each list the candidate's violations once.
func TestCheckRetriedAfterADeadlockReportsOneAttempt(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))

	locker := must(pgx.Connect(ctx, dsn))
	defer locker.Close(ctx)
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	lock := must(locker.Begin(ctx))
	defer func() { _ = lock.Rollback(ctx) }()
	must(lock.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('r', 0))`))

	type outcome struct {
		result ledger.CheckResult
		err    error
	}
	checked := make(chan outcome, 1)
	go func() {
		// No commit and a wrong current hash: the candidate has two violations.
		result, err := s.Check(ctx, checkRequestFor(landRequest(m, "", []string{"a.lean"}, map[string]string{"C": "wrong"}, nil, nil)))
		checked <- outcome{result, err}
	}()
	waitForBlockedConnections(t, observer, 1)
	if _, err := lock.Exec(ctx, `SELECT 1 FROM contracts WHERE repo = 'r' AND name = 'C' FOR UPDATE`); err != nil {
		t.Fatalf("the locking session, not the check, was the deadlock victim: %v", err)
	}
	// The locking session got C while it still held the event lock, so the check's first attempt was aborted.
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-checked
	if got.err != nil {
		t.Fatal(got.err)
	}
	want := []string{ledger.ReasonInvalidCandidate, ledger.ReasonStaleRead}
	kinds := []string{}
	for _, v := range got.result.Violations {
		kinds = append(kinds, v.Kind)
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("retried check violations %v, want %v", kinds, want)
	}
	var recorded [][]string
	for _, event := range must(s.Events(ctx, "r", 0, "M-1")) {
		if event.Kind != "migration.checked" {
			continue
		}
		var data struct {
			Violations []string `json:"violations"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, data.Violations)
	}
	if !reflect.DeepEqual(recorded, [][]string{want}) {
		t.Fatalf("migration.checked events record %v, want one event with %v", recorded, want)
	}
}

// TestCheckSharesTheContractsItReads catches a check that takes its read contracts for update. A session that holds
// contract C under a share lock, as a running check does, must not block a check of another migration that read C;
// a session that holds C for update, as a landing does, must.
func TestCheckSharesTheContractsItReads(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	request := checkRequestFor(landRequest(m, "c", []string{"a.lean"}, map[string]string{"C": "hash-1"}, nil, nil))

	locker := must(pgx.Connect(ctx, dsn))
	defer locker.Close(ctx)
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	shared := must(locker.Begin(ctx))
	must(shared.Exec(ctx, `SELECT 1 FROM contracts WHERE repo = 'r' AND name = 'C' FOR SHARE`))
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := s.Check(bounded, request)
	if err != nil || !res.Pass {
		t.Fatalf("check beside a contract another session shares: %+v, %v; want a pass without waiting", res, err)
	}
	if err := shared.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	exclusive := must(locker.Begin(ctx))
	defer func() { _ = exclusive.Rollback(ctx) }()
	must(exclusive.Exec(ctx, `SELECT 1 FROM contracts WHERE repo = 'r' AND name = 'C' FOR UPDATE`))
	checked := make(chan error, 1)
	go func() {
		_, err := s.Check(ctx, request)
		checked <- err
	}()
	waitForBlockedConnections(t, observer, 1)
	if err := exclusive.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
}
