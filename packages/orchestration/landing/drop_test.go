package landing

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// admissionStore is a MemoryStore whose reads report a fixed ledger admission, and which counts saves.
type admissionStore struct {
	MemoryStore
	admission string
	saves     int
}

func (s *admissionStore) Save(ctx context.Context, batch *Batch) error {
	s.saves++
	return s.MemoryStore.Save(ctx, batch)
}

func (s *admissionStore) Load(ctx context.Context, id string) (Batch, bool, error) {
	batch, found, err := s.MemoryStore.Load(ctx, id)
	batch.Admission = s.admission
	return batch, found, err
}

type dropLedger struct {
	refusals map[string]error
	returned []string
}

func (*dropLedger) Check(context.Context, string, string, int64, []string, map[string]string) (CheckResult, error) {
	return CheckResult{}, nil
}
func (*dropLedger) Land(context.Context, LandRequest) error { return nil }
func (l *dropLedger) ReturnMember(_ context.Context, _ string, migration string, _ int64, _ string) error {
	if err := l.refusals[migration]; err != nil {
		return err
	}
	l.returned = append(l.returned, migration)
	return nil
}
func (*dropLedger) MigrationState(context.Context, string, string) (string, string, error) {
	return "open", "", nil
}

type declineCall struct {
	record    Batch
	reason    string
	returns   []Return
	successor string
}

type dropBatchLedger struct {
	declines []declineCall
	err      error
}

func (*dropBatchLedger) Admit(context.Context, *Batch, []AdmitMember) error { return nil }
func (*dropBatchLedger) LandBatch(context.Context, *Batch, string) error    { return nil }
func (l *dropBatchLedger) DeclineBatch(_ context.Context, batch *Batch, reason string, returns []Return, successor string) error {
	if l.err != nil {
		return l.err
	}
	l.declines = append(l.declines, declineCall{record: cloneBatch(*batch), reason: reason, returns: returns, successor: successor})
	batch.RecordVersion++
	batch.Admission = AdmissionDeclined
	return nil
}
func (*dropBatchLedger) BeginWithdraw(context.Context, string, string) error { return nil }
func (*dropBatchLedger) ReadyMembers(context.Context, string) ([]Member, error) {
	return nil, nil
}

func dropFixture(t *testing.T, admission string) (*admissionStore, *dropLedger, *dropBatchLedger, Runner, *Batch) {
	t.Helper()
	store := &admissionStore{admission: admission}
	ledger := &dropLedger{refusals: map[string]error{}}
	batches := &dropBatchLedger{}
	members := []Member{
		{Migration: "a", Generation: 3, ReadyEvent: 30},
		{Migration: "b", Generation: 4, ReadyEvent: 40},
	}
	batch := &Batch{ID: "batch-1", Repository: "repo", State: Checked, FrozenMembers: cloneMembers(members), ActiveMembers: cloneMembers(members)}
	if _, err := store.Create(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	return store, ledger, batches, Runner{Store: store, Ledger: ledger, Admission: batches}, batch
}

func TestDropMembersNotAdmittedReturnsEachAndEndsEmpty(t *testing.T) {
	store, ledger, batches, runner, batch := dropFixture(t, NotAdmitted)
	ctx := context.Background()
	if err := runner.dropMembers(ctx, batch, map[string]string{"a": "overlap"}); err != nil {
		t.Fatal(err)
	}
	if batch.State != Checked || len(batch.ActiveMembers) != 1 {
		t.Fatalf("after first drop: state %q, active %v", batch.State, batch.ActiveMembers)
	}
	if err := runner.dropMembers(ctx, batch, map[string]string{"b": "stale_read"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ledger.returned, []string{"a", "b"}) {
		t.Fatalf("returned = %v", ledger.returned)
	}
	stored, _, _ := store.Load(ctx, batch.ID)
	if stored.State != Empty || len(stored.ActiveMembers) != 0 {
		t.Fatalf("stored state %q, active %v; want empty", stored.State, stored.ActiveMembers)
	}
	if len(batches.declines) != 0 {
		t.Fatalf("declined a batch that was not admitted")
	}
}

func TestDropMembersStaleGenerationCompletesDrop(t *testing.T) {
	store, ledger, _, runner, batch := dropFixture(t, NotAdmitted)
	ledger.refusals["a"] = &Refusal{Reason: "stale_generation", Member: "a"}
	if err := runner.dropMembers(context.Background(), batch, map[string]string{"a": "overlap"}); err != nil {
		t.Fatalf("drop error = %v", err)
	}
	stored, _, _ := store.Load(context.Background(), batch.ID)
	if _, found := findMember(stored.ActiveMembers, "a"); found {
		t.Fatal("member a is still active")
	}
	if len(stored.Dropped) != 1 || stored.Dropped[0].Reason != "stale_generation" {
		t.Fatalf("dropped = %+v", stored.Dropped)
	}
	if operation, found := stored.Operations["return/a"]; found && operation.State != OperationComplete {
		t.Fatalf("return intent is still open: %+v", operation)
	}
}

func TestDropMembersBatchAdmittedRefusalDropsNothing(t *testing.T) {
	store, ledger, _, runner, batch := dropFixture(t, NotAdmitted)
	ledger.refusals["a"] = &Refusal{Reason: "batch_admitted", Member: "a"}
	err := runner.dropMembers(context.Background(), batch, map[string]string{"a": "overlap"})
	if !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("drop error = %v, want ErrAdmissionConflict", err)
	}
	stored, _, _ := store.Load(context.Background(), batch.ID)
	if _, found := findMember(stored.ActiveMembers, "a"); !found {
		t.Fatal("member a left the active set")
	}
	if len(stored.Dropped) != 0 {
		t.Fatalf("dropped = %+v", stored.Dropped)
	}
	if _, found := findMember(batch.ActiveMembers, "a"); !found {
		t.Fatal("caller's batch lost member a")
	}
}

func TestDropMembersAdmittedDeclinesOnceAndNeverSaves(t *testing.T) {
	store, ledger, batches, runner, batch := dropFixture(t, Admitted)
	saves := store.saves
	if err := runner.dropMembers(context.Background(), batch, map[string]string{"a": "merge_group_tree_mismatch"}); err != nil {
		t.Fatal(err)
	}
	if len(batches.declines) != 1 {
		t.Fatalf("declines = %d, want 1", len(batches.declines))
	}
	call := batches.declines[0]
	wantReturns := []Return{{Migration: "a", Generation: 3, ReadyEvent: 30, Reason: "merge_group_tree_mismatch", Key: returnKey("batch-1", "a", 3)}}
	if !reflect.DeepEqual(call.returns, wantReturns) {
		t.Fatalf("returns = %+v", call.returns)
	}
	if call.successor != "batch-1-next" || call.record.Successor != call.successor || call.record.State != Declined {
		t.Fatalf("decline successor %q, record %q/%q", call.successor, call.record.State, call.record.Successor)
	}
	if store.saves != saves {
		t.Fatalf("store saves = %d, want %d", store.saves, saves)
	}
	if len(ledger.returned) != 0 {
		t.Fatalf("ReturnMember called for an admitted batch: %v", ledger.returned)
	}
	if batch.State != Declined || !batch.State.terminal() {
		t.Fatalf("batch state = %q", batch.State)
	}
}

func TestDropMembersDeclinedRecordKeepsSuccessor(t *testing.T) {
	_, _, batches, runner, batch := dropFixture(t, Admitted)
	if err := runner.dropMembers(context.Background(), batch, map[string]string{"a": "queue_removed", "b": "queue_removed"}); err != nil {
		t.Fatal(err)
	}
	if batch.Successor != "batch-1-next" || batches.declines[0].record.Successor != "batch-1-next" {
		t.Fatalf("successor = %q, record successor = %q", batch.Successor, batches.declines[0].record.Successor)
	}
	if batch.State == Empty {
		t.Fatal("a declined batch became empty")
	}
}

func TestDropMembersAdmittedStaleBatchIsConflict(t *testing.T) {
	_, _, batches, runner, batch := dropFixture(t, Admitted)
	batches.err = &Refusal{Reason: "stale_batch"}
	if err := runner.dropMembers(context.Background(), batch, map[string]string{"a": "x"}); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("drop error = %v, want ErrAdmissionConflict", err)
	}
	if _, found := findMember(batch.ActiveMembers, "a"); !found {
		t.Fatal("member a left the active set")
	}
}
