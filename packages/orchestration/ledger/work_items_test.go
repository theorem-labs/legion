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

func TestOwnerWorkRevisionChangesOnlyWhenItsQueueChanges(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	revision, err := s.OwnerWorkRevision(ctx, "r", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if revision != 0 {
		t.Fatalf("initial revision = %d, want 0", revision)
	}
	if err := s.Enqueue(ctx, "r", "item-a", "owner-a"); err != nil {
		t.Fatal(err)
	}
	opened := must(s.OwnerWorkRevision(ctx, "r", "owner-a"))
	if opened == 0 {
		t.Fatal("enqueue left owner revision at zero")
	}
	lease := must(s.Claim(ctx, "r", "item-a", "worker", time.Minute))
	if got := must(s.OwnerWorkRevision(ctx, "r", "owner-a")); got != opened {
		t.Fatalf("claim changed owner work revision from %d to %d", opened, got)
	}
	if err := s.Complete(ctx, "r", "item-a", "worker", lease.Generation, "done"); err != nil {
		t.Fatal(err)
	}
	completed := must(s.OwnerWorkRevision(ctx, "r", "owner-a"))
	if completed <= opened {
		t.Fatalf("completion revision = %d, want greater than %d", completed, opened)
	}
	if err := s.Enqueue(ctx, "r", "item-b", "owner-b"); err != nil {
		t.Fatal(err)
	}
	if got := must(s.OwnerWorkRevision(ctx, "r", "owner-a")); got != completed {
		t.Fatalf("other owner's enqueue changed revision from %d to %d", completed, got)
	}
}
