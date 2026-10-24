package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const applyKey = "apply/batch-1/0/report"

// opsLines joins operations as the JSON lines that `declindex ledger --emit` writes.
func opsLines(operations ...any) []byte {
	var lines strings.Builder
	for _, operation := range operations {
		lines.Write(must(json.Marshal(operation)))
		lines.WriteByte('\n')
	}
	return []byte(lines.String())
}

func registerOp(name, owner, hash string) map[string]any {
	return map[string]any{"op": "register", "name": name, "owner": owner, "statement_hash": hash, "version_read": 0}
}

func librarianOp(id string) map[string]any {
	return map[string]any{"op": "librarian", "id": id, "kind": "near_pair", "proposer": "owner:alice", "envelope": "proposal " + id + "\n"}
}

// applyFixture completes the hook of batch-1 with ops as its output ops.jsonl and the contracts apply as its
// follow-on, as the landing program does after a `declindex ledger` run.
func applyFixture(t *testing.T, ops []byte) (*ledger.Store, string) {
	t.Helper()
	s, dsn := effectFixture(t)
	must(s.BatchEffectDone(context.Background(), "r", "batch-1", hookKey, ledger.DoneRequest{
		Result:  jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
		Then:    []ledger.FollowOn{{Key: applyKey, Op: "apply", Kind: "contracts", Input: "ops.jsonl"}},
		Outputs: []ledger.OutputFile{{Name: "ops.jsonl", Bytes: ops}},
	}))
	return s, dsn
}

// BatchEffectApplyContracts reads the operations from the hook's stored output. It registers an absent contract with
// an event that carries the landing commit and the effect key, leaves a present contract with the same owner and hash
// unchanged, creates librarian/<batch>/<id> for each librarian line with the line, the landing commit and the hook in
// its result, and marks the apply done with the names it applied and left unchanged. A repeat is refused with
// effect_done and creates nothing.
func TestApplyContractsAppliesTheHooksStoredOperations(t *testing.T) {
	ctx := context.Background()
	ops := opsLines(registerOp("New", "owner:alice", "h-new"), librarianOp("near-1"), registerOp("Same", "owner:bob", "h-same"), librarianOp("near-2"))
	s, dsn := applyFixture(t, ops)
	must(s.RegisterContract(ctx, "r", "Same", "owner:bob", "h-same", false))
	contractEvents := len(eventsOf(t, s, "Same", "contract.registered"))

	applied := must(s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey))
	if applied.State != ledger.EffectDone || !sameJSON(t, applied.Result, jsonValue(map[string]any{"applied": []string{"New"}, "unchanged": []string{"Same"}})) {
		t.Fatalf("the apply after it ran is %+v", applied)
	}
	contracts := map[string]ledger.Contract{}
	for _, c := range must(s.Contracts(ctx, "r")) {
		contracts[c.Name] = c
	}
	if c := contracts["New"]; len(contracts) != 2 || c.Owner != "owner:alice" || c.StatementHash != "h-new" || c.Version != 1 || c.Pinned {
		t.Fatalf("contracts after the apply = %+v", contracts)
	}
	registered := eventsOf(t, s, "New", "contract.registered")
	if len(registered) != 1 || registered[0].Actor != "owner:alice" || !sameJSON(t, registered[0].Data, jsonValue(map[string]any{
		"statement_hash": "h-new", "version": 1, "pinned": false, "batch": "batch-1", "effect": applyKey, "commit": landingCommit,
	})) {
		t.Fatalf("the registration's events = %+v", registered)
	}
	if got := len(eventsOf(t, s, "Same", "contract.registered")); got != contractEvents {
		t.Fatalf("the unchanged contract has %d registration events, want %d", got, contractEvents)
	}
	for _, id := range []string{"near-1", "near-2"} {
		intent := effectOf(t, s, "batch-1", "librarian/batch-1/"+id)
		if intent.Op != "librarian" || intent.State != ledger.EffectIntent ||
			!sameJSON(t, intent.Result, jsonValue(map[string]any{"line": librarianOp(id), "commit": landingCommit, "hook": hookKey})) {
			t.Fatalf("the librarian intent %s is %+v", id, intent)
		}
	}
	appliedEvents := eventsOf(t, s, "batch-1", "effect.applied")
	if len(appliedEvents) != 1 || !sameJSON(t, appliedEvents[0].Data, jsonValue(map[string]any{
		"key": applyKey, "commit": landingCommit, "applied": []string{"New"}, "unchanged": []string{"Same"},
		"follow_ons": []string{"librarian/batch-1/near-1", "librarian/batch-1/near-2"},
	})) {
		t.Fatalf("the apply's events = %+v", appliedEvents)
	}
	if pending, listed := pendingOf(t, s, "batch-1"); !listed || !slices.Equal(pending, []string{"librarian/batch-1/near-1", "librarian/batch-1/near-2"}) {
		t.Fatalf("after the apply the batch is listed=%v with pending %q", listed, pending)
	}

	before := snapshot(t, s, dsn, "batch-1")
	_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
	wantRefusal(t, err, ledger.ReasonEffectDone)
	assertLedgerUnchanged(t, s, dsn, before)
}

// A register line for a name that another landing has registered since the hook read the contracts, with another
// owner or another hash, refuses the whole call with contract_conflict, names the line, and applies nothing: no
// registration, no librarian intent, no done.
func TestApplyContractsConflictAppliesNothing(t *testing.T) {
	for _, test := range []struct{ name, owner, hash string }{
		{"another owner", "owner:carol", "h-taken"},
		{"another hash", "owner:alice", "h-other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			ops := opsLines(registerOp("A", "owner:alice", "h-a"), librarianOp("near-1"), registerOp("Taken", "owner:alice", "h-taken"))
			s, dsn := applyFixture(t, ops)
			must(s.RegisterContract(ctx, "r", "Taken", test.owner, test.hash, false))
			before := snapshot(t, s, dsn, "batch-1")
			_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
			refusal := wantRefusal(t, err, ledger.ReasonContractConflict)
			if !strings.Contains(refusal.Message, "line 3") || !strings.Contains(refusal.Message, "Taken") {
				t.Fatalf("the conflict does not name line 3 and its contract: %v", refusal)
			}
			assertLedgerUnchanged(t, s, dsn, before)
		})
	}
}

// A line that does not decode as a register or a librarian operation, or a registration that RegisterContract's own
// argument rules refuse, fails the call with nothing applied. So does an apply whose named output does not exist.
func TestApplyContractsRefusesWhatItCannotApply(t *testing.T) {
	for _, test := range []struct {
		name string
		ops  []byte
	}{
		{"not JSON", append(opsLines(registerOp("A", "owner:alice", "h-a")), []byte("not json\n")...)},
		{"an unknown op", opsLines(registerOp("A", "owner:alice", "h-a"), map[string]any{"op": "unregister", "name": "A"})},
		{"a field of the wrong type", opsLines(registerOp("A", "owner:alice", "h-a"), map[string]any{"op": "register", "name": 7, "owner": "o", "statement_hash": "h"})},
		{"a registration with no owner", opsLines(librarianOp("near-1"), registerOp("B", "", "h-b"))},
		{"a registration with no hash", opsLines(registerOp("A", "owner:alice", "h-a"), registerOp("B", "owner:alice", " "))},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := applyFixture(t, test.ops)
			before := snapshot(t, s, dsn, "batch-1")
			_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
			var invalid *ledger.InvalidError
			if !errors.As(err, &invalid) || !strings.Contains(invalid.Message, "line 2") {
				t.Fatalf("applying %q: %v, want an invalid line 2", test.ops, err)
			}
			assertLedgerUnchanged(t, s, dsn, before)
		})
	}
	t.Run("a missing output", func(t *testing.T) {
		ctx := context.Background()
		s, dsn := effectFixture(t)
		must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{
			Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
			Then:   []ledger.FollowOn{{Key: applyKey, Op: "apply", Kind: "contracts", Input: "ops.jsonl"}},
		}))
		before := snapshot(t, s, dsn, "batch-1")
		_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
		wantNotFound(t, err, "output")
		assertLedgerUnchanged(t, s, dsn, before)
	})
}

// An empty output applies nothing and marks the apply done with nothing applied or unchanged.
func TestApplyContractsOfAnEmptyOutputIsDone(t *testing.T) {
	ctx := context.Background()
	s, _ := applyFixture(t, nil)
	applied := must(s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey))
	if applied.State != ledger.EffectDone || !sameJSON(t, applied.Result, json.RawMessage(`{"applied":[],"unchanged":[]}`)) {
		t.Fatalf("the apply of an empty output is %+v", applied)
	}
	if contracts := must(s.Contracts(ctx, "r")); len(contracts) != 0 {
		t.Fatalf("an empty output registered %+v", contracts)
	}
}

// databaseDeadlocks is the number of deadlocks that Postgres has detected in observer's database, read after every
// other session of that database has ended, so each one has reported its counts.
func databaseDeadlocks(t *testing.T, observer *pgx.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var others int64
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&others); err != nil {
			t.Fatal(err)
		}
		if others == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d other sessions of the database remain", others)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var deadlocks int64
	if err := observer.QueryRow(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).Scan(&deadlocks); err != nil {
		t.Fatal(err)
	}
	return deadlocks
}

// Two applies whose hooks register the same two names in opposite orders run at once, and both finish with no
// deadlock: each name is registered by one of them and left unchanged by the other. The test holds uncommitted
// registrations of both names until both applies wait, so both reach their first registration together.
func TestConcurrentAppliesOfTheSameNamesDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	// Every session of the database ends before the test reads its deadlock count, so the first store closes now.
	first, dsn := newStoreDSN(t)
	first.Close()
	applierDSN := must(url.Parse(dsn))
	query := applierDSN.Query()
	query.Set("pool_max_conns", "2")
	applierDSN.RawQuery = query.Encode()
	s := must(ledger.Open(ctx, applierDSN.String()))
	for i, order := range [][]string{{"X", "Y"}, {"Y", "X"}} {
		id := fmt.Sprintf("batch-%d", i+1)
		landedBatch(t, s, dsn, id, "hook")
		must(s.BatchEffectDone(ctx, "r", id, "hook", ledger.DoneRequest{
			Result:  jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
			Then:    []ledger.FollowOn{{Key: "apply", Op: "apply", Kind: "contracts", Input: "ops.jsonl"}},
			Outputs: []ledger.OutputFile{{Name: "ops.jsonl", Bytes: opsLines(registerOp(order[0], "owner:alice", "h"), registerOp(order[1], "owner:alice", "h"))}},
		}))
	}
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	holderConn := must(pgx.Connect(ctx, dsn))
	holder := must(holderConn.Begin(ctx))
	must(holder.Exec(ctx, `INSERT INTO contracts (repo, name, owner, statement_hash) VALUES ('r', 'X', 'holder', 'h'), ('r', 'Y', 'holder', 'h')`))
	errs := make(chan error, 2)
	for _, id := range []string{"batch-1", "batch-2"} {
		go func() {
			_, err := s.BatchEffectApplyContracts(ctx, "r", id, "apply")
			errs <- err
		}()
	}
	waitForBlockedConnections(t, observer, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	must(0, holderConn.Close(ctx))
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	results := []json.RawMessage{effectOf(t, s, "batch-1", "apply").Result, effectOf(t, s, "batch-2", "apply").Result}
	s.Close()
	if deadlocks := databaseDeadlocks(t, observer); deadlocks != 0 {
		t.Fatalf("the two applies deadlocked %d times", deadlocks)
	}
	var registered, unchanged []string
	for _, result := range results {
		var r struct{ Applied, Unchanged []string }
		if err := json.Unmarshal(result, &r); err != nil {
			t.Fatal(err)
		}
		registered, unchanged = append(registered, r.Applied...), append(unchanged, r.Unchanged...)
	}
	slices.Sort(registered)
	slices.Sort(unchanged)
	if !reflect.DeepEqual(registered, []string{"X", "Y"}) || !reflect.DeepEqual(unchanged, []string{"X", "Y"}) {
		t.Fatalf("the applies registered %q and left %q unchanged; want each name registered once and unchanged once", registered, unchanged)
	}
}

// An apply and a RegisterContract of one of the apply's names, at once, do not deadlock. Every event takes the
// repository's event lock until its transaction commits, so the test holds that lock until both calls wait: an apply
// that appended a registration's event before its next registration would wait on RegisterContract's uncommitted
// insert while RegisterContract waits on the event lock. The apply registers both names, and RegisterContract, which
// waits on the apply's insert, is refused with exists.
func TestApplyAndRegisterContractAtOnceDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	first, dsn := newStoreDSN(t)
	first.Close()
	sized := must(url.Parse(dsn))
	query := sized.Query()
	query.Set("pool_max_conns", "2")
	sized.RawQuery = query.Encode()
	s := must(ledger.Open(ctx, sized.String()))
	landedBatch(t, s, dsn, "batch-1", hookKey)
	must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{
		Result:  jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
		Then:    []ledger.FollowOn{{Key: applyKey, Op: "apply", Kind: "contracts", Input: "ops.jsonl"}},
		Outputs: []ledger.OutputFile{{Name: "ops.jsonl", Bytes: opsLines(registerOp("X", "owner:alice", "h"), registerOp("Y", "owner:alice", "h"))}},
	}))
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	holderConn := must(pgx.Connect(ctx, dsn))
	holder := must(holderConn.Begin(ctx))
	must(holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('r', 0))`))

	applied := make(chan error, 1)
	go func() {
		_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
		applied <- err
	}()
	waitForBlockedConnections(t, observer, 1)
	registered := make(chan error, 1)
	go func() {
		_, err := s.RegisterContract(ctx, "r", "Y", "owner:bob", "h-bob", false)
		registered <- err
	}()
	waitForBlockedConnections(t, observer, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	must(0, holderConn.Close(ctx))
	if err := <-applied; err != nil {
		t.Fatalf("the apply: %v", err)
	}
	wantRefusal(t, <-registered, ledger.ReasonExists)
	if apply := effectOf(t, s, "batch-1", applyKey); !sameJSON(t, apply.Result, jsonValue(map[string]any{"applied": []string{"X", "Y"}, "unchanged": []string{}})) {
		t.Fatalf("the apply is %+v", apply)
	}
	if y := must(s.Contract(ctx, "r", "Y")); y.Owner != "owner:alice" {
		t.Fatalf("contract Y is %+v", y)
	}
	s.Close()
	if deadlocks := databaseDeadlocks(t, observer); deadlocks != 0 {
		t.Fatalf("the apply and the registration deadlocked %d times", deadlocks)
	}
}
