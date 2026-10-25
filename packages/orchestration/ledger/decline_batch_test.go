package ledger_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// declineFixture is repository r with batch-1 admitted at version 2: M-1, which changes contract C with its owner's
// review, M-2, which reads contract D, and M-3, in that order. members are what the landing program admitted.
type declineFixture struct {
	landingFixture
	members map[string]ledger.AdmitMember
}

func admittedDecline(t *testing.T) declineFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "d1", false))
	members := map[string]ledger.AdmitMember{
		"M-1": readyMember(t, s, "M-1", "alice", "a.lean", map[string]string{"C": "c1"}, changes("C", "c2", "alice")),
		"M-2": readyMember(t, s, "M-2", "bob", "b.lean", map[string]string{"D": "d1"}, nil),
		"M-3": readyMember(t, s, "M-3", "carol", "c.lean", nil, nil),
	}
	createBatch(t, s, "batch-1", batchRecord("checked"))
	admitted := must(s.AdmitBatch(ctx, admitRequest("batch-1", 1, members["M-1"], members["M-2"], members["M-3"])))
	return declineFixture{landingFixture: landingFixture{s: s, dsn: dsn, admitted: admitted}, members: members}
}

func (f declineFixture) state(t *testing.T) landingState {
	t.Helper()
	return readLandingState(t, f.s, f.dsn, "batch-1", "M-1", "M-2", "M-3")
}

// returned is the round that sends member id of batch-1 back to its owner.
func returned(id string) ledger.RoundRequest {
	return ledger.RoundRequest{Repo: "r", ID: id, Generation: 1, Reason: "build failed", Batch: "batch-1", Key: "batch-1/" + id}
}

func declineRequest(id string, version int64, successor string, returns ...ledger.RoundRequest) ledger.DeclineRequest {
	return ledger.DeclineRequest{
		Repo: "r", ID: id, Reason: "merge_group_tree_mismatch", Returns: returns, Successor: successor, ExpectedVersion: version,
		Record: batchRecord("declined", "reason", "merge_group_tree_mismatch", "successor", successor),
	}
}

func readyIDs(t *testing.T, s *ledger.Store) []string {
	t.Helper()
	ids := []string{}
	for _, record := range must(s.ReadyMigrations(context.Background(), "r")) {
		ids = append(ids, record.ID)
	}
	return ids
}

// DeclineBatch ends the admission without a landing: in one transaction it releases the fence, records each return as
// RecordRound records it (the member's round counts and its ready record clears), and writes the caller's record at
// the next version with the decline's digest and the successor, appending batch.declined. A member that is not
// returned keeps its ready record, so ReadyMigrations holds exactly the other two, and the fence no longer holds any.
func TestDeclineBatchReturnsOneMemberAndKeepsTheOthersReady(t *testing.T) {
	ctx := context.Background()
	f := admittedDecline(t)
	before := len(must(f.s.Events(ctx, "r", 0, "")))
	request := declineRequest("batch-1", f.admitted.RecordVersion, "batch-2", returned("M-3"))
	declined := must(f.s.DeclineBatch(ctx, request))
	if declined.Phase != ledger.PhaseDeclined || declined.Admission != "declined" || declined.Successor != "batch-2" ||
		len(declined.DeclineDigest) != 64 || declined.RecordVersion != f.admitted.RecordVersion+1 ||
		!sameJSON(t, declined.Record, request.Record) {
		t.Fatalf("declined batch = %+v", declined)
	}
	if loaded := must(f.s.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, declined) {
		t.Fatalf("stored batch %+v, want the returned %+v", loaded, declined)
	}
	for _, member := range batchMembers(t, f.dsn, "batch-1") {
		if member.Admitted {
			t.Fatalf("member %s is still admitted after the decline", member.Migration)
		}
	}
	if ready := readyIDs(t, f.s); !slices.Equal(ready, []string{"M-1", "M-2"}) {
		t.Fatalf("ready migrations after the decline = %q, want M-1 and M-2", ready)
	}
	if m := must(f.s.Migration(ctx, "r", "M-3")); m.State != ledger.StateOpen || m.LandingRounds != 1 {
		t.Fatalf("the returned member is %+v", m)
	}
	described, events := eventsSince(t, f.s, before)
	if !slices.Equal(described, []string{"migration.round_failed M-3", "batch.declined batch-1"}) {
		t.Fatalf("events of the decline = %q", described)
	}
	if data := eventData(t, events[0]); data["batch"] != "batch-1" || data["key"] != "batch-1/M-3" || data["ready_cleared"] != true {
		t.Fatalf("migration.round_failed = %v", data)
	}
	if data := eventData(t, events[1]); data["reason"] != "merge_group_tree_mismatch" || data["successor"] != "batch-2" ||
		!reflect.DeepEqual(data["returns"], []any{"M-3"}) || !reflect.DeepEqual(data["members"], []any{"M-1", "M-2", "M-3"}) {
		t.Fatalf("batch.declined = %v", data)
	}
	if transferred, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe"); err != nil || transferred.Owner != "zoe" {
		t.Fatalf("transfer after the decline: %+v, %v", transferred, err)
	}
}

// A decline with no returns (the queue removed the pull request, and no member is blamed) leaves every member ready
// and records no round.
func TestDeclineBatchWithNoReturnsKeepsEveryMemberReady(t *testing.T) {
	ctx := context.Background()
	f := admittedDecline(t)
	before := len(must(f.s.Events(ctx, "r", 0, "")))
	must(f.s.DeclineBatch(ctx, declineRequest("batch-1", f.admitted.RecordVersion, "batch-2")))
	if ready := readyIDs(t, f.s); !slices.Equal(ready, []string{"M-1", "M-2", "M-3"}) {
		t.Fatalf("ready migrations = %q", ready)
	}
	if described, _ := eventsSince(t, f.s, before); !slices.Equal(described, []string{"batch.declined batch-1"}) {
		t.Fatalf("events of the decline = %q", described)
	}
}

// After a competing save, a decline from the old version is refused with stale_batch and changes nothing; after a
// reload it declines.
func TestDeclineBatchComparesTheRecordVersion(t *testing.T) {
	ctx := context.Background()
	f := admittedDecline(t)
	competing := must(f.s.SaveBatch(ctx, "r", "batch-1", batchRecord("publishing", "round", 2), f.admitted.RecordVersion))
	before := f.state(t)
	_, err := f.s.DeclineBatch(ctx, declineRequest("batch-1", f.admitted.RecordVersion, "batch-2", returned("M-3")))
	wantRefusal(t, err, ledger.ReasonStaleBatch)
	if after := f.state(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused decline changed the ledger:\n got %+v\nwant %+v", after, before)
	}
	declined := must(f.s.DeclineBatch(ctx, declineRequest("batch-1", competing.RecordVersion, "batch-2", returned("M-3"))))
	if declined.RecordVersion != competing.RecordVersion+1 || declined.Admission != "declined" {
		t.Fatalf("decline after a reload = %+v", declined)
	}
}

// A retry of a decline whose response was lost carries the same reason, returns and successor: it returns the stored
// batch and changes nothing, whatever version and record it carries. A request that differs in any of them, from the
// old version, is a second runner's, refused with stale_batch. A restart finds the batch as the decline wrote it.
func TestDeclineBatchReplaysTheSameRequestOnly(t *testing.T) {
	ctx := context.Background()
	f := admittedDecline(t)
	request := declineRequest("batch-1", f.admitted.RecordVersion, "batch-2", returned("M-3"))
	declined := must(f.s.DeclineBatch(ctx, request))
	before := f.state(t)
	retry := request
	for _, version := range []int64{request.ExpectedVersion, 9} {
		retry.ExpectedVersion, retry.Record = version, batchRecord("other")
		if replayed, err := f.s.DeclineBatch(ctx, retry); err != nil || !reflect.DeepEqual(replayed, declined) {
			t.Fatalf("retry at version %d = %+v, %v; want the stored %+v", version, replayed, err, declined)
		}
	}
	for _, change := range []func(*ledger.DeclineRequest){
		func(r *ledger.DeclineRequest) { r.Reason = "queue_removed" },
		func(r *ledger.DeclineRequest) { r.Successor = "batch-3" },
		func(r *ledger.DeclineRequest) { r.Returns = nil },
		func(r *ledger.DeclineRequest) { r.Returns = []ledger.RoundRequest{returned("M-2")} },
		func(r *ledger.DeclineRequest) { r.Returns[0].Key = "another-key" },
	} {
		changed := declineRequest("batch-1", f.admitted.RecordVersion, "batch-2", returned("M-3"))
		change(&changed)
		_, err := f.s.DeclineBatch(ctx, changed)
		wantRefusal(t, err, ledger.ReasonStaleBatch)
	}
	if after := f.state(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a retry or a refused decline changed the ledger:\n got %+v\nwant %+v", after, before)
	}
	f.s.Close()
	restarted := must(ledger.Open(ctx, f.dsn))
	defer restarted.Close()
	if loaded := must(restarted.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, declined) {
		t.Fatalf("after a restart the batch is %+v, want %+v", loaded, declined)
	}
}

// After a decline and a restart, the declined batch is listed until its successor exists, and the members that were
// not returned form the successor from their unchanged ready records: the successor admits them with the same ready
// events, and the declined batch leaves the list.
func TestDeclinedBatchIsFollowedByItsSuccessorAfterARestart(t *testing.T) {
	ctx := context.Background()
	f := admittedDecline(t)
	must(f.s.DeclineBatch(ctx, declineRequest("batch-1", f.admitted.RecordVersion, "batch-2", returned("M-3"))))
	f.s.Close()

	restarted := must(ledger.Open(ctx, f.dsn))
	defer restarted.Close()
	if active := activeIDs(t, restarted); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("active batches after the restart = %q, want the declined batch-1 until its successor exists", active)
	}
	if ready := readyIDs(t, restarted); !slices.Equal(ready, []string{"M-1", "M-2"}) {
		t.Fatalf("ready migrations after the restart = %q", ready)
	}
	createBatch(t, restarted, "batch-2", batchRecord("checked"))
	successor := must(restarted.AdmitBatch(ctx, admitRequest("batch-2", 1, f.members["M-1"], f.members["M-2"])))
	if successor.Admission != "admitted" {
		t.Fatalf("the successor = %+v", successor)
	}
	if active := activeIDs(t, restarted); !slices.Equal(active, []string{"batch-2"}) {
		t.Fatalf("active batches with the successor = %q", active)
	}
}
