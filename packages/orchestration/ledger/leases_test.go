package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestTwentyConcurrentClaimersGetExactlyOneHolder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []string
	refused := 0
	start := make(chan struct{})
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			holder := fmt.Sprintf("agent-%02d", i)
			_, err := s.Claim(ctx, "r", "item-1", holder, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			var r *ledger.Refusal
			switch {
			case err == nil:
				winners = append(winners, holder)
			case errors.As(err, &r) && r.Reason == ledger.ReasonLeaseHeld:
				refused++
			default:
				t.Errorf("%s: unexpected error %v", holder, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(winners) != 1 || refused != 19 {
		t.Fatalf("winners %v, refused %d; want exactly one winner and 19 refusals", winners, refused)
	}
	it := must(s.Item(ctx, "r", "item-1"))
	if it.Holder != winners[0] || it.Generation != 1 {
		t.Fatalf("item holder %q generation %d; want %q generation 1", it.Holder, it.Generation, winners[0])
	}
}

func TestExpiredLeaseIsRegrantedAndOldHolderIsRefusedByGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	old := must(s.Claim(ctx, "r", "item", "alice", 150*time.Millisecond))
	_, err := s.Claim(ctx, "r", "item", "bob", time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseHeld)
	time.Sleep(300 * time.Millisecond)

	// A late renew or result from alice, before anyone else claims, is refused: the lease is over.
	_, err = s.Renew(ctx, "r", "item", "alice", old.Generation, time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseExpired)
	wantRefusal(t, s.Complete(ctx, "r", "item", "alice", old.Generation, "late"), ledger.ReasonLeaseExpired)

	bob := must(s.Claim(ctx, "r", "item", "bob", time.Minute))
	if bob.Generation != old.Generation+1 {
		t.Fatalf("re-grant generation %d, want %d", bob.Generation, old.Generation+1)
	}
	_, err = s.Renew(ctx, "r", "item", "alice", old.Generation, time.Minute)
	wantRefusal(t, err, ledger.ReasonStaleGeneration)
	wantRefusal(t, s.Complete(ctx, "r", "item", "alice", old.Generation, "late"), ledger.ReasonStaleGeneration)
	wantRefusal(t, s.Release(ctx, "r", "item", "alice", old.Generation), ledger.ReasonStaleGeneration)
	// The right generation under the wrong name is refused too.
	wantRefusal(t, s.Complete(ctx, "r", "item", "alice", bob.Generation, "x"), ledger.ReasonNotHolder)

	if err := s.Complete(ctx, "r", "item", "bob", bob.Generation, "proved"); err != nil {
		t.Fatal(err)
	}
	it := must(s.Item(ctx, "r", "item"))
	if it.State != "done" || it.Result != "proved" || it.Holder != "" {
		t.Fatalf("after complete: %+v", it)
	}
	_, err = s.Claim(ctx, "r", "item", "carol", time.Minute)
	wantRefusal(t, err, ledger.ReasonDone)
}

func TestRenewMovesExpiryByRequestedLength(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	l := must(s.Claim(ctx, "r", "item", "alice", time.Minute))
	ttl := 2 * time.Minute
	r := must(s.Renew(ctx, "r", "item", "alice", l.Generation, ttl))
	if r.Generation != l.Generation {
		t.Fatalf("renew changed the generation: %d -> %d", l.Generation, r.Generation)
	}
	if got := r.ExpiresAt.Sub(l.ExpiresAt); got != ttl {
		t.Fatalf("renew moved expiry by %s, want %s", got, ttl)
	}
	_, err := s.Claim(ctx, "r", "item", "bob", time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseHeld)
	if err := s.Complete(ctx, "r", "item", "alice", l.Generation, "ok"); err != nil {
		t.Fatalf("complete within the renewed lease: %v", err)
	}
}

func TestReleaseFreesTheItemAndRetiresTheGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := must(s.Claim(ctx, "r", "item", "alice", time.Minute))
	if err := s.Release(ctx, "r", "item", "alice", a.Generation); err != nil {
		t.Fatal(err)
	}
	b := must(s.Claim(ctx, "r", "item", "bob", time.Minute))
	if b.Generation != a.Generation+1 {
		t.Fatalf("generation after release and claim %d, want %d", b.Generation, a.Generation+1)
	}
	wantRefusal(t, s.Complete(ctx, "r", "item", "alice", a.Generation, "x"), ledger.ReasonStaleGeneration)
}

func TestExpireSweepsOnlyExpiredLeases(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.Claim(ctx, "r", "short", "alice", 100*time.Millisecond))
	must(s.Claim(ctx, "r", "long", "bob", time.Hour))
	time.Sleep(250 * time.Millisecond)

	st := must(s.Status(ctx, "r"))
	if len(st.Stale) != 1 || st.Stale[0].ID != "short" {
		t.Fatalf("stale before sweep: %+v", st.Stale)
	}
	if len(st.Leases) != 1 || st.Leases[0].ID != "long" {
		t.Fatalf("held leases before sweep: %+v", st.Leases)
	}

	expired := must(s.Expire(ctx, "r"))
	if len(expired) != 1 || expired[0].ID != "short" || expired[0].Holder != "alice" {
		t.Fatalf("expired: %+v", expired)
	}
	if it := must(s.Item(ctx, "r", "short")); it.Holder != "" {
		t.Fatalf("short still held after sweep: %+v", it)
	}
	if it := must(s.Item(ctx, "r", "long")); it.Holder != "bob" {
		t.Fatalf("long lost its holder: %+v", it)
	}
	if again := must(s.Expire(ctx, "r")); len(again) != 0 {
		t.Fatalf("second sweep expired %+v", again)
	}
}

func TestClaimRejectsNonPositiveOrSubMicrosecondTTL(t *testing.T) {
	s := newStore(t)
	for _, ttl := range []time.Duration{0, 500 * time.Nanosecond, -time.Second} {
		_, err := s.Claim(context.Background(), "r", "item", "alice", ttl)
		var inv *ledger.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("lease length %s: want invalid input, got %v", ttl, err)
		}
	}
}

func TestUnknownRepositoryIsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.Claim(context.Background(), "nope", "item", "alice", time.Minute)
	var nf *ledger.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want not found, got %v", err)
	}
}
