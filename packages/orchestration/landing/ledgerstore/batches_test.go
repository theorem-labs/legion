package ledgerstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic("unexpected error: " + err.Error())
	}
	return value
}

// newBatches opens a ledger on a fresh database with repository r configured, and returns the batch store over it.
func newBatches(t *testing.T) (*ledgerstore.Batches, *ledger.Store) {
	t.Helper()
	ctx := context.Background()
	dsn := testpg.Database(t, "landing_ledgerstore")
	store := must(ledger.Open(ctx, dsn))
	t.Cleanup(store.Close)
	must(store.EnsureRepo(ctx, "r", 3))
	return ledgerstore.New(store, "r"), store
}

// batch is a frozen batch of one member, in state.
func batch(id string, state landing.State) landing.Batch {
	return landing.Batch{
		ID:         id,
		Repository: "r",
		Base:       "base-1",
		BaseBranch: "main",
		FrozenMembers: []landing.Member{{
			Migration: "M-1", Generation: 1, Head: "head-M-1", Changed: []string{"a.lean"},
		}},
		ActiveMembers: []landing.Member{{
			Migration: "M-1", Generation: 1, Head: "head-M-1", Changed: []string{"a.lean"},
		}},
		State: state,
	}
}

// A created batch is stored whole, at record version 1, and loads back as it was written. A create of an id that the
// ledger already has creates nothing and gives the stored batch's version, so a retry after a lost response has one
// batch.
func TestCreateStoresTheWholeBatchOnce(t *testing.T) {
	ctx := context.Background()
	batches, _ := newBatches(t)
	created := batch("batch-1", landing.Frozen)
	wasCreated, err := batches.Create(ctx, &created)
	if err != nil || !wasCreated {
		t.Fatalf("create: %v, %v", wasCreated, err)
	}
	if created.RecordVersion != 1 || created.SchemaVersion != landing.BatchSchemaVersion {
		t.Fatalf("created batch = %+v, want record version 1 and the schema version", created)
	}
	loaded, found, err := batches.Load(ctx, "batch-1")
	if err != nil || !found {
		t.Fatalf("load: %v, %v", found, err)
	}
	if loaded.ID != created.ID || loaded.Base != created.Base || loaded.State != landing.Frozen ||
		loaded.RecordVersion != 1 || len(loaded.ActiveMembers) != 1 || loaded.ActiveMembers[0].Migration != "M-1" {
		t.Fatalf("loaded batch = %+v, want the created one", loaded)
	}

	again := batch("batch-1", landing.Frozen)
	wasCreated, err = batches.Create(ctx, &again)
	if err != nil || wasCreated {
		t.Fatalf("second create: %v, %v", wasCreated, err)
	}
	if again.RecordVersion != 1 {
		t.Fatalf("the second create left record version %d, want the stored 1", again.RecordVersion)
	}
	if reloaded, _, err := batches.Load(ctx, "batch-1"); err != nil || reloaded.RecordVersion != 1 {
		t.Fatalf("after the second create the batch is %+v, %v", reloaded, err)
	}
}

// A save writes at the version the caller read and raises it. A save at a version another writer moved past is
// refused with ErrStale and changes nothing.
func TestSaveRaisesTheVersionAndRefusesAStaleOne(t *testing.T) {
	ctx := context.Background()
	batches, store := newBatches(t)
	saved := batch("batch-1", landing.Frozen)
	must(batches.Create(ctx, &saved))

	saved.State = landing.Checked
	if err := batches.Save(ctx, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RecordVersion != 2 {
		t.Fatalf("after the save the version is %d, want 2", saved.RecordVersion)
	}

	stale := saved
	stale.RecordVersion = 1
	stale.State = landing.Building
	err := batches.Save(ctx, &stale)
	if !errors.Is(err, ledgerstore.ErrStale) {
		t.Fatalf("a save from an old version: %v, want a stale batch", err)
	}
	stored := must(store.LoadBatch(ctx, "r", "batch-1"))
	if stored.RecordVersion != 2 || stored.Phase != string(landing.Checked) {
		t.Fatalf("the refused save changed the batch: %+v", stored)
	}
}

// ListActive lists the batches that a restarted program still has work on, in creation order, with the record
// version each is stored at. A landed or failed batch is not listed. (A declined batch whose successor does not
// exist yet is listed too; only DeclineBatch writes that state, so the ledger's own tests cover it.)
func TestListActiveListsTheWorkThatIsLeft(t *testing.T) {
	ctx := context.Background()
	batches, _ := newBatches(t)
	for _, state := range []landing.State{landing.Frozen, landing.Landed, landing.Failed} {
		created := batch("batch-"+string(state), state)
		must(batches.Create(ctx, &created))
	}

	active, err := batches.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "batch-frozen" || active[0].RecordVersion != 1 {
		t.Fatalf("active batches = %+v, want the frozen one at version 1", active)
	}

	advanced := active[0]
	advanced.State = landing.Publishing
	if err := batches.Save(ctx, &advanced); err != nil {
		t.Fatal(err)
	}
	active = must(batches.ListActive(ctx))
	if len(active) != 1 || active[0].State != landing.Publishing || active[0].RecordVersion != 2 {
		t.Fatalf("active batches after the save = %+v", active)
	}
}

// The record holds the whole batch: another store over the same database, as a restarted program opens it, loads
// every field that the runner stored, and the stored record never carries a record version of its own.
func TestTheRecordHoldsTheWholeBatchAcrossARestart(t *testing.T) {
	ctx := context.Background()
	batches, store := newBatches(t)
	written := batch("batch-1", landing.Green)
	written.Branch = "land/batch-1"
	written.Round = 2
	written.CheckPolicyDigest = "policy-1"
	written.Checks = []landing.CheckVerdict{{Name: "lint", Verdict: "pass"}}
	written.Receipt = &landing.Receipt{Key: "key-1", Tree: "tree-1", Toolchain: "4.0.0"}
	must(batches.Create(ctx, &written))

	restarted := ledgerstore.New(store, "r")
	loaded, found, err := restarted.Load(ctx, "batch-1")
	if err != nil || !found {
		t.Fatalf("load after the restart: %v, %v", found, err)
	}
	if loaded.Branch != written.Branch || loaded.Round != written.Round ||
		loaded.CheckPolicyDigest != written.CheckPolicyDigest || len(loaded.Checks) != 1 ||
		loaded.Receipt == nil || loaded.Receipt.Tree != "tree-1" {
		t.Fatalf("batch after the restart = %+v, want the stored one", loaded)
	}
	stored := must(store.LoadBatch(ctx, "r", "batch-1"))
	var record map[string]any
	if err := json.Unmarshal(stored.Record, &record); err != nil {
		t.Fatal(err)
	}
	if _, present := record["record_version"]; present {
		t.Fatalf("the stored record carries a record version: %s", stored.Record)
	}
	if record["state"] != string(landing.Green) || record["schema_version"] != float64(landing.BatchSchemaVersion) {
		t.Fatalf("the stored record = %s", stored.Record)
	}
}

// A load of an id that the ledger does not have reports that it is absent, not an error.
func TestLoadOfAnUnknownBatchIsNotFound(t *testing.T) {
	ctx := context.Background()
	batches, _ := newBatches(t)
	loaded, found, err := batches.Load(ctx, "batch-missing")
	if err != nil || found {
		t.Fatalf("load of an unknown batch: %+v, %v, %v", loaded, found, err)
	}
}
