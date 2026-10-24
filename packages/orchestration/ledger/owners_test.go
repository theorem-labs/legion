package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// A lease taken with no length is supervised: it has no end, no sweep frees it, and nobody else
// can take the item while it is held.
func TestSupervisedLeaseNeverExpires(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	lease := must(s.Claim(ctx, "r", "item", "function:get_mountpoint", 0))
	if lease.ExpiresAt != nil {
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

// HasOpenWork is true exactly while the owner has an open item: not before its first enqueue, not after its last
// item is done, and not because another owner has work. An empty owner is invalid input.
func TestHasOpenWorkFollowsTheOwnersOpenItems(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const owner, other = "struct:mount", "struct:state"
	hasWork := func(step string, want bool) {
		t.Helper()
		if got := must(s.HasOpenWork(ctx, "r", owner)); got != want {
			t.Fatalf("%s: HasOpenWork = %t, want %t", step, got, want)
		}
	}
	hasWork("before any enqueue", false)
	if err := s.Enqueue(ctx, "r", "other-item", other); err != nil {
		t.Fatal(err)
	}
	hasWork("after another owner's enqueue", false)
	if err := s.Enqueue(ctx, "r", "item", owner); err != nil {
		t.Fatal(err)
	}
	hasWork("after its enqueue", true)
	lease := must(s.Claim(ctx, "r", "item", owner, 0))
	hasWork("while its item is held", true)
	if err := s.Complete(ctx, "r", "item", owner, lease.Generation, "proved"); err != nil {
		t.Fatal(err)
	}
	hasWork("after its only item is done", false)
	var invalid *ledger.InvalidError
	if _, err := s.HasOpenWork(ctx, "r", " "); !errors.As(err, &invalid) {
		t.Fatalf("blank owner: %v, want invalid input", err)
	}
}

// EnsureRepoWithUpstreamOwner sets the landing-round limit and the upstream owner in one change, which one
// repository.configured event records with both values; an empty owner clears it. EnsureRepo leaves the upstream
// owner as it is, and a call that changes nothing appends no event.
func TestEnsureRepoConfiguresTheUpstreamOwnerInOneChange(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	type configured struct {
		MaxLandingRounds int    `json:"max_landing_rounds"`
		UpstreamOwner    string `json:"upstream_owner"`
	}
	var seen []configured
	step := func(name string, repository ledger.Repository, rounds int, owner string, events ...configured) {
		t.Helper()
		if repository.Name != "u" || repository.MaxLandingRounds != rounds || repository.UpstreamOwner != owner {
			t.Fatalf("%s: repository %+v, want %d rounds and upstream owner %q", name, repository, rounds, owner)
		}
		seen = append(seen, events...)
		var got []configured
		for _, event := range must(s.Events(ctx, "u", 0, "u")) {
			var data configured
			if err := json.Unmarshal(event.Data, &data); err != nil || event.Kind != "repository.configured" {
				t.Fatalf("%s: event %+v: %v", name, event, err)
			}
			got = append(got, data)
		}
		if !slices.Equal(got, seen) {
			t.Fatalf("%s: repository.configured events %+v, want %+v", name, got, seen)
		}
	}
	step("create with an upstream owner", must(s.EnsureRepoWithUpstreamOwner(ctx, "u", 2, "upstream")), 2, "upstream",
		configured{2, "upstream"})
	step("ensure keeps the upstream owner", must(s.EnsureRepo(ctx, "u", 2)), 2, "upstream")
	step("the same upstream owner again", must(s.EnsureRepoWithUpstreamOwner(ctx, "u", 2, "upstream")), 2, "upstream")
	step("a new limit keeps the upstream owner", must(s.EnsureRepo(ctx, "u", 3)), 3, "upstream", configured{3, "upstream"})
	step("clear the upstream owner", must(s.EnsureRepoWithUpstreamOwner(ctx, "u", 3, "")), 3, "", configured{3, ""})
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

// queued is the open item id of owner as Enqueue leaves it: no holder, generation 0, no lease end.
func queued(id, owner string) ledger.Item {
	return ledger.Item{ID: id, Owner: owner, State: "open"}
}

// assertOpenItems reads owner's open items in repository r and compares them with want, field by field.
func assertOpenItems(t *testing.T, s *ledger.Store, owner string, want ...ledger.Item) {
	t.Helper()
	got := must(s.OpenItems(context.Background(), "r", owner))
	gotJSON, wantJSON := must(json.Marshal(got)), must(json.Marshal(want))
	if got == nil || len(got) != len(want) {
		t.Fatalf("open items of %s = %s, want %s", owner, gotJSON, wantJSON)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Owner != w.Owner || g.State != w.State || g.Holder != w.Holder || g.Generation != w.Generation ||
			g.Result != w.Result || (g.ExpiresAt == nil) != (w.ExpiresAt == nil) ||
			(g.ExpiresAt != nil && !g.ExpiresAt.Equal(*w.ExpiresAt)) {
			t.Fatalf("open item %d of %s differs: got %s, want %s", i, owner, gotJSON, wantJSON)
		}
	}
}

// OpenItems lists one owner's open items in repository r in enqueue order, whatever their ids: none of another
// owner's, none of another repository's, and no completed item. Each shows its holder and generation, and its lease
// end only for a fixed-end lease. An owner with no items has an empty list.
func TestOpenItemsListsOneOwnersOpenItemsInEnqueueOrder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const owner, other, holder = "struct:mount", "function:get_mountpoint", "session-1"
	assertOpenItems(t, s, owner)
	must(s.EnsureRepo(ctx, "elsewhere", 3))
	if err := s.Enqueue(ctx, "elsewhere", "e", owner); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, owner string }{{"c", owner}, {"x", other}, {"a", owner}, {"y", other}, {"b", owner}, {"d", owner}} {
		if err := s.Enqueue(ctx, "r", item.id, item.owner); err != nil {
			t.Fatal(err)
		}
	}
	assertOpenItems(t, s, owner, queued("c", owner), queued("a", owner), queued("b", owner), queued("d", owner))

	released := must(s.Claim(ctx, "r", "c", holder, time.Minute))
	if err := s.Release(ctx, "r", "c", holder, released.Generation); err != nil {
		t.Fatal(err)
	}
	must(s.Claim(ctx, "r", "a", holder, 0))
	first := must(s.Claim(ctx, "r", "b", holder, time.Minute))
	if err := s.Release(ctx, "r", "b", holder, first.Generation); err != nil {
		t.Fatal(err)
	}
	fixed := must(s.Claim(ctx, "r", "b", holder, time.Hour))
	done := must(s.Claim(ctx, "r", "d", holder, 0))
	if err := s.Complete(ctx, "r", "d", holder, done.Generation, "proved"); err != nil {
		t.Fatal(err)
	}

	free := queued("c", owner)
	free.Generation = 1
	supervised := queued("a", owner)
	supervised.Holder, supervised.Generation = holder, 1
	leased := queued("b", owner)
	leased.Holder, leased.Generation, leased.ExpiresAt = holder, 2, fixed.ExpiresAt
	assertOpenItems(t, s, owner, free, supervised, leased)
	assertOpenItems(t, s, other, queued("x", other), queued("y", other))
	assertOpenItems(t, s, "global:nobody")
}

// Items enqueued at the same time are listed by id.
func TestOpenItemsBreaksEnqueueTimeTiesByID(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	const owner = "struct:mount"
	for _, id := range []string{"z2", "z1"} {
		if err := s.Enqueue(ctx, "r", id, owner); err != nil {
			t.Fatal(err)
		}
	}
	assertOpenItems(t, s, owner, queued("z2", owner), queued("z1", owner))
	db := must(pgx.Connect(ctx, dsn))
	defer db.Close(ctx)
	must(db.Exec(ctx, `UPDATE work_items SET created_at = (SELECT created_at FROM work_items WHERE repo = 'r' AND id = 'z2')
		WHERE repo = 'r' AND id = 'z1'`))
	assertOpenItems(t, s, owner, queued("z1", owner), queued("z2", owner))
}

// An empty owner is invalid input, and a repository that does not exist is not found.
func TestOpenItemsRefusesAnEmptyOwnerAndAMissingRepository(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, owner := range []string{"", "  "} {
		var invalid *ledger.InvalidError
		if _, err := s.OpenItems(ctx, "r", owner); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "empty owner") {
			t.Fatalf("open items of owner %q: want invalid input naming the empty owner, got %v", owner, err)
		}
	}
	var notFound *ledger.NotFoundError
	if _, err := s.OpenItems(ctx, "missing", "struct:mount"); !errors.As(err, &notFound) || notFound.What != "repository" {
		t.Fatalf("open items in a missing repository: want not found, got %v", err)
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

// TestReleaseHolderSerializesClaimsForStoppedHolder catches a new claim escaping the stopped-holder
// release after its work-item scan has started.
func TestReleaseHolderSerializesClaimsForStoppedHolder(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	const holder = "struct:mount"
	must(s.EnsureRepo(ctx, "held", 3))
	if err := s.Enqueue(ctx, "held", "held", holder); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, "held", "available", holder); err != nil {
		t.Fatal(err)
	}
	must(s.Claim(ctx, "held", "held", holder, 0))
	cursor := must(s.LatestEventID(ctx, "held"))

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
	must(lock.Exec(ctx, `SELECT 1 FROM work_items WHERE repo = $1 AND id = $2 FOR UPDATE`, "held", "held"))

	type released struct {
		items []ledger.Item
		err   error
	}
	releasing := make(chan released, 1)
	go func() {
		items, err := s.ReleaseHolder(ctx, "held", holder, "process stopped")
		releasing <- released{items, err}
	}()
	waitForBlockedConnections(t, observer, 1)

	type claimed struct {
		lease ledger.Lease
		err   error
	}
	claiming := make(chan claimed, 1)
	go func() {
		lease, err := s.Claim(ctx, "held", "available", holder, 0)
		claiming <- claimed{lease, err}
	}()
	// The release owns the repository lock before its work-item query waits, so this claim is
	// blocked too. It can run only after the release's holder_stopped event commits.
	waitForBlockedConnections(t, observer, 2)

	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	locked = false
	gotRelease := <-releasing
	if gotRelease.err != nil {
		t.Fatal(gotRelease.err)
	}
	if len(gotRelease.items) != 1 || gotRelease.items[0].ID != "held" {
		t.Fatalf("released items = %+v, want held", gotRelease.items)
	}
	gotClaim := <-claiming
	if gotClaim.err != nil || gotClaim.lease.ID != "available" || gotClaim.lease.Holder != holder {
		t.Fatalf("claim after holder release = %+v, %v", gotClaim.lease, gotClaim.err)
	}

	events := must(s.Events(ctx, "held", cursor, ""))
	if len(events) != 2 || events[0].Kind != "item.holder_stopped" || events[0].Subject != "held" ||
		events[1].Kind != "item.claimed" || events[1].Subject != "available" {
		t.Fatalf("events after holder release = %+v, want holder_stopped then claimed", events)
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
