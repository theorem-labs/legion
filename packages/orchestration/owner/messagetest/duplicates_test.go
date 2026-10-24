package messagetest

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// answersAtOnce is the ledger as a target owner sees it when it answers an item the moment it is enqueued: the owner
// claims and completes the item before the driver's next call.
type answersAtOnce struct {
	*ledger.Store
	t *testing.T
}

func (a answersAtOnce) Enqueue(ctx context.Context, repo, id, owner string) error {
	if err := a.Store.Enqueue(ctx, repo, id, owner); err != nil {
		return err
	}
	item, err := a.Store.Item(ctx, repo, id)
	if err != nil {
		a.t.Fatal(err)
	}
	if item.State == "done" {
		return nil
	}
	lease, err := a.Store.Claim(ctx, repo, id, owner, 0)
	if err != nil {
		a.t.Fatal(err)
	}
	if err := a.Store.Complete(ctx, repo, id, owner, lease.Generation, "answered"); err != nil {
		a.t.Fatal(err)
	}
	return nil
}

// The duplicates step enqueues one need twice. The target may answer it between the two calls: the ledger then
// refuses the second as an existing item, which is a duplicate enqueue as much as the idempotent repeat of an open
// item, so the step goes on, and the run still checks one record and one completion. An item that existed before the
// step, or that belongs to another owner, still fails it.
func TestEnqueueTwiceTakesAnAnswerBetweenTheTwoCalls(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, testpg.Database(t, "message_test_duplicates"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	const owner, item = "struct:mountpoint", "need:function:a@x/y.c->struct:mountpoint@r1"

	if err := enqueueTwice(ctx, store, "r", item+"-open", owner); err != nil {
		t.Fatalf("two enqueues of an open item: %v", err)
	}
	if err := enqueueTwice(ctx, answersAtOnce{store, t}, "r", item, owner); err != nil {
		t.Fatalf("two enqueues around the owner's answer: %v", err)
	}
	current, err := store.Item(ctx, "r", item)
	if err != nil || current.State != "done" || current.Result != "answered" {
		t.Fatalf("item = %+v, %v; want done once with the owner's answer", current, err)
	}

	var refusal *ledger.Refusal
	if err := enqueueTwice(ctx, store, "r", item, owner); !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonExists {
		t.Fatalf("two enqueues of an item done before the step = %v, want the ledger's refusal", err)
	}
	if err := enqueueTwice(ctx, store, "r", item+"-open", "struct:other"); !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonExists {
		t.Fatalf("two enqueues of another owner's item = %v, want the ledger's refusal", err)
	}
}
