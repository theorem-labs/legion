package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	old := must(s.Claim(ctx, "r", "item", "alice", 20*time.Millisecond))
	_, err := s.Claim(ctx, "r", "item", "bob", time.Minute)
	wantRefusal(t, err, ledger.ReasonLeaseHeld)
	waitForDatabaseExpiry(t, observer, old.ExpiresAt)

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

func TestRenewMovesAndPersistsExpiryByRequestedLength(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	l := must(s.Claim(ctx, "r", "item", "alice", 20*time.Millisecond))
	ttl := 300 * time.Millisecond
	r := must(s.Renew(ctx, "r", "item", "alice", l.Generation, ttl))
	if r.Generation != l.Generation {
		t.Fatalf("renew changed the generation: %d -> %d", l.Generation, r.Generation)
	}
	if got := r.ExpiresAt.Sub(*l.ExpiresAt); got != ttl {
		t.Fatalf("renew moved expiry by %s, want %s", got, ttl)
	}
	waitForDatabaseExpiry(t, observer, l.ExpiresAt)
	item := must(s.Item(ctx, "r", "item"))
	if item.Holder != "alice" || item.ExpiresAt == nil || !item.ExpiresAt.Equal(*r.ExpiresAt) {
		t.Fatalf("renewed expiry was not persisted: %+v; want %s", item, r.ExpiresAt)
	}
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
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	short := must(s.Claim(ctx, "r", "short", "alice", 20*time.Millisecond))
	must(s.Claim(ctx, "r", "long", "bob", time.Hour))
	waitForDatabaseExpiry(t, observer, short.ExpiresAt)

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

func TestStatusReadsLeaseAndMigrationFromOneSnapshot(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(s.Claim(ctx, "r", "item", "alice", time.Hour))
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))

	locker := must(pgx.Connect(ctx, dsn))
	defer locker.Close(ctx)
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	lock := must(locker.Begin(ctx))
	locked := true
	defer func() {
		if locked {
			_ = lock.Rollback(ctx)
		}
	}()
	must(lock.Exec(ctx, `LOCK TABLE migration_writes IN ACCESS EXCLUSIVE MODE`))

	statuses := make(chan struct {
		status ledger.Status
		err    error
	}, 1)
	go func() {
		status, err := s.Status(ctx, "r")
		statuses <- struct {
			status ledger.Status
			err    error
		}{status, err}
	}()
	waitForBlockedConnections(t, observer, 1)

	changer := must(pgx.Connect(ctx, dsn))
	defer changer.Close(ctx)
	change := must(changer.Begin(ctx))
	must(change.Exec(ctx, `UPDATE work_items SET holder = NULL, lease_expires_at = NULL WHERE repo = $1 AND id = $2`, "r", "item"))
	must(change.Exec(ctx, `UPDATE migrations SET state = 'abandoned', closed_at = clock_timestamp(), close_reason = 'sweep'
		WHERE repo = $1 AND id = $2`, "r", "M-1"))
	if err := change.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	locked = false

	got := <-statuses
	if got.err != nil {
		t.Fatal(got.err)
	}
	before := len(got.status.Leases) == 1 && got.status.Leases[0].ID == "item" &&
		len(got.status.Migrations) == 1 && got.status.Migrations[0].ID == "M-1"
	after := len(got.status.Leases) == 0 && len(got.status.Migrations) == 0
	if !before && !after {
		t.Fatalf("torn status snapshot: %+v", got.status)
	}
}

func TestExpireLocksThenRechecksLeaseLiveness(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, s *ledger.Store, dsn string, lease ledger.Lease, lock pgx.Tx, observer *pgx.Conn)
	}{
		{
			name: "expires while sweep waits",
			run: func(t *testing.T, s *ledger.Store, dsn string, lease ledger.Lease, lock pgx.Tx, observer *pgx.Conn) {
				swept := make(chan struct {
					items []ledger.Item
					err   error
				}, 1)
				go func() {
					items, err := s.Expire(context.Background(), "r")
					swept <- struct {
						items []ledger.Item
						err   error
					}{items, err}
				}()
				waitForBlockedConnections(t, observer, 1)
				waitForDatabaseExpiry(t, observer, lease.ExpiresAt)
				if err := lock.Rollback(context.Background()); err != nil {
					t.Fatal(err)
				}
				got := <-swept
				if got.err != nil {
					t.Fatal(got.err)
				}
				if len(got.items) != 1 || got.items[0].ID != "item" || got.items[0].Holder != "alice" {
					t.Fatalf("swept items: %+v", got.items)
				}
			},
		},
		{
			name: "renewed before sweep gets row",
			run: func(t *testing.T, s *ledger.Store, dsn string, lease ledger.Lease, lock pgx.Tx, observer *pgx.Conn) {
				swept := make(chan struct {
					items []ledger.Item
					err   error
				}, 1)
				go func() {
					items, err := s.Expire(context.Background(), "r")
					swept <- struct {
						items []ledger.Item
						err   error
					}{items, err}
				}()
				waitForBlockedConnections(t, observer, 1)
				var renewed time.Time
				if err := lock.QueryRow(context.Background(), `UPDATE work_items
					SET lease_expires_at = clock_timestamp() + interval '1 minute'
					WHERE repo = $1 AND id = $2
					RETURNING lease_expires_at`, "r", "item").Scan(&renewed); err != nil {
					t.Fatal(err)
				}
				waitForDatabaseExpiry(t, observer, lease.ExpiresAt)
				if err := lock.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				got := <-swept
				if got.err != nil {
					t.Fatal(got.err)
				}
				if len(got.items) != 0 {
					t.Fatalf("sweep freed renewed lease: %+v", got.items)
				}
				item := must(s.Item(context.Background(), "r", "item"))
				if item.Holder != "alice" || item.ExpiresAt == nil || !item.ExpiresAt.Equal(renewed) {
					t.Fatalf("renewed item: %+v; want expiry %s", item, renewed)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, dsn := newStoreDSN(t)
			ctx := context.Background()
			lease := must(s.Claim(ctx, "r", "item", "alice", 20*time.Millisecond))
			locker := must(pgx.Connect(ctx, dsn))
			defer locker.Close(ctx)
			observer := must(pgx.Connect(ctx, dsn))
			defer observer.Close(ctx)
			lock := must(locker.Begin(ctx))
			locked := true
			defer func() {
				if locked {
					_ = lock.Rollback(ctx)
				}
			}()
			must(lock.Exec(ctx, `SELECT 1 FROM work_items WHERE repo = $1 AND id = $2 FOR UPDATE`, "r", "item"))
			test.run(t, s, dsn, lease, lock, observer)
			locked = false
		})
	}
}

// A lease length of zero is a supervised lease (TestSupervisedLeaseNeverExpires); a negative length,
// or one that rounds to no microsecond, is invalid.
func TestClaimRejectsNegativeOrSubMicrosecondTTL(t *testing.T) {
	s := newStore(t)
	for _, ttl := range []time.Duration{500 * time.Nanosecond, -time.Second} {
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

func TestLeaseLivenessIsEvaluatedAfterTheItemRowLock(t *testing.T) {
	tests := []struct {
		name    string
		operate func(context.Context, *ledger.Store, ledger.Lease) error
		success bool
	}{
		{
			name: "renew",
			operate: func(ctx context.Context, s *ledger.Store, lease ledger.Lease) error {
				_, err := s.Renew(ctx, "r", "item", "alice", lease.Generation, time.Minute)
				return err
			},
		},
		{
			name: "complete",
			operate: func(ctx context.Context, s *ledger.Store, lease ledger.Lease) error {
				return s.Complete(ctx, "r", "item", "alice", lease.Generation, "late")
			},
		},
		{
			name: "claim",
			operate: func(ctx context.Context, s *ledger.Store, lease ledger.Lease) error {
				_, err := s.Claim(ctx, "r", "item", "bob", time.Minute)
				return err
			},
			success: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, dsn := newStoreDSN(t)
			ctx := context.Background()
			lease := must(s.Claim(ctx, "r", "item", "alice", 100*time.Millisecond))
			locker := must(pgx.Connect(ctx, dsn))
			defer locker.Close(ctx)
			observer := must(pgx.Connect(ctx, dsn))
			defer observer.Close(ctx)
			lock := must(locker.Begin(ctx))
			must(lock.Exec(ctx, `SELECT 1 FROM work_items WHERE repo = $1 AND id = $2 FOR UPDATE`, "r", "item"))

			done := make(chan error, 1)
			go func() { done <- test.operate(ctx, s, lease) }()
			waitForBlockedConnections(t, observer, 1)
			waitForDatabaseExpiry(t, observer, lease.ExpiresAt)
			if err := lock.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			err := <-done
			if test.success {
				if err != nil {
					t.Fatal(err)
				}
				item := must(s.Item(ctx, "r", "item"))
				if item.Holder != "bob" || item.Generation != lease.Generation+1 {
					t.Fatalf("claim after expiry: %+v", item)
				}
				return
			}
			wantRefusal(t, err, ledger.ReasonLeaseExpired)
		})
	}
}

// waitForDatabaseExpiry waits until the database clock passes a fixed lease end; a lease with no end fails the test.
func waitForDatabaseExpiry(t *testing.T, conn *pgx.Conn, expiresAt *time.Time) {
	t.Helper()
	if expiresAt == nil {
		t.Fatal("waiting for the end of a lease that has none")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var expired bool
		if err := conn.QueryRow(context.Background(), `SELECT clock_timestamp() >= $1`, expiresAt).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for database lease expiry")
		case <-tick.C:
		}
	}
}
