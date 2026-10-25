package ledgerstore_test

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// A member frozen from the ledger carries every field the preliminary checks read: the statement records of its
// ready record (the reviews and the reviewed head that clear a statement change, and a compatibility proof) and the
// names it declared as reads, from which the program builds the current hashes of the full check. Without the
// reviews a reviewed change is refused with statement_unreviewed before admission; without the read names a
// proof-only migration that declares a read is refused with unverified_read.
func TestReadyMembersCarryWhatThePreliminaryChecksRead(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	must(store.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	must(store.RegisterContract(ctx, "r", "D", "alice", "d1", false))

	// A reviewed change of contract C.
	must(store.OpenMigration(ctx, ledger.OpenRequest{
		Repo: "r", ID: "M-reviewed", Owner: "alice", BaseCommit: "base", Branch: "mig/reviewed", Writes: []string{"C.lean"},
	}))
	must(store.Ready(ctx, ledger.ReadyRequest{
		Repo: "r", ID: "M-reviewed", Generation: 1, Key: "k-reviewed",
		StatementRecords: ledger.StatementRecords{
			ChangedContracts: map[string]string{"C": "c2"}, Head: "head-reviewed", ReviewedHead: "head-reviewed",
			Reviews: []ledger.Review{{Contract: "C", Reviewer: "bob", Head: "head-reviewed"}},
		},
	}))

	// A change cleared by a compatibility proof, which also declares a read of D.
	must(store.OpenMigration(ctx, ledger.OpenRequest{
		Repo: "r", ID: "M-proof", Owner: "carol", BaseCommit: "base", Branch: "mig/proof",
		Writes: []string{"P.lean"}, Reads: map[string]string{"D": "d1"},
	}))
	must(store.Ready(ctx, ledger.ReadyRequest{
		Repo: "r", ID: "M-proof", Generation: 1, Key: "k-proof",
		StatementRecords: ledger.StatementRecords{
			ChangedContracts: map[string]string{"C": "c3"}, Head: "head-proof", CompatibilityProof: "Proof.compat",
		},
	}))

	members, err := ledgerstore.NewLedger(store, "r").ReadyMembers(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("ready members = %+v, want two", members)
	}
	byID := map[string]landing.Member{}
	for _, member := range members {
		byID[member.Migration] = member
	}

	reviewed := byID["M-reviewed"]
	want := []landing.Review{{Contract: "C", Reviewer: "bob", Head: "head-reviewed"}}
	if !slices.Equal(reviewed.Reviews, want) || reviewed.ReviewedHead != "head-reviewed" {
		t.Fatalf("the reviewed member is %+v, want its reviews and reviewed head: the deferred check refuses it without them", reviewed)
	}
	if reviewed.ChangedContracts["C"] != "c2" || reviewed.Head != "head-reviewed" {
		t.Fatalf("the reviewed member lost its statement change: %+v", reviewed)
	}

	proof := byID["M-proof"]
	if proof.CompatibilityProof != "Proof.compat" {
		t.Fatalf("the proof member is %+v, want its compatibility proof", proof)
	}
	if !slices.Equal(proof.ReadNames, []string{"D"}) {
		t.Fatalf("the proof member's read names are %q, want D: the full check refuses it with unverified_read without them", proof.ReadNames)
	}
	if len(reviewed.ReadNames) != 0 {
		t.Fatalf("the reviewed member declares no read, but carries %q", reviewed.ReadNames)
	}
}
