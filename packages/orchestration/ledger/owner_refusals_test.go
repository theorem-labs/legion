package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

type ownerRefusedEvent struct {
	Owner    string `json:"owner"`
	Revision int64  `json:"revision"`
	Reason   string `json:"reason"`
}

// ownerRefusedEvents reads repository r's owner.refused events, each of which names its owner as its subject.
func ownerRefusedEvents(t *testing.T, s *ledger.Store) []ownerRefusedEvent {
	t.Helper()
	events := []ownerRefusedEvent{}
	for _, e := range must(s.Events(context.Background(), "r", 0, "")) {
		if e.Kind != "owner.refused" {
			continue
		}
		var payload ownerRefusedEvent
		if err := json.Unmarshal(e.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if e.Subject != payload.Owner {
			t.Fatalf("owner.refused event subject %q, want its owner %q", e.Subject, payload.Owner)
		}
		events = append(events, payload)
	}
	return events
}

func assertOwnerRefusals(t *testing.T, s *ledger.Store, want ...ledger.OwnerRefusal) {
	t.Helper()
	got := must(s.OwnerRefusals(context.Background(), "r"))
	if got == nil || len(got) != len(want) {
		t.Fatalf("owner refusals = %#v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Owner != want[i].Owner || got[i].Revision != want[i].Revision || got[i].Reason != want[i].Reason ||
			!got[i].At.Equal(want[i].At) {
			t.Fatalf("owner refusal %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func enqueueFor(t *testing.T, s *ledger.Store, id, owner string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := s.Enqueue(ctx, "r", id, owner); err != nil {
		t.Fatal(err)
	}
	return must(s.OwnerWorkRevision(ctx, "r", owner))
}

// The first refusal of an owner at a revision appends one owner.refused event; a repeat with the same reason returns
// the stored refusal and appends nothing.
func TestRecordOwnerRefusedAppendsOnePerRevision(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const owner = "function:get_mountpoint"
	revision := enqueueFor(t, s, "item-1", owner)

	first := must(s.RecordOwnerRefused(ctx, "r", owner, revision, "no template"))
	if first.Owner != owner || first.Revision != revision || first.Reason != "no template" || first.At.IsZero() {
		t.Fatalf("first refusal = %+v", first)
	}
	repeat := must(s.RecordOwnerRefused(ctx, "r", owner, revision, "no template"))
	if repeat.Owner != first.Owner || repeat.Revision != first.Revision || repeat.Reason != first.Reason || !repeat.At.Equal(first.At) {
		t.Fatalf("repeated refusal = %+v, want the stored %+v", repeat, first)
	}
	if got, want := ownerRefusedEvents(t, s), []ownerRefusedEvent{{owner, revision, "no template"}}; !slices.Equal(got, want) {
		t.Fatalf("owner.refused events = %+v, want %+v", got, want)
	}
	assertOwnerRefusals(t, s, first)
}

// A second refusal at the same revision with another reason is refused and changes nothing.
func TestRecordOwnerRefusedRefusesAnotherReasonAtTheSameRevision(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const owner = "struct:mount"
	revision := enqueueFor(t, s, "item-1", owner)
	first := must(s.RecordOwnerRefused(ctx, "r", owner, revision, "no template"))

	_, err := s.RecordOwnerRefused(ctx, "r", owner, revision, "owner id outside the accepted forms")
	wantRefusal(t, err, ledger.ReasonRefusalConflict)
	assertOwnerRefusals(t, s, first)
	if got := ownerRefusedEvents(t, s); len(got) != 1 {
		t.Fatalf("a conflicting refusal appended events: %+v", got)
	}
}

// OwnerRefusals shows an owner's refusal only while the owner's work revision is the refused one: the owner's new work
// or a completion hides it, a refusal at the new revision shows, and a claim or another owner's work changes nothing.
// A refusal at an older revision is recorded once and never shown.
func TestOwnerRefusalsShowOnlyTheRefusalOfTheCurrentWork(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const refused, other = "struct:mount", "function:a_b"
	assertOwnerRefusals(t, s)

	refusedRevision := enqueueFor(t, s, "1", refused)
	otherOld := enqueueFor(t, s, "2", other)
	first := must(s.RecordOwnerRefused(ctx, "r", refused, refusedRevision, "no template"))
	assertOwnerRefusals(t, s, first)
	enqueueFor(t, s, "3", other)
	assertOwnerRefusals(t, s, first)

	newer := enqueueFor(t, s, "4", refused)
	assertOwnerRefusals(t, s)
	second := must(s.RecordOwnerRefused(ctx, "r", refused, newer, "owner id outside the accepted forms"))
	assertOwnerRefusals(t, s, second)

	lease := must(s.Claim(ctx, "r", "4", refused, 0))
	assertOwnerRefusals(t, s, second)
	if err := s.Complete(ctx, "r", "4", refused, lease.Generation, "done"); err != nil {
		t.Fatal(err)
	}
	assertOwnerRefusals(t, s)

	older := must(s.RecordOwnerRefused(ctx, "r", other, otherOld, "no template"))
	if older.Revision != otherOld {
		t.Fatalf("older refusal = %+v, want revision %d", older, otherOld)
	}
	assertOwnerRefusals(t, s)
	want := []ownerRefusedEvent{
		{refused, refusedRevision, "no template"},
		{refused, newer, "owner id outside the accepted forms"},
		{other, otherOld, "no template"},
	}
	if got := ownerRefusedEvents(t, s); !slices.Equal(got, want) {
		t.Fatalf("owner.refused events = %+v, want %+v", got, want)
	}
}

// An empty owner or reason, a revision below 1, and a revision newer than the owner's work revision are invalid input
// and record nothing; the newer revision's error names the owner's current revision.
func TestRecordOwnerRefusedRefusesInvalidInput(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const owner = "struct:mount"
	current := enqueueFor(t, s, "item-1", owner)
	for _, test := range []struct {
		name, owner string
		revision    int64
		reason      string
		message     string
	}{
		{"empty owner", "", current, "no template", "empty owner"},
		{"blank owner", " \t", current, "no template", "empty owner"},
		{"empty reason", owner, current, "", "empty reason"},
		{"blank reason", owner, current, "  ", "empty reason"},
		{"zero revision", owner, 0, "no template", "revision 0"},
		{"negative revision", owner, -1, "no template", "revision -1"},
		{"newer revision", owner, current + 1, "no template", fmt.Sprintf("work revision %d", current)},
		{"owner without work", "global:idle", 1, "no template", "work revision 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.RecordOwnerRefused(ctx, "r", test.owner, test.revision, test.reason)
			var invalid *ledger.InvalidError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want invalid input naming %q, got %v", test.message, err)
			}
		})
	}
	if got := ownerRefusedEvents(t, s); len(got) != 0 {
		t.Fatalf("invalid refusals appended events: %+v", got)
	}
	assertOwnerRefusals(t, s)
	var notFound *ledger.NotFoundError
	if _, err := s.RecordOwnerRefused(ctx, "missing", owner, current, "no template"); !errors.As(err, &notFound) {
		t.Fatalf("refusal in a missing repository: %v", err)
	}
}

// TestRecordOwnerRefusedConcurrentRepeatAppendsOnce catches a repeat that looks for the stored refusal before the
// first call commits: it must wait for that row and return it, never insert a second refusal or fail.
func TestRecordOwnerRefusedConcurrentRepeatAppendsOnce(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	const owner = "struct:mount"
	revision := enqueueFor(t, s, "item-1", owner)

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
	// The first call stops at its event append, after it has inserted the refusal, until this lock is released.
	must(lock.Exec(ctx, `LOCK TABLE events IN SHARE MODE`))

	type recorded struct {
		refusal ledger.OwnerRefusal
		err     error
	}
	first := make(chan recorded, 1)
	go func() {
		refusal, err := s.RecordOwnerRefused(ctx, "r", owner, revision, "no template")
		first <- recorded{refusal, err}
	}()
	waitForBlockedConnections(t, observer, 1)
	repeat := make(chan recorded, 1)
	go func() {
		refusal, err := s.RecordOwnerRefused(ctx, "r", owner, revision, "no template")
		repeat <- recorded{refusal, err}
	}()
	waitForBlockedConnections(t, observer, 2)

	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	locked = false
	gotFirst, gotRepeat := <-first, <-repeat
	if gotFirst.err != nil || gotRepeat.err != nil {
		t.Fatalf("first call: %v; concurrent repeat: %v", gotFirst.err, gotRepeat.err)
	}
	if !gotRepeat.refusal.At.Equal(gotFirst.refusal.At) || gotRepeat.refusal.Reason != "no template" {
		t.Fatalf("concurrent repeat = %+v, want the stored %+v", gotRepeat.refusal, gotFirst.refusal)
	}
	if got := ownerRefusedEvents(t, s); len(got) != 1 {
		t.Fatalf("concurrent repeat appended %d owner.refused events, want 1", len(got))
	}
}
