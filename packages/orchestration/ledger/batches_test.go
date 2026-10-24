package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const escalationOwner = "steward:landing"

// batchRecord is a landing batch record in state. The ledger reads only state and, for landed_unrecorded,
// escalate_to; the other fields stand for the landing program's own.
func batchRecord(state string, fields ...any) json.RawMessage {
	body := map[string]any{"schema_version": 1, "state": state, "reason": "", "round": 0, "frozenMembers": []string{"M-1"}}
	for i := 0; i+1 < len(fields); i += 2 {
		body[fields[i].(string)] = fields[i+1]
	}
	return must(json.Marshal(body))
}

// unrecordedRecord is the record the landing program saves when the ledger refuses a landing after GitHub merged it.
func unrecordedRecord(escalateTo string) json.RawMessage {
	return batchRecord("landed_unrecorded", "reason", "merged, but the ledger refused the landing", "escalate_to", escalateTo)
}

func createBatch(t *testing.T, s *ledger.Store, id string, record json.RawMessage) ledger.Batch {
	t.Helper()
	batch, created, err := s.CreateBatch(context.Background(), "r", id, record)
	if err != nil || !created {
		t.Fatalf("create batch %s: created=%v err=%v", id, created, err)
	}
	return batch
}

// sameJSON reports whether two JSON texts hold the same value, whatever their key order or spacing.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// batchSQL runs statement on its own connection. The tests use it for the columns that only the calls of later tasks
// write (AdmitBatch, LandBatch, DeclineBatch and the effect log), so that SaveBatch and ListActiveBatches can be
// tested against them now.
func batchSQL(t *testing.T, dsn, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// setAdmission gives batch id the admission and successor that AdmitBatch, LandBatch or DeclineBatch leaves.
func setAdmission(t *testing.T, dsn, id, admission, successor string) {
	t.Helper()
	batchSQL(t, dsn, `UPDATE batches SET admission = nullif($2, ''), successor = nullif($3, '') WHERE repo = 'r' AND id = $1`,
		id, admission, successor)
}

// assertBatchUnchanged checks that batch is stored exactly as before and that repository r has no event it did not
// have.
func assertBatchUnchanged(t *testing.T, s *ledger.Store, before ledger.Batch, events int) {
	t.Helper()
	ctx := context.Background()
	if after := must(s.LoadBatch(ctx, "r", before.ID)); !reflect.DeepEqual(after, before) {
		t.Fatalf("batch changed:\n got %+v\nwant %+v", after, before)
	}
	if got := len(must(s.Events(ctx, "r", 0, ""))); got != events {
		t.Fatalf("%d events, want the %d there were", got, events)
	}
}

func eventKinds(t *testing.T, s *ledger.Store, subject string) []string {
	t.Helper()
	kinds := []string{}
	for _, e := range must(s.Events(context.Background(), "r", 0, subject)) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// CreateBatch stores the record at version 1 with its state as the phase and appends batch.created. A second create
// of the same id returns the stored batch, uncreated, whatever record it carries, and changes nothing.
func TestCreateBatchStoresTheRecordAtVersionOne(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	record := batchRecord("frozen", "candidate", map[string]any{"commit": "c1", "tree": "t1"})
	batch, created, err := s.CreateBatch(ctx, "r", "batch-1", record)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if batch.Repo != "r" || batch.ID != "batch-1" || batch.Phase != "frozen" || batch.RecordVersion != 1 ||
		batch.Admission != "" || batch.Successor != "" || batch.PendingEffects == nil || len(batch.PendingEffects) != 0 ||
		batch.CreatedAt.IsZero() || !batch.UpdatedAt.Equal(batch.CreatedAt) || !sameJSON(t, batch.Record, record) {
		t.Fatalf("created batch = %+v", batch)
	}
	if loaded := must(s.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, batch) {
		t.Fatalf("loaded %+v, want the created %+v", loaded, batch)
	}
	events := len(must(s.Events(ctx, "r", 0, "")))
	again, created, err := s.CreateBatch(ctx, "r", "batch-1", batchRecord("checked"))
	if err != nil || created || !reflect.DeepEqual(again, batch) {
		t.Fatalf("second create: created=%v err=%v batch=%+v, want the stored %+v", created, err, again, batch)
	}
	assertBatchUnchanged(t, s, batch, events)
	if got := eventKinds(t, s, "batch-1"); !slices.Equal(got, []string{"batch.created"}) {
		t.Fatalf("batch events = %q", got)
	}
	var missing *ledger.NotFoundError
	if _, _, err := s.CreateBatch(ctx, "missing", "batch-1", batchRecord("frozen")); !errors.As(err, &missing) || missing.What != "repository" {
		t.Fatalf("create in a missing repository: %v", err)
	}
}

// Every write of a batch row escalates a landed_unrecorded record in its own transaction, so a batch created in that
// state enqueues landing:<id> for escalate_to as a save to it does, and a repeat of the create enqueues nothing new.
func TestCreateBatchLandedUnrecordedEnqueuesItsEscalation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	created := createBatch(t, s, "batch-1", unrecordedRecord(escalationOwner))
	if created.Phase != "landed_unrecorded" {
		t.Fatalf("created %+v", created)
	}
	if item := must(s.Item(ctx, "r", "landing:batch-1")); item.Owner != escalationOwner || item.State != "open" {
		t.Fatalf("escalation item = %+v", item)
	}
	events := len(must(s.Events(ctx, "r", 0, "")))
	if again, fresh, err := s.CreateBatch(ctx, "r", "batch-1", unrecordedRecord(escalationOwner)); err != nil || fresh || !reflect.DeepEqual(again, created) {
		t.Fatalf("repeat create: created=%v err=%v batch=%+v", fresh, err, again)
	}
	assertBatchUnchanged(t, s, created, events)
}

// Concurrent creates of one id store one batch: every other create returns it, uncreated.
func TestConcurrentCreatesStoreOneBatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const creators = 8
	var wait sync.WaitGroup
	created := make([]bool, creators)
	errs := make([]error, creators)
	start := make(chan struct{})
	for i := range creators {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, created[i], errs[i] = s.CreateBatch(ctx, "r", "batch-1", batchRecord("frozen", "round", i))
		}()
	}
	close(start)
	wait.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if n := len(slices.DeleteFunc(created, func(c bool) bool { return !c })); n != 1 {
		t.Fatalf("%d concurrent creates created the batch, want 1", n)
	}
	if got := eventKinds(t, s, "batch-1"); !slices.Equal(got, []string{"batch.created"}) {
		t.Fatalf("batch events = %q", got)
	}
}

// The ledger trusts its caller: a save writes the record it is sent at the next version, and the phase becomes the
// record's state, whatever the stored phase and admission are. The admission and the successor are not the save's
// to change.
func TestSaveBatchWritesTheRecordItIsSent(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	for _, test := range []struct {
		name, phase, admission, state string
	}{
		{"active to active", "frozen", "", "checked"},
		{"active to failed", "building", "", "failed"},
		{"admitted to another active state", "checked", "admitted", "publishing"},
		{"landed to an active state", "landed", "landed", "checked"},
		{"declined to declined", "declined", "declined", "declined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := strings.ReplaceAll(test.name, " ", "-")
			createBatch(t, s, id, batchRecord(test.phase))
			setAdmission(t, dsn, id, test.admission, "next")
			before := must(s.LoadBatch(ctx, "r", id))
			record := batchRecord(test.state, "round", 2)
			saved := must(s.SaveBatch(ctx, "r", id, record, before.RecordVersion))
			if saved.Phase != test.state || saved.RecordVersion != before.RecordVersion+1 || saved.Admission != test.admission ||
				saved.Successor != "next" || !sameJSON(t, saved.Record, record) || !saved.CreatedAt.Equal(before.CreatedAt) ||
				!saved.UpdatedAt.After(before.UpdatedAt) {
				t.Fatalf("saved %+v from %+v", saved, before)
			}
			if loaded := must(s.LoadBatch(ctx, "r", id)); !reflect.DeepEqual(loaded, saved) {
				t.Fatalf("loaded %+v, want the saved %+v", loaded, saved)
			}
			if got := eventKinds(t, s, id); !slices.Equal(got, []string{"batch.created", "batch.saved"}) {
				t.Fatalf("batch events = %q", got)
			}
		})
	}
}

// Saving landed_unrecorded enqueues the work item landing:<id> for the record's escalate_to owner in the same
// transaction, and a restart finds both. A retry after a lost response returns the stored batch; a later save of the
// same record is a new save, but its enqueue finds the item open for that owner and appends nothing.
func TestSaveBatchLandedUnrecordedEnqueuesItsEscalationOnce(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	created := createBatch(t, s, "batch-1", batchRecord("publishing"))
	setAdmission(t, dsn, "batch-1", "admitted", "")
	record := unrecordedRecord(escalationOwner)

	saved := must(s.SaveBatch(ctx, "r", "batch-1", record, created.RecordVersion))
	if saved.Phase != "landed_unrecorded" || saved.RecordVersion != 2 || saved.Admission != "admitted" {
		t.Fatalf("saved %+v", saved)
	}
	s.Close()
	restarted := must(ledger.Open(ctx, dsn))
	defer restarted.Close()
	item := must(restarted.Item(ctx, "r", "landing:batch-1"))
	if item.Owner != escalationOwner || item.State != "open" || item.Holder != "" {
		t.Fatalf("escalation item = %+v", item)
	}
	if loaded := must(restarted.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, saved) {
		t.Fatalf("after a restart the batch is %+v, want the saved %+v", loaded, saved)
	}

	events := len(must(restarted.Events(ctx, "r", 0, "")))
	replayed := must(restarted.SaveBatch(ctx, "r", "batch-1", record, created.RecordVersion))
	if !reflect.DeepEqual(replayed, saved) {
		t.Fatalf("retry after a lost response returned %+v, want the stored %+v", replayed, saved)
	}
	assertBatchUnchanged(t, restarted, saved, events)
	again := must(restarted.SaveBatch(ctx, "r", "batch-1", record, saved.RecordVersion))
	if again.RecordVersion != saved.RecordVersion+1 || again.Phase != "landed_unrecorded" {
		t.Fatalf("a second save of the record = %+v", again)
	}
	if got := eventKinds(t, restarted, "landing:batch-1"); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q, want one item.enqueued", got)
	}
	if got := eventKinds(t, restarted, "batch-1"); !slices.Equal(got, []string{"batch.created", "batch.saved", "batch.saved"}) {
		t.Fatalf("batch events = %q", got)
	}
	var stored struct {
		Reason     string `json:"reason"`
		EscalateTo string `json:"escalate_to"`
	}
	if err := json.Unmarshal(must(restarted.LoadBatch(ctx, "r", "batch-1")).Record, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Reason != "merged, but the ledger refused the landing" || stored.EscalateTo != escalationOwner {
		t.Fatalf("stored landed_unrecorded record = %+v", stored)
	}
}

// When landing:<id> already belongs to another owner, Enqueue's rule refuses the escalation, and the save that needed
// it is refused with it and changes nothing: the escalation and the save commit together or not at all.
func TestSaveBatchLandedUnrecordedCommitsWithItsEscalation(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	if err := s.Enqueue(ctx, "r", "landing:batch-1", "steward:someone-else"); err != nil {
		t.Fatal(err)
	}
	createBatch(t, s, "batch-1", batchRecord("publishing"))
	setAdmission(t, dsn, "batch-1", "admitted", "")
	before := must(s.LoadBatch(ctx, "r", "batch-1"))
	events := len(must(s.Events(ctx, "r", 0, "")))
	_, err := s.SaveBatch(ctx, "r", "batch-1", unrecordedRecord(escalationOwner), before.RecordVersion)
	wantRefusal(t, err, ledger.ReasonExists)
	assertBatchUnchanged(t, s, before, events)
}

// Every save names the version it read. A save from a version another writer has already moved past is refused with
// stale_batch and changes nothing, and succeeds after a reload. A retry of a save whose response was lost returns the
// stored batch, version and all, and appends nothing; the same record saved again at the current version is a new
// save.
func TestSaveBatchComparesTheRecordVersion(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	created := createBatch(t, s, "batch-1", batchRecord("frozen"))

	competing := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("checked"), created.RecordVersion))
	events := len(must(s.Events(ctx, "r", 0, "")))
	_, err := s.SaveBatch(ctx, "r", "batch-1", batchRecord("building"), created.RecordVersion)
	refusal := wantRefusal(t, err, ledger.ReasonStaleBatch)
	if !strings.Contains(refusal.Message, "version 2") || !strings.Contains(refusal.Message, "version 1") {
		t.Fatalf("stale_batch message %q does not name the stored and the expected versions", refusal.Message)
	}
	assertBatchUnchanged(t, s, competing, events)
	reloaded := must(s.LoadBatch(ctx, "r", "batch-1"))
	second := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("building"), reloaded.RecordVersion))
	if second.RecordVersion != 3 || second.Phase != "building" {
		t.Fatalf("save after a reload = %+v", second)
	}

	third := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("green"), second.RecordVersion))
	events = len(must(s.Events(ctx, "r", 0, "")))
	retried := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("green"), second.RecordVersion))
	if !reflect.DeepEqual(retried, third) {
		t.Fatalf("retry after a lost response returned %+v, want the stored %+v", retried, third)
	}
	assertBatchUnchanged(t, s, third, events)
	_, err = s.SaveBatch(ctx, "r", "batch-1", batchRecord("green"), second.RecordVersion-1)
	wantRefusal(t, err, ledger.ReasonStaleBatch)
	assertBatchUnchanged(t, s, third, events)
	again := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("green"), third.RecordVersion))
	if again.RecordVersion != third.RecordVersion+1 {
		t.Fatalf("the same record saved at the current version = %+v, want version %d", again, third.RecordVersion+1)
	}

	var notFound *ledger.NotFoundError
	if _, err := s.SaveBatch(ctx, "r", "batch-2", batchRecord("checked"), 1); !errors.As(err, &notFound) || notFound.What != "batch" {
		t.Fatalf("save of a missing batch: %v", err)
	}
	if _, err := s.SaveBatch(ctx, "missing", "batch-1", batchRecord("checked"), 1); !errors.As(err, &notFound) || notFound.What != "repository" {
		t.Fatalf("save in a missing repository: %v", err)
	}
}

// Writers that save different records from the same version at once commit exactly one record; every other one is
// refused with stale_batch.
func TestConcurrentSavesFromOneVersionCommitOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	created := createBatch(t, s, "batch-1", batchRecord("frozen"))
	const writers = 12
	var wait sync.WaitGroup
	results := make([]error, writers)
	start := make(chan struct{})
	for i := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, results[i] = s.SaveBatch(ctx, "r", "batch-1", batchRecord("checked", "round", i+1), created.RecordVersion)
		}()
	}
	close(start)
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		wantRefusal(t, err, ledger.ReasonStaleBatch)
	}
	if winners != 1 {
		t.Fatalf("%d concurrent saves from one version committed, want 1", winners)
	}
	if got := must(s.LoadBatch(ctx, "r", "batch-1")).RecordVersion; got != 2 {
		t.Fatalf("record version %d after the concurrent saves, want 2", got)
	}
}

func activeIDs(t *testing.T, s *ledger.Store) []string {
	t.Helper()
	ids := []string{}
	for _, batch := range must(s.ListActiveBatches(context.Background(), "r")) {
		ids = append(ids, batch.ID)
	}
	return ids
}

// ListActiveBatches lists, by creation, every batch of the repository whose phase is active, every declined batch
// whose successor names no batch yet, and every terminal batch with an effect still in intent, with its pending
// effect keys. It lists the same after a restart, and nothing of another repository.
func TestListActiveBatchesListsWhatARestartMustFinish(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.EnsureRepo(ctx, "other", 3))
	if _, _, err := s.CreateBatch(ctx, "other", "batch-a", batchRecord("frozen")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"batch-c", "batch-a", "batch-b"} {
		createBatch(t, s, id, batchRecord("frozen"))
	}
	must(s.SaveBatch(ctx, "r", "batch-a", batchRecord("building"), 1))
	for _, state := range []string{"failed", "empty"} {
		created := createBatch(t, s, state, batchRecord("checked"))
		must(s.SaveBatch(ctx, "r", state, batchRecord(state), created.RecordVersion))
	}
	unrecorded := createBatch(t, s, "unrecorded", batchRecord("publishing"))
	setAdmission(t, dsn, "unrecorded", "admitted", "")
	must(s.SaveBatch(ctx, "r", "unrecorded", unrecordedRecord(escalationOwner), unrecorded.RecordVersion))
	for _, end := range []struct{ id, phase, admission, successor string }{
		{"landed", "landed", "landed", ""},
		{"declined-missing-successor", "declined", "declined", "successor"},
		{"declined-existing-successor", "declined", "declined", "batch-c"},
		{"declined-no-successor", "declined", "declined", ""},
		{"landed-with-effects", "landed", "landed", ""},
	} {
		createBatch(t, s, end.id, batchRecord(end.phase))
		setAdmission(t, dsn, end.id, end.admission, end.successor)
	}
	// No call writes an effect yet: LandBatch and ResolveBatch record a landed batch's effects (plan task L8c). The
	// test writes them directly to check the listing rule for a terminal batch with an effect still to do.
	batchSQL(t, dsn, `INSERT INTO batch_effects (repo, batch, key, op, state) VALUES
		('r', 'landed-with-effects', 'hook/landed-with-effects/1/score', 'hook', 'intent'),
		('r', 'landed-with-effects', 'hook/landed-with-effects/0/report', 'hook', 'intent'),
		('r', 'landed-with-effects', 'hook/landed-with-effects/2/done', 'hook', 'done'),
		('r', 'landed', 'hook/landed/0/report', 'hook', 'done')`)

	want := []string{"batch-c", "batch-a", "batch-b", "declined-missing-successor", "landed-with-effects"}
	if got := activeIDs(t, s); !slices.Equal(got, want) {
		t.Fatalf("active batches = %q, want %q", got, want)
	}
	s.Close()
	restarted := must(ledger.Open(ctx, dsn))
	defer restarted.Close()
	if got := activeIDs(t, restarted); !slices.Equal(got, want) {
		t.Fatalf("active batches after a restart = %q, want %q", got, want)
	}
	pending := map[string][]string{}
	for _, batch := range must(restarted.ListActiveBatches(ctx, "r")) {
		pending[batch.ID] = batch.PendingEffects
		if loaded := must(restarted.LoadBatch(ctx, "r", batch.ID)); !reflect.DeepEqual(loaded, batch) {
			t.Fatalf("listed %+v, but loading it gives %+v", batch, loaded)
		}
	}
	wantPending := map[string][]string{
		"batch-c": {}, "batch-a": {}, "batch-b": {}, "declined-missing-successor": {},
		"landed-with-effects": {"hook/landed-with-effects/0/report", "hook/landed-with-effects/1/score"},
	}
	if !reflect.DeepEqual(pending, wantPending) {
		t.Fatalf("pending effects = %q, want %q", pending, wantPending)
	}
	if other := must(restarted.ListActiveBatches(ctx, "other")); len(other) != 1 || other[0].Repo != "other" || other[0].Phase != "frozen" {
		t.Fatalf("active batches of the other repository = %+v", other)
	}

	batchSQL(t, dsn, `UPDATE batch_effects SET state = 'done' WHERE repo = 'r' AND batch = 'landed-with-effects'`)
	createBatch(t, restarted, "successor", batchRecord("empty", "reason", "no ready members"))
	want = []string{"batch-c", "batch-a", "batch-b"}
	if got := activeIDs(t, restarted); !slices.Equal(got, want) {
		t.Fatalf("active batches after the effects and the successor = %q, want %q", got, want)
	}
	var notFound *ledger.NotFoundError
	if _, err := restarted.ListActiveBatches(ctx, "missing"); !errors.As(err, &notFound) {
		t.Fatalf("active batches of a missing repository: %v", err)
	}
}

// The landing program saves a batch at every phase it passes through; after a restart, a new process loads the record
// and version it last saved, and lists the batch as active until it is terminal.
func TestBatchSurvivesARestartAtEveryLandingPhase(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	batch := createBatch(t, s, "batch-1", batchRecord("frozen"))
	store := s
	for _, record := range []json.RawMessage{
		batchRecord("checked"), batchRecord("building"), batchRecord("blaming"), batchRecord("green"),
		batchRecord("publishing"), nil, batchRecord("publishing", "pullRequest", map[string]any{"number": 7}),
		unrecordedRecord(escalationOwner),
	} {
		if record == nil {
			setAdmission(t, dsn, "batch-1", "admitted", "")
			continue
		}
		saved := must(store.SaveBatch(ctx, "r", "batch-1", record, batch.RecordVersion))
		store.Close()
		store = must(ledger.Open(ctx, dsn))
		batch = must(store.LoadBatch(ctx, "r", "batch-1"))
		if !reflect.DeepEqual(batch, saved) {
			t.Fatalf("after a restart at %s the batch is %+v, want %+v", saved.Phase, batch, saved)
		}
		if listed := slices.Contains(activeIDs(t, store), "batch-1"); listed == (saved.Phase == "landed_unrecorded") {
			t.Fatalf("after a restart at %s the batch is listed active: %v", saved.Phase, listed)
		}
	}
	store.Close()
}

// LoadBatch of an unknown batch, or of a known batch id in another repository, is not found.
func TestLoadBatchReadsOneRepository(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must(s.EnsureRepo(ctx, "other", 3))
	createBatch(t, s, "batch-1", batchRecord("frozen"))
	var notFound *ledger.NotFoundError
	for _, at := range [][2]string{{"r", "batch-2"}, {"other", "batch-1"}, {"missing", "batch-1"}} {
		if _, err := s.LoadBatch(ctx, at[0], at[1]); !errors.As(err, &notFound) || notFound.What != "batch" {
			t.Fatalf("load %s/%s: %v", at[0], at[1], err)
		}
	}
}

// Schema 0006 lets a migration be admitted in one batch at a time: a second admitted row of it is refused, and a new
// row is accepted once the first is released, as DeclineBatch and LandBatch release it.
func TestBatchMembersAdmitAMigrationOnce(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	createBatch(t, s, "batch-1", batchRecord("frozen"))
	createBatch(t, s, "batch-2", batchRecord("frozen"))
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)

	var definition string
	if err := conn.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE tablename = 'batch_members' AND indexname = 'batch_members_admitted'`).
		Scan(&definition); err != nil {
		t.Fatalf("the admitted-member index: %v", err)
	}
	if want := "CREATE UNIQUE INDEX batch_members_admitted ON public.batch_members USING btree (repo, migration) WHERE admitted"; definition != want {
		t.Fatalf("index definition %q, want %q", definition, want)
	}
	member := `INSERT INTO batch_members (repo, batch, migration, position, generation, head, ready_event, admitted)
		VALUES ('r', $1, 'M-1', 0, 1, 'head', 1, $2)`
	must(conn.Exec(ctx, member, "batch-1", true))
	_, err := conn.Exec(ctx, member, "batch-2", true)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23505" || databaseError.ConstraintName != "batch_members_admitted" {
		t.Fatalf("a second admitted row: %v, want a unique violation of batch_members_admitted", err)
	}
	must(conn.Exec(ctx, member, "batch-2", false))
	must(conn.Exec(ctx, `UPDATE batch_members SET admitted = false WHERE repo = 'r' AND batch = 'batch-1'`))
	must(conn.Exec(ctx, `UPDATE batch_members SET admitted = true WHERE repo = 'r' AND batch = 'batch-2'`))
	_, err = conn.Exec(ctx, `UPDATE batch_members SET admitted = true WHERE repo = 'r' AND batch = 'batch-1'`)
	if !errors.As(err, &databaseError) || databaseError.Code != "23505" {
		t.Fatalf("re-admitting the released row while another is admitted: %v, want a unique violation", err)
	}
}

// crashChildEnv names the database that TestSaveBatchCrashChild saves to. It is set only in the child process that
// the crash tests start.
const crashChildEnv = "LEDGER_TEST_CRASH_CHILD_DSN"

// TestSaveBatchCrashChild is not a test of its own: the crash tests run it in a child process. It saves batch-1 of
// repository r to landed_unrecorded from version 1, then kills its own process with SIGKILL, so the process ends
// with no cleanup right after the save's commit, or wherever the parent kills it first.
func TestSaveBatchCrashChild(t *testing.T) {
	dsn := os.Getenv(crashChildEnv)
	if dsn == "" {
		t.Skip("run only as the child process of the crash tests")
	}
	s, err := ledger.Open(context.Background(), dsn)
	if err != nil {
		os.Exit(2)
	}
	if _, err := s.SaveBatch(context.Background(), "r", "batch-1", unrecordedRecord(escalationOwner), 1); err != nil {
		os.Exit(3)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// startCrashChild starts TestSaveBatchCrashChild against dsn in a new process, whose connections carry
// application_name crash_child.
func startCrashChild(t *testing.T, dsn string) *exec.Cmd {
	t.Helper()
	childDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := childDSN.Query()
	query.Set("application_name", "crash_child")
	childDSN.RawQuery = query.Encode()
	child := exec.Command(os.Args[0], "-test.run=^TestSaveBatchCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), crashChildEnv+"="+childDSN.String())
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	return child
}

// waitForKill waits for child to end, and fails unless SIGKILL ended it.
func waitForKill(t *testing.T, child *exec.Cmd) {
	t.Helper()
	err := child.Wait()
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended with %v (%v), want SIGKILL", child.ProcessState, err)
	}
}

// waitForNoBackends waits until no database session of the killed child remains, so its transaction has ended.
func waitForNoBackends(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int64
		if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'crash_child'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions of the killed child remain", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// crashFixture is repository r with batch-1 admitted at version 1, as the landing program has it just before it
// saves landed_unrecorded.
func crashFixture(t *testing.T) (*ledger.Store, string, ledger.Batch) {
	t.Helper()
	s, dsn := newStoreDSN(t)
	createBatch(t, s, "batch-1", batchRecord("publishing"))
	setAdmission(t, dsn, "batch-1", "admitted", "")
	return s, dsn, must(s.LoadBatch(context.Background(), "r", "batch-1"))
}

// A process killed right after its landed_unrecorded save commits leaves both the save and its escalation item, and
// a new process finds them. A repeat of the save from that process enqueues nothing new.
func TestSaveBatchLandedUnrecordedSurvivesAKillAfterTheCommit(t *testing.T) {
	ctx := context.Background()
	s, dsn, _ := crashFixture(t)
	waitForKill(t, startCrashChild(t, dsn))
	s.Close()

	restarted := must(ledger.Open(ctx, dsn))
	defer restarted.Close()
	batch := must(restarted.LoadBatch(ctx, "r", "batch-1"))
	if batch.Phase != "landed_unrecorded" || batch.RecordVersion != 2 {
		t.Fatalf("after the kill the batch is %+v", batch)
	}
	if item := must(restarted.Item(ctx, "r", "landing:batch-1")); item.Owner != escalationOwner || item.State != "open" {
		t.Fatalf("after the kill the escalation item is %+v", item)
	}
	events := len(must(restarted.Events(ctx, "r", 0, "")))
	if replayed := must(restarted.SaveBatch(ctx, "r", "batch-1", unrecordedRecord(escalationOwner), 1)); !reflect.DeepEqual(replayed, batch) {
		t.Fatalf("the repeat returned %+v, want the stored %+v", replayed, batch)
	}
	assertBatchUnchanged(t, restarted, batch, events)
	if got := eventKinds(t, restarted, "landing:batch-1"); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q, want one item.enqueued", got)
	}
}

// A process killed inside its landed_unrecorded save, after the batch's update and while the escalation's enqueue
// waits, leaves neither: the batch is as it was and no escalation item exists. The test holds an uncommitted insert of
// the escalation item, so the child's enqueue waits on it; then it kills the child and rolls its insert back.
func TestSaveBatchLandedUnrecordedKilledBeforeTheCommitLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, dsn, before := crashFixture(t)
	events := len(must(s.Events(ctx, "r", 0, "")))
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	holder := must(conn.Begin(ctx))
	must(holder.Exec(ctx, `INSERT INTO work_items (repo, id, owner) VALUES ('r', 'landing:batch-1', $1)`, escalationOwner))

	child := startCrashChild(t, dsn)
	watcher := must(pgx.Connect(ctx, dsn))
	defer watcher.Close(ctx)
	waitForBlockedConnections(t, watcher, 1)
	var updated bool
	if err := watcher.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
		WHERE a.application_name = 'crash_child' AND l.locktype = 'transactionid' AND l.mode = 'ExclusiveLock' AND l.granted)`).
		Scan(&updated); err != nil || !updated {
		t.Fatalf("the child holds no transaction of its own while it waits (%v): it has not updated the batch", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitForKill(t, child)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	waitForNoBackends(t, watcher)

	assertBatchUnchanged(t, s, before, events)
	var notFound *ledger.NotFoundError
	if _, err := s.Item(ctx, "r", "landing:batch-1"); !errors.As(err, &notFound) {
		t.Fatalf("after a kill inside the save the escalation item exists: %v", err)
	}
	saved := must(s.SaveBatch(ctx, "r", "batch-1", unrecordedRecord(escalationOwner), before.RecordVersion))
	if saved.RecordVersion != 2 || must(s.Item(ctx, "r", "landing:batch-1")).Owner != escalationOwner {
		t.Fatalf("the save after the kill = %+v", saved)
	}
}
