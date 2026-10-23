package ledger_test

import (
	"context"
	"testing"
	"time"
)

// A standing owner is woken only by one of its open ledger items. Completing that item removes
// the wake condition without changing the item's lease history.
func TestEnqueueMakesOnlyItsOwnersOpenWorkVisible(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.Enqueue(ctx, "r", "item-1", "owner-a"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for _, tc := range []struct {
		owner string
		want  bool
	}{
		{owner: "owner-a", want: true},
		{owner: "owner-b", want: false},
	} {
		got, err := s.HasOpenWork(ctx, "r", tc.owner)
		if err != nil {
			t.Fatalf("HasOpenWork(%q): %v", tc.owner, err)
		}
		if got != tc.want {
			t.Errorf("HasOpenWork(%q) = %t, want %t", tc.owner, got, tc.want)
		}
	}

	lease := must(s.Claim(ctx, "r", "item-1", "worker", time.Minute))
	if err := s.Complete(ctx, "r", "item-1", "worker", lease.Generation, "finished"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := s.HasOpenWork(ctx, "r", "owner-a")
	if err != nil {
		t.Fatalf("HasOpenWork after completion: %v", err)
	}
	if got {
		t.Fatal("the completed item still wakes its owner")
	}
}
