package ledgerstore_test

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
)

// ReadyMembers lists every ready migration of the repository, in the order their records were published, with the
// ready event that a batch freezes. A migration without a ready record is not listed.
func TestReadyMembersListsEveryReadyMigration(t *testing.T) {
	ctx := context.Background()
	_, store := newBatches(t)
	first := readyMember(t, store, "M-1", "alice", "a.lean", nil)
	second := readyMember(t, store, "M-2", "bob", "b.lean", nil)
	must(store.OpenMigration(ctx, openRequest("M-3", "carol", "c.lean")))

	members := must(ledgerstore.NewLedger(store, "r").ReadyMembers(ctx, "r"))
	if len(members) != 2 || members[0].Migration != "M-1" || members[1].Migration != "M-2" {
		t.Fatalf("ready members = %+v, want M-1 and M-2", members)
	}
	if members[0].ReadyEvent != first.ReadyEvent || members[1].ReadyEvent != second.ReadyEvent ||
		members[0].Head != "head-M-1" || members[0].Generation != 1 {
		t.Fatalf("ready members = %+v, want the frozen ready records", members)
	}
}
