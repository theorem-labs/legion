package ledger_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// A lease taken with no length is supervised: it has no end, no sweep frees it, and nobody else
// can take the item while it is held.
func TestSupervisedLeaseNeverExpires(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	lease := must(s.Claim(ctx, "r", "item", "function:get_mountpoint", 0))
	if !lease.ExpiresAt.IsZero() {
		t.Fatalf("supervised lease has an end: %+v", lease)
	}
	if expired := must(s.Expire(ctx, "r")); len(expired) != 0 {
		t.Fatalf("the sweep freed a supervised lease: %+v", expired)
	}
	_, err := s.Claim(ctx, "r", "item", "bob", time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseHeld)
	it := must(s.Item(ctx, "r", "item"))
	if it.Holder != "function:get_mountpoint" || it.ExpiresAt != nil {
		t.Fatalf("supervised item: %+v", it)
	}
	st := must(s.Status(ctx, "r"))
	if len(st.Leases) != 1 || st.Leases[0].ID != "item" || len(st.Stale) != 0 {
		t.Fatalf("status of a supervised lease: leases %+v stale %+v", st.Leases, st.Stale)
	}
	if _, err := s.Renew(ctx, "r", "item", "function:get_mountpoint", lease.Generation, time.Minute); err == nil {
		t.Fatal("a supervised lease was renewed to an end")
	}
	if err := s.Complete(ctx, "r", "item", "function:get_mountpoint", lease.Generation, "proved"); err != nil {
		t.Fatalf("complete under a supervised lease: %v", err)
	}
}

// ReleaseHolder frees, in one transaction, every lease of the stopped holder at its current
// generation and no one else's; the holder's late result and renewal are refused, and the freed
// item is claimed again at the next generation.
func TestReleaseHolderFreesExactlyThatHoldersLeases(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Enqueue(ctx, "r", id, "struct:mount"); err != nil {
			t.Fatal(err)
		}
	}
	a := must(s.Claim(ctx, "r", "a", "struct:mount", 0))
	b := must(s.Claim(ctx, "r", "b", "struct:mount", time.Minute))
	c := must(s.Claim(ctx, "r", "c", "global:mount_lock", 0))

	freed := must(s.ReleaseHolder(ctx, "r", "struct:mount", "process gone"))
	ids := []string{}
	for _, it := range freed {
		ids = append(ids, it.ID)
	}
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("freed %v, want [a b]", ids)
	}
	for _, want := range []ledger.Lease{a, b} {
		it := must(s.Item(ctx, "r", want.ID))
		if it.Holder != "" || it.ExpiresAt != nil || it.Generation != want.Generation || it.State != "open" {
			t.Fatalf("freed item %s: %+v", want.ID, it)
		}
	}
	if it := must(s.Item(ctx, "r", "c")); it.Holder != "global:mount_lock" || it.Generation != c.Generation {
		t.Fatalf("another holder's lease changed: %+v", it)
	}
	wantRefusal(t, s.Complete(ctx, "r", "a", "struct:mount", a.Generation, "late"), ledger.ReasonLeaseExpired)
	_, err := s.Renew(ctx, "r", "b", "struct:mount", b.Generation, time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseExpired)

	again := must(s.Claim(ctx, "r", "a", "struct:mount", 0))
	if again.Generation != a.Generation+1 {
		t.Fatalf("re-claim generation %d, want %d", again.Generation, a.Generation+1)
	}
	wantRefusal(t, s.Complete(ctx, "r", "a", "struct:mount", a.Generation, "late"), ledger.ReasonStaleGeneration)

	stopped := 0
	for _, e := range must(s.Events(ctx, "r", 0, "")) {
		if e.Kind != "item.holder_stopped" {
			continue
		}
		stopped++
		var data struct {
			Generation int64  `json:"generation"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(e.Data, &data); err != nil || e.Actor != "struct:mount" || data.Reason != "process gone" {
			t.Fatalf("holder_stopped event: %+v %v", e, err)
		}
	}
	if stopped != 2 {
		t.Fatalf("%d item.holder_stopped events, want 2", stopped)
	}
	if again := must(s.ReleaseHolder(ctx, "r", "nobody", "gone")); len(again) != 0 {
		t.Fatalf("a holder with no leases freed %+v", again)
	}
}

func TestOpenOwnersListsEachOwnerWithOpenWorkOnce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if got := must(s.OpenOwners(ctx, "r")); got == nil || len(got) != 0 {
		t.Fatalf("open owners of an empty queue = %#v, want an empty list", got)
	}
	for _, item := range []struct{ id, owner string }{{"1", "struct:mount"}, {"2", "function:a_b"}, {"3", "struct:mount"}, {"4", "global:x"}} {
		if err := s.Enqueue(ctx, "r", item.id, item.owner); err != nil {
			t.Fatal(err)
		}
	}
	lease := must(s.Claim(ctx, "r", "4", "global:x", 0))
	if err := s.Complete(ctx, "r", "4", "global:x", lease.Generation, "done"); err != nil {
		t.Fatal(err)
	}
	if got := must(s.OpenOwners(ctx, "r")); !slices.Equal(got, []string{"function:a_b", "struct:mount"}) {
		t.Fatalf("open owners = %v", got)
	}
}

// The events that reopen work name their item's owner: an enqueue, a release, an expiry and a
// stopped holder. A claim and a completion reopen nothing.
func TestOwnerWakesNameTheOwnersOfReopenedWork(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	start := must(s.LatestEventID(ctx, "r"))
	if err := s.Enqueue(ctx, "r", "e", "function:enqueued"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, owner string }{{"rel", "struct:released"}, {"stop", "global:stopped"}, {"done", "subsystem:done"}} {
		if err := s.Enqueue(ctx, "r", item.id, item.owner); err != nil {
			t.Fatal(err)
		}
	}
	afterEnqueues := must(s.LatestEventID(ctx, "r"))
	rel := must(s.Claim(ctx, "r", "rel", "struct:released", 0))
	if err := s.Release(ctx, "r", "rel", "struct:released", rel.Generation); err != nil {
		t.Fatal(err)
	}
	must(s.Claim(ctx, "r", "stop", "global:stopped", 0))
	must(s.ReleaseHolder(ctx, "r", "global:stopped", "gone"))
	done := must(s.Claim(ctx, "r", "done", "subsystem:done", 0))
	if err := s.Complete(ctx, "r", "done", "subsystem:done", done.Generation, "ok"); err != nil {
		t.Fatal(err)
	}

	owners, last := must2(s.OwnerWakes(ctx, "r", start))
	if want := []string{"function:enqueued", "global:stopped", "struct:released", "subsystem:done"}; !slices.Equal(owners, want) {
		t.Fatalf("wakes after the start = %v, want %v", owners, want)
	}
	if last != must(s.LatestEventID(ctx, "r")) {
		t.Fatalf("cursor %d, want the latest event", last)
	}
	owners, _ = must2(s.OwnerWakes(ctx, "r", afterEnqueues))
	if want := []string{"global:stopped", "struct:released"}; !slices.Equal(owners, want) {
		t.Fatalf("wakes after the enqueues = %v, want %v", owners, want)
	}
	owners, again := must2(s.OwnerWakes(ctx, "r", last))
	if len(owners) != 0 || again != last {
		t.Fatalf("wakes after the cursor = %v at %d, want none at %d", owners, again, last)
	}
}

// A listener hears an enqueue's notification, which names its repository, as the transaction
// commits.
func TestListenerHearsAnEnqueue(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ledger.Listen(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	enqueued := time.Now()
	if err := s.Enqueue(ctx, "r", "item", "function:get_mountpoint"); err != nil {
		t.Fatal(err)
	}
	wait, done := context.WithTimeout(ctx, time.Second)
	defer done()
	repo, err := listener.Wait(wait)
	if err != nil || repo != "r" {
		t.Fatalf("notification = %q, %v; want r within a second", repo, err)
	}
	if elapsed := time.Since(enqueued); elapsed > time.Second {
		t.Fatalf("notification took %s", elapsed)
	}
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic("unexpected error: " + err.Error())
	}
	return a, b
}
