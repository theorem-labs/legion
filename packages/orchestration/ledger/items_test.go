package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestOpenItemsReturnsOnlyAnOwnersOpenItemsInEnqueueOrder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, item := range []struct {
		id, owner string
	}{
		{"a", "function:x"},
		{"b", "function:x"},
		{"c", "function:x"},
		{"d", "struct:y"},
	} {
		if err := s.Enqueue(ctx, "r", item.id, item.owner); err != nil {
			t.Fatalf("enqueue %s: %v", item.id, err)
		}
	}
	lease, err := s.Claim(ctx, "r", "a", "holder", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, "r", "a", "holder", lease.Generation, "done"); err != nil {
		t.Fatal(err)
	}

	items, err := s.OpenItems(ctx, "r", "function:x")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("OpenItems count = %d, want 2: %+v", len(items), items)
	}
	for index, wantID := range []string{"b", "c"} {
		if item := items[index]; item.ID != wantID || item.Owner != "function:x" || item.Item.Owner != "function:x" ||
			item.EnqueuedAt.IsZero() || item.EnqueueEvent == 0 {
			t.Fatalf("OpenItems[%d] = %+v, want open item %q with enqueue metadata", index, item, wantID)
		}
	}
	if items[0].EnqueueEvent >= items[1].EnqueueEvent {
		t.Fatalf("OpenItems order = %d then %d, want ascending enqueue events", items[0].EnqueueEvent, items[1].EnqueueEvent)
	}
}

func TestOpenItemsUnknownRepositoryReturnsNotFound(t *testing.T) {
	_, err := newStore(t).OpenItems(context.Background(), "missing", "function:x")
	var notFound *ledger.NotFoundError
	if !errors.As(err, &notFound) || notFound.What != "repository" || notFound.ID != "missing" {
		t.Fatalf("OpenItems missing repository error = %v, want repository NotFoundError", err)
	}
}

func TestOpenItemsPreservesLiveLease(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Enqueue(ctx, "r", "item", "function:x"); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Claim(ctx, "r", "item", "holder", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.OpenItems(ctx, "r", "function:x")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("OpenItems count = %d, want 1", len(items))
	}
	item := items[0]
	if item.Holder != "holder" || item.Generation != lease.Generation || item.ExpiresAt == nil || !item.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("OpenItems lease = %+v, want holder %q generation %d expiry %s", item, "holder", lease.Generation, lease.ExpiresAt)
	}
}

func TestOpenItemsReturnsEmptyQueueForExistingRepository(t *testing.T) {
	items, err := newStore(t).OpenItems(context.Background(), "r", "function:x")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("OpenItems = %+v, want no items", items)
	}
}
