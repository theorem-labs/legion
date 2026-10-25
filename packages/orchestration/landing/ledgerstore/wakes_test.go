package ledgerstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/landing/ledgerstore"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// wakesFixture is a ledger with repositories r and other, and the wake source of r, listening.
func wakesFixture(t *testing.T) (*ledger.Store, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dsn := testpg.Database(t, "landing_wakes")
	store := must(ledger.Open(ctx, dsn))
	t.Cleanup(store.Close)
	must(store.EnsureRepo(ctx, "r", 3))
	must(store.EnsureRepo(ctx, "other", 3))
	wakes, err := ledgerstore.ReadyWakes(ctx, dsn, store, "r")
	if err != nil {
		t.Fatal(err)
	}
	return store, wakes
}

func woke(wakes <-chan struct{}, within time.Duration) bool {
	select {
	case <-wakes:
		return true
	case <-time.After(within):
		return false
	}
}

// A ready submission of the repository wakes the program once; an event of another kind or of another repository
// does not wake it.
func TestReadyWakesOnlyOnThisRepositorysReadyEvents(t *testing.T) {
	ctx := context.Background()
	store, wakes := wakesFixture(t)

	must(store.OpenMigration(ctx, ledger.OpenRequest{Repo: "r", ID: "M-1", Owner: "alice", BaseCommit: "b", Branch: "m1", Writes: []string{"a.lean"}}))
	must(store.OpenMigration(ctx, ledger.OpenRequest{Repo: "other", ID: "M-9", Owner: "bob", BaseCommit: "b", Branch: "m9", Writes: []string{"z.lean"}}))
	must(store.Ready(ctx, ledger.ReadyRequest{Repo: "other", ID: "M-9", Generation: 1, Head: "h9", Key: "k9"}))
	if woke(wakes, 500*time.Millisecond) {
		t.Fatal("an open of r or a ready of another repository woke the program")
	}

	must(store.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: "M-1", Generation: 1, Head: "h1", Key: "k1"}))
	if !woke(wakes, 5*time.Second) {
		t.Fatal("a ready submission of r did not wake the program")
	}
	if woke(wakes, 300*time.Millisecond) {
		t.Fatal("one ready submission woke the program twice")
	}
}

// Ready submissions that arrive before the program reads its wake coalesce into one pending wake, so a busy round
// is followed by exactly one more.
func TestReadyWakesCoalesceWhileTheProgramIsBusy(t *testing.T) {
	ctx := context.Background()
	store, wakes := wakesFixture(t)
	for _, id := range []string{"M-1", "M-2", "M-3"} {
		must(store.OpenMigration(ctx, ledger.OpenRequest{Repo: "r", ID: id, Owner: "alice", BaseCommit: "b", Branch: id, Writes: []string{id + ".lean"}}))
		must(store.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: id, Generation: 1, Head: "h-" + id, Key: "k-" + id}))
	}
	time.Sleep(time.Second)
	if !woke(wakes, time.Second) {
		t.Fatal("three ready submissions left no wake")
	}
	if woke(wakes, 300*time.Millisecond) {
		t.Fatal("three ready submissions left more than one wake pending")
	}
}
