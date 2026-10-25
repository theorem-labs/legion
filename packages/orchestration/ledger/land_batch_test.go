package ledger_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// landingEffects are the post-landing effects that the landing program froze at batch id's admission: a hook of the
// whole batch and a hook part of member M-1. The ledger reads only key, op and member of each.
func landingEffects(id string) []map[string]any {
	return []map[string]any{
		{"key": "hook/" + id + "/0/report", "op": "hook", "hook": "report"},
		{"key": "hook/" + id + "/1/score", "op": "hook", "member": "M-1", "part": "score"},
	}
}

// landedBatchRecord is the record the landing program writes when GitHub merged batch id's pull request at
// mergedCommit.
func landedBatchRecord(id, mergedCommit string) json.RawMessage {
	return batchRecord("landed", "pull_request", map[string]any{"number": 7, "merged_commit": mergedCommit},
		"post_land_effects", landingEffects(id))
}

func landBatchRequest(id string, version int64, mergedCommit string) ledger.LandBatchRequest {
	return ledger.LandBatchRequest{Repo: "r", ID: id, MergedCommit: mergedCommit, ExpectedVersion: version, Record: landedBatchRecord(id, mergedCommit)}
}

// landingFixture is repository r with batch-1 admitted at version 2: M-2, which reads contract D, at position 0, and
// M-1, which changes contract C from c1 to c2 with its owner alice's review at its head, at position 1. Agreement A-1
// read C at version 1.
type landingFixture struct {
	s        *ledger.Store
	dsn      string
	admitted ledger.Batch
}

func admittedLanding(t *testing.T) landingFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "d1", false))
	must(s.RecordAgreement(ctx, "r", "A-1", []string{"alice", "bob"}, map[string]int64{"C": 1}))
	changer := readyMember(t, s, "M-1", "alice", "a.lean", map[string]string{"C": "c1"}, changes("C", "c2", "alice"))
	reader := readyMember(t, s, "M-2", "bob", "b.lean", map[string]string{"D": "d1"}, nil)
	createBatch(t, s, "batch-1", batchRecord("checked"))
	admitted := must(s.AdmitBatch(ctx, admitRequest("batch-1", 1, reader, changer)))
	return landingFixture{s: s, dsn: dsn, admitted: admitted}
}

// effectRow is one row of batch_effects, with its result as JSON text.
type effectRow struct {
	Key    string
	Member string
	Op     string
	State  string
	Result string
}

// effectRows reads the effects of batch id, by key. The effect log's own calls read them too; these tests read the
// rows directly so that they check exactly what the landing wrote.
func effectRows(t *testing.T, dsn, id string) []effectRow {
	t.Helper()
	ctx := context.Background()
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	rows := must(conn.Query(ctx, `SELECT key, member, op, state, result::text FROM batch_effects
		WHERE repo = 'r' AND batch = $1 ORDER BY key`, id))
	effects := must(pgx.CollectRows(rows, pgx.RowToStructByPos[effectRow]))
	if effects == nil {
		effects = []effectRow{}
	}
	return effects
}

// landingState is everything that a batch landing, resolution or decline writes in a landing fixture.
type landingState struct {
	Batch      ledger.Batch
	Members    []memberRow
	Migrations []ledger.Migration
	Ready      []ledger.ReadyRecord
	Contracts  []ledger.Contract
	Stale      []ledger.Agreement
	Effects    []effectRow
	Events     int
}

func readLandingState(t *testing.T, s *ledger.Store, dsn, id string, migrations ...string) landingState {
	t.Helper()
	ctx := context.Background()
	state := landingState{
		Batch:     must(s.LoadBatch(ctx, "r", id)),
		Members:   batchMembers(t, dsn, id),
		Ready:     must(s.ReadyMigrations(ctx, "r")),
		Contracts: must(s.Contracts(ctx, "r")),
		Stale:     must(s.StaleAgreements(ctx, "r")),
		Effects:   effectRows(t, dsn, id),
		Events:    len(must(s.Events(ctx, "r", 0, ""))),
	}
	for _, migration := range migrations {
		state.Migrations = append(state.Migrations, must(s.Migration(ctx, "r", migration)))
	}
	return state
}

func (f landingFixture) state(t *testing.T) landingState {
	t.Helper()
	return readLandingState(t, f.s, f.dsn, "batch-1", "M-1", "M-2")
}

// eventsSince lists the kind and subject of each event of repository r after the first after events.
func eventsSince(t *testing.T, s *ledger.Store, after int) ([]string, []ledger.Event) {
	t.Helper()
	all := must(s.Events(context.Background(), "r", 0, ""))[after:]
	described := make([]string, len(all))
	for i, event := range all {
		described[i] = event.Kind + " " + event.Subject
	}
	return described, all
}

func eventData(t *testing.T, event ledger.Event) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// LandBatch lands the admitted members in position order at the merged commit, with Land's effects: the member that
// changes C advances it with its review as last_change, the agreement that read C's old version goes stale, and each
// member closes as landed at the merged commit. In the same transaction it releases the admission, writes the caller's
// record at the next version with the merged commit, records one intent per post_land_effects entry whose result
// holds the merged commit, and appends batch.landed. The batch stays listed while an intent is pending.
func TestLandBatchLandsEveryMemberInPositionOrder(t *testing.T) {
	ctx := context.Background()
	f := admittedLanding(t)
	before := len(must(f.s.Events(ctx, "r", 0, "")))
	request := landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-1")
	landed := must(f.s.LandBatch(ctx, request))

	wantEffects := []string{"hook/batch-1/0/report", "hook/batch-1/1/score"}
	if landed.Phase != ledger.PhaseLanded || landed.Admission != "landed" || landed.MergedCommit != "merged-1" ||
		landed.RecordVersion != f.admitted.RecordVersion+1 || !sameJSON(t, landed.Record, request.Record) ||
		!slices.Equal(landed.PendingEffects, wantEffects) {
		t.Fatalf("landed batch = %+v", landed)
	}
	if loaded := must(f.s.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, landed) {
		t.Fatalf("stored batch %+v, want the returned %+v", loaded, landed)
	}
	for _, member := range batchMembers(t, f.dsn, "batch-1") {
		if member.Admitted {
			t.Fatalf("member %s is still admitted after the landing", member.Migration)
		}
	}
	for _, id := range []string{"M-1", "M-2"} {
		if m := must(f.s.Migration(ctx, "r", id)); m.State != ledger.StateLanded || m.LandedCommit != "merged-1" {
			t.Fatalf("member %s after the landing: %+v", id, m)
		}
	}
	c := must(f.s.Contract(ctx, "r", "C"))
	review := json.RawMessage(`{"review":{"contract":"C","reviewer":"alice","head":"head-M-1"}}`)
	if c.StatementHash != "c2" || c.Version != 2 || !sameJSON(t, json.RawMessage(c.LastChange), review) {
		t.Fatalf("contract C after the landing: %+v", c)
	}
	if stale := must(f.s.StaleAgreements(ctx, "r")); len(stale) != 1 || stale[0].ID != "A-1" {
		t.Fatalf("stale agreements after the landing: %+v", stale)
	}

	described, events := eventsSince(t, f.s, before)
	want := []string{"migration.landed M-2", "contract.advanced C", "agreement.staled A-1", "migration.landed M-1", "batch.landed batch-1"}
	if !slices.Equal(described, want) {
		t.Fatalf("events of the landing = %q, want %q", described, want)
	}
	if data := eventData(t, events[3]); data["commit"] != "merged-1" || data["batch"] != "batch-1" || data["head"] != "head-M-1" {
		t.Fatalf("migration.landed of M-1 = %v", data)
	}
	if data := eventData(t, events[4]); data["merged_commit"] != "merged-1" || !reflect.DeepEqual(data["members"], []any{"M-2", "M-1"}) ||
		!reflect.DeepEqual(data["effects"], []any{"hook/batch-1/0/report", "hook/batch-1/1/score"}) {
		t.Fatalf("batch.landed = %v", data)
	}

	effects := effectRows(t, f.dsn, "batch-1")
	wantRows := []effectRow{
		{Key: "hook/batch-1/0/report", Member: "", Op: "hook", State: "intent"},
		{Key: "hook/batch-1/1/score", Member: "M-1", Op: "hook", State: "intent"},
	}
	for i := range effects {
		if !sameJSON(t, json.RawMessage(effects[i].Result), json.RawMessage(`{"commit":"merged-1"}`)) {
			t.Fatalf("effect %s result %s, want the merged commit", effects[i].Key, effects[i].Result)
		}
		effects[i].Result = ""
	}
	if !reflect.DeepEqual(effects, wantRows) {
		t.Fatalf("effects = %+v, want %+v", effects, wantRows)
	}
	if active := activeIDs(t, f.s); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("active batches after the landing = %q, want batch-1 while its effects are pending", active)
	}
}

// A record with no post-landing effects, its list null as the landing program encodes an empty one, records no intent,
// and the landed batch leaves the active list at once.
func TestLandBatchWithNoEffectsRecordsNoIntent(t *testing.T) {
	ctx := context.Background()
	f := admittedLanding(t)
	request := landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-1")
	request.Record = batchRecord("landed", "post_land_effects", nil)
	landed := must(f.s.LandBatch(ctx, request))
	if len(landed.PendingEffects) != 0 || len(effectRows(t, f.dsn, "batch-1")) != 0 {
		t.Fatalf("a landing with no effects recorded %+v", landed.PendingEffects)
	}
	if active := activeIDs(t, f.s); len(active) != 0 {
		t.Fatalf("active batches = %q", active)
	}
}

// The landing ends the fence: a contract that a member changed can be transferred, and a takeover of a member is
// refused because it landed (not_open), not because a batch holds it.
func TestLandBatchReleasesTheFence(t *testing.T) {
	ctx := context.Background()
	f := admittedLanding(t)
	_, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe")
	wantRefusal(t, err, ledger.ReasonBatchAdmitted)
	must(f.s.LandBatch(ctx, landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-1")))
	if transferred, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe"); err != nil || transferred.Owner != "zoe" {
		t.Fatalf("transfer after the landing: %+v, %v", transferred, err)
	}
	for _, id := range []string{"M-1", "M-2"} {
		_, err := f.s.TakeOver(ctx, "r", id, "zoe", 1)
		wantRefusal(t, err, ledger.ReasonNotOpen)
	}
}

// After a competing save, a landing from the old version is refused with stale_batch and changes nothing; after a
// reload it lands.
func TestLandBatchComparesTheRecordVersion(t *testing.T) {
	ctx := context.Background()
	f := admittedLanding(t)
	competing := must(f.s.SaveBatch(ctx, "r", "batch-1", batchRecord("publishing", "round", 2), f.admitted.RecordVersion))
	before := f.state(t)
	_, err := f.s.LandBatch(ctx, landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-1"))
	wantRefusal(t, err, ledger.ReasonStaleBatch)
	if after := f.state(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused landing changed the ledger:\n got %+v\nwant %+v", after, before)
	}
	landed := must(f.s.LandBatch(ctx, landBatchRequest("batch-1", competing.RecordVersion, "merged-1")))
	if landed.RecordVersion != competing.RecordVersion+1 || landed.Admission != "landed" {
		t.Fatalf("landing after a reload = %+v", landed)
	}
}

// A retry of a landing whose response was lost names the same merged commit: it returns the stored batch and changes
// nothing, whatever version and record it carries. Another merged commit from the old version is a second runner's
// request, refused with stale_batch. A restart finds the batch as the landing wrote it.
func TestLandBatchReplaysTheSameMergedCommit(t *testing.T) {
	ctx := context.Background()
	f := admittedLanding(t)
	request := landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-1")
	landed := must(f.s.LandBatch(ctx, request))
	before := f.state(t)
	retry := request
	for _, version := range []int64{request.ExpectedVersion, 9} {
		retry.ExpectedVersion, retry.Record = version, batchRecord("other")
		if replayed, err := f.s.LandBatch(ctx, retry); err != nil || !reflect.DeepEqual(replayed, landed) {
			t.Fatalf("retry at version %d = %+v, %v; want the stored %+v", version, replayed, err, landed)
		}
	}
	_, err := f.s.LandBatch(ctx, landBatchRequest("batch-1", f.admitted.RecordVersion, "merged-2"))
	wantRefusal(t, err, ledger.ReasonStaleBatch)
	if after := f.state(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a retry or a refused landing changed the ledger:\n got %+v\nwant %+v", after, before)
	}
	f.s.Close()
	restarted := must(ledger.Open(ctx, f.dsn))
	defer restarted.Close()
	if loaded := must(restarted.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, landed) {
		t.Fatalf("after a restart the batch is %+v, want %+v", loaded, landed)
	}
}
