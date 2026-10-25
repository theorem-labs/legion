package ledgerstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// readyMember opens migration id, declares write and the reads given, and readies it, so the ledger holds the ready
// record that an admission names. It returns the member the program freezes from that record.
func readyMember(t *testing.T, store *ledger.Store, id, owner, write string, reads map[string]string) landing.Member {
	t.Helper()
	ctx := context.Background()
	must(store.OpenMigration(ctx, ledger.OpenRequest{
		Repo: "r", ID: id, Owner: owner, BaseCommit: "base-1", Branch: "mig/" + id, Writes: []string{write}, Reads: reads,
	}))
	record := must(store.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: id, Generation: 1, Head: "head-" + id, Key: "ready-" + id}))
	current := map[string]string{}
	for name, hash := range reads {
		current[name] = hash
	}
	return landing.Member{
		Migration: id, Generation: 1, Head: "head-" + id, ReadyEvent: record.ReadyEvent,
		Changed: []string{write}, CurrentHashes: current,
	}
}

// openRequest opens migration id for owner with one declared write.
func openRequest(id, owner, write string) ledger.OpenRequest {
	return ledger.OpenRequest{
		Repo: "r", ID: id, Owner: owner, BaseCommit: "base-1", Branch: "mig/" + id, Writes: []string{write},
	}
}

// admittedBatch is batch-1 with M-1 admitted, and the member it holds.
func admittedBatch(t *testing.T) (*ledgerstore.Batches, *ledgerstore.Ledger, *ledger.Store, landing.Batch, landing.Member) {
	t.Helper()
	ctx := context.Background()
	batches, store := newBatches(t)
	member := readyMember(t, store, "M-1", "alice", "a.lean", nil)
	admitting := batch("batch-1", landing.Green)
	admitting.FrozenMembers = []landing.Member{member}
	admitting.ActiveMembers = []landing.Member{member}
	admitting.Candidate.Commit, admitting.Candidate.Tree = "candidate-1", "tree-1"
	must(batches.Create(ctx, &admitting))

	calls := ledgerstore.NewLedger(store, "r")
	admitting.State = landing.Publishing
	if err := calls.Admit(ctx, &admitting, []landing.AdmitMember{{
		Migration: member.Migration, ReadyEvent: member.ReadyEvent, CurrentHashes: map[string]string{}, ChangedPaths: member.Changed,
	}}); err != nil {
		t.Fatal(err)
	}
	return batches, calls, store, admitting, member
}

// An admission fences the batch's members, stores the batch's record at the next version, and leaves the batch
// admitted. The record it stores is the batch the runner holds.
func TestAdmitFencesTheMembersAndStoresTheRecord(t *testing.T) {
	ctx := context.Background()
	batches, _, store, admitted, _ := admittedBatch(t)
	if admitted.RecordVersion != 2 || admitted.Admission != landing.Admitted {
		t.Fatalf("after the admission the batch is %+v, want version 2 and admitted", admitted)
	}
	if stored := must(store.LoadBatch(ctx, "r", "batch-1")); stored.Admission != "admitted" || stored.RecordVersion != 2 {
		t.Fatalf("the stored batch is %+v", stored)
	}
	loaded, _, err := batches.Load(ctx, "batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != landing.Publishing || loaded.Admission != landing.Admitted || loaded.RecordVersion != 2 {
		t.Fatalf("the loaded batch is %+v", loaded)
	}
	// The fence holds: a takeover of the admitted member is refused.
	_, err = store.TakeOver(ctx, "r", "M-1", "bob", 1)
	var refusal *ledger.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonBatchAdmitted {
		t.Fatalf("a takeover of an admitted member: %v, want batch_admitted", err)
	}
}

// A member that the ledger refuses comes back as a member refusal that names it and its violation kinds, and
// nothing is admitted. A second runner's request for a batch another version moved past is a batch conflict.
func TestAdmitRoutesItsRefusals(t *testing.T) {
	ctx := context.Background()
	batches, store := newBatches(t)
	member := readyMember(t, store, "M-1", "alice", "a.lean", nil)
	admitting := batch("batch-1", landing.Green)
	admitting.ActiveMembers, admitting.FrozenMembers = []landing.Member{member}, []landing.Member{member}
	must(batches.Create(ctx, &admitting))
	calls := ledgerstore.NewLedger(store, "r")

	outside := []landing.AdmitMember{{
		Migration: member.Migration, ReadyEvent: member.ReadyEvent, CurrentHashes: map[string]string{},
		ChangedPaths: []string{"outside.lean"},
	}}
	err := calls.Admit(ctx, &admitting, outside)
	var refusal *landing.Refusal
	if !errors.As(err, &refusal) || !refusal.MemberRefusal() || refusal.Member != "M-1" {
		t.Fatalf("an admission of a member with an undeclared write: %v", err)
	}
	if !slices.Contains(refusal.Violations, "undeclared_write") {
		t.Fatalf("the refusal's violations are %q, want the member's kinds", refusal.Violations)
	}
	if stored := must(store.LoadBatch(ctx, "r", "batch-1")); stored.Admission != "" || stored.RecordVersion != 1 {
		t.Fatalf("the refused admission changed the batch: %+v", stored)
	}

	stale := admitting
	stale.RecordVersion = 0
	err = calls.Admit(ctx, &stale, []landing.AdmitMember{{
		Migration: member.Migration, ReadyEvent: member.ReadyEvent, CurrentHashes: map[string]string{}, ChangedPaths: member.Changed,
	}})
	if !errors.As(err, &refusal) || !refusal.BatchConflict() {
		t.Fatalf("an admission from an old version: %v, want a batch conflict", err)
	}
}

// LandBatch lands every member at GitHub's merged commit, releases the fence and stores the landed record.
func TestLandBatchLandsTheMembersAndReleasesTheFence(t *testing.T) {
	ctx := context.Background()
	_, calls, store, admitted, _ := admittedBatch(t)
	admitted.State = landing.Landed
	if err := calls.LandBatch(ctx, &admitted, "merged-1"); err != nil {
		t.Fatal(err)
	}
	if admitted.RecordVersion != 3 || admitted.Admission != landing.AdmissionLanded {
		t.Fatalf("after the landing the batch is %+v", admitted)
	}
	if m := must(store.Migration(ctx, "r", "M-1")); m.State != ledger.StateLanded || m.LandedCommit != "merged-1" {
		t.Fatalf("the member after the landing is %+v", m)
	}
}

// A landing records each post-landing effect of the batch's record as an intent, the batch shows them pending, and
// EffectDone finishes them one by one; a repeat succeeds.
func TestLandBatchRecordsItsPostLandingEffects(t *testing.T) {
	ctx := context.Background()
	batches, calls, _, admitted, _ := admittedBatch(t)
	admitted.State = landing.Landed
	admitted.PostLandEffects = []landing.PostLandEffect{{Key: "score", Op: "score"}, {Key: "event/M-1", Op: "event", Member: "M-1"}}
	if err := calls.LandBatch(ctx, &admitted, "merged-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(admitted.PendingEffects, []string{"event/M-1", "score"}) {
		t.Fatalf("pending effects after the landing = %q", admitted.PendingEffects)
	}
	for range 2 {
		if err := calls.EffectDone(ctx, "r", "batch-1", "score", []byte(`{"outcome":"scored"}`)); err != nil {
			t.Fatal(err)
		}
	}
	loaded, _, err := batches.Load(ctx, "batch-1")
	if err != nil || !slices.Equal(loaded.PendingEffects, []string{"event/M-1"}) {
		t.Fatalf("pending effects after one done = %q, %v", loaded.PendingEffects, err)
	}
	active := must(batches.ListActive(ctx))
	if len(active) != 1 || active[0].ID != "batch-1" {
		t.Fatalf("a landed batch with an effect to do is not listed: %+v", active)
	}
}

// DeclineBatch records each return at the member's frozen ready record, names the successor and stores the declined
// record. The returned member is open again, with its round counted.
func TestDeclineBatchReturnsItsMembers(t *testing.T) {
	ctx := context.Background()
	_, calls, store, admitted, member := admittedBatch(t)
	admitted.State = landing.Declined
	admitted.Successor = "batch-2"
	returns := []landing.Return{{
		Migration: member.Migration, Generation: member.Generation, ReadyEvent: member.ReadyEvent,
		Reason: "merge_group_tree_mismatch", Key: "batch-1/M-1/1",
	}}
	if err := calls.DeclineBatch(ctx, &admitted, "merge_group_tree_mismatch", returns, "batch-2"); err != nil {
		t.Fatal(err)
	}
	if admitted.RecordVersion != 3 || admitted.Admission != landing.AdmissionDeclined {
		t.Fatalf("after the decline the batch is %+v", admitted)
	}
	if m := must(store.Migration(ctx, "r", "M-1")); m.State != ledger.StateOpen || m.LandingRounds != 1 {
		t.Fatalf("the returned member is %+v", m)
	}
	if ready := must(store.ReadyMigrations(ctx, "r")); len(ready) != 0 {
		t.Fatalf("the returned member still has a ready record: %+v", ready)
	}
}

// EndWithdraw marks the withdraw effect done, and a repeat succeeds, so the batch leaves the active list once its
// admission ends.
func TestEndWithdrawMarksTheEffectDoneOnce(t *testing.T) {
	ctx := context.Background()
	_, calls, store, _, _ := admittedBatch(t)
	must(0, calls.BeginWithdraw(ctx, "r", "batch-1"))
	for range 2 {
		if err := calls.EndWithdraw(ctx, "r", "batch-1"); err != nil {
			t.Fatal(err)
		}
	}
	effects := must(store.BatchEffects(ctx, "r", "batch-1"))
	if len(effects) != 1 || effects[0].State != ledger.EffectDone {
		t.Fatalf("the batch's effects are %+v, want one done withdraw", effects)
	}
}

// BeginWithdraw records the batch's withdraw effect, and a repeat records nothing more.
func TestBeginWithdrawRecordsTheEffectOnce(t *testing.T) {
	ctx := context.Background()
	_, calls, store, _, _ := admittedBatch(t)
	for range 2 {
		if err := calls.BeginWithdraw(ctx, "r", "batch-1"); err != nil {
			t.Fatal(err)
		}
	}
	effects := must(store.BatchEffects(ctx, "r", "batch-1"))
	if len(effects) != 1 || effects[0].Key != "withdraw/batch-1" {
		t.Fatalf("the batch's effects are %+v, want one withdraw", effects)
	}
}
