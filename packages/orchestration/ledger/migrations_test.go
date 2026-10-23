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

func open(t *testing.T, s *ledger.Store, id, owner string, writes []string, reads map[string]string) (ledger.Migration, error) {
	t.Helper()
	return s.OpenMigration(context.Background(), ledger.OpenRequest{
		Repo: "r", ID: id, Owner: owner, BaseCommit: "base0", Branch: "mig/" + id, Writes: writes, Reads: reads,
	})
}

func TestOpenRefusesAWriteInsideAnOpenMigrationsDirectory(t *testing.T) {
	s := newStore(t)
	must(open(t, s, "M-dir", "alice", []string{"src/lib/"}, nil))
	_, err := open(t, s, "M-file", "bob", []string{"docs/x.md", "src/lib/deep/x.lean"}, nil)
	r := wantRefusal(t, err, ledger.ReasonOverlap)
	if r.Other != "M-dir" || len(r.Overlaps) != 1 || r.Overlaps[0] != (ledger.PathClash{Ours: "src/lib/deep/x.lean", Theirs: "src/lib/"}) {
		t.Fatalf("refusal should name M-dir and the clash: %+v", r)
	}
}

func TestOpenRefusesADirectoryContainingAnOpenMigrationsFile(t *testing.T) {
	s := newStore(t)
	must(open(t, s, "M-file", "alice", []string{"src/lib/x.lean"}, nil))
	_, err := open(t, s, "M-dir", "bob", []string{"src/"}, nil)
	r := wantRefusal(t, err, ledger.ReasonOverlap)
	if r.Other != "M-file" || len(r.Overlaps) != 1 || r.Overlaps[0] != (ledger.PathClash{Ours: "src/", Theirs: "src/lib/x.lean"}) {
		t.Fatalf("refusal should name M-file and the clash: %+v", r)
	}
	// Nested directories clash in both orders, and an identical file clashes.
	_, err = open(t, s, "M-same", "carol", []string{"src/lib/x.lean"}, nil)
	wantRefusal(t, err, ledger.ReasonOverlap)
	must(open(t, s, "M-d2", "dan", []string{"a/b/"}, nil))
	_, err = open(t, s, "M-d3", "erin", []string{"a/b/c/"}, nil)
	wantRefusal(t, err, ledger.ReasonOverlap)
	_, err = open(t, s, "M-d4", "erin", []string{"a/"}, nil)
	wantRefusal(t, err, ledger.ReasonOverlap)
}

func TestOpenAcceptsDisjointWriteSets(t *testing.T) {
	s := newStore(t)
	must(open(t, s, "M-1", "alice", []string{"src/lib/"}, nil))
	// A shared string prefix is not a shared directory.
	must(open(t, s, "M-2", "bob", []string{"src/library.lean", "src/li/"}, nil))
	must(open(t, s, "M-3", "carol", []string{"src/lib.lean", "docs/"}, nil))
	st := must(s.Status(context.Background(), "r"))
	if len(st.Migrations) != 3 {
		t.Fatalf("open migrations %d, want 3", len(st.Migrations))
	}
}

func TestConcurrentOverlappingOpensGrantOneLock(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	start := make(chan struct{})
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Every pair overlaps: each writes the shared file, half of them through a directory.
			w := []string{"shared/f.lean", fmt.Sprintf("own/%d.lean", i)}
			if i%2 == 0 {
				w = []string{"shared/", fmt.Sprintf("own/%d.lean", i)}
			}
			_, err := open(t, s, fmt.Sprintf("M-%d", i), fmt.Sprintf("o%d", i), w, nil)
			mu.Lock()
			defer mu.Unlock()
			var r *ledger.Refusal
			switch {
			case err == nil:
				won++
			case errors.As(err, &r) && r.Reason == ledger.ReasonOverlap:
				lost++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 || lost != 19 {
		t.Fatalf("won %d lost %d; want 1 and 19", won, lost)
	}
}

func TestOpenRefusesAnExistingNameAndBadPaths(t *testing.T) {
	s := newStore(t)
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	_, err := open(t, s, "M-1", "bob", []string{"b.lean"}, nil)
	wantRefusal(t, err, ledger.ReasonExists)
	for _, bad := range [][]string{{"../escape"}, {"/abs/path"}, {""}, {"a/../../b"}, {}, {"."}} {
		_, err := open(t, s, "M-bad", "bob", bad, nil)
		var inv *ledger.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("writes %q: want invalid, got %v", bad, err)
		}
	}
}

func TestTakeoverRefusesTheOldOwnersGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"X.foo": "h1"}))
	if m.Generation != 1 {
		t.Fatalf("new migration generation %d, want 1", m.Generation)
	}
	m2 := must(s.TakeOver(ctx, "r", "M-1", "bob", 1))
	if m2.Generation != 2 || m2.Owner != "bob" {
		t.Fatalf("after takeover: %+v", m2)
	}
	// A second replacement that read generation 1 loses the race.
	_, err := s.TakeOver(ctx, "r", "M-1", "carol", 1)
	wantRefusal(t, err, ledger.ReasonStaleGeneration)

	res := must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1,
		Changed: []string{"a.lean"}, Current: map[string]string{"X.foo": "h1"}}))
	if res.Pass || !hasViolation(res, ledger.ViolationStaleGeneration) {
		t.Fatalf("old generation check: %+v", res)
	}
	_, err = s.RecordRound(ctx, "r", "M-1", 1, "kicked")
	wantRefusal(t, err, ledger.ReasonStaleGeneration)
	_, err = s.ExtendMigration(ctx, "r", "M-1", 1, []string{"b.lean"}, nil)
	wantRefusal(t, err, ledger.ReasonStaleGeneration)
	_, err = s.Abandon(ctx, "r", "M-1", 1, "")
	wantRefusal(t, err, ledger.ReasonStaleGeneration)

	res = must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 2,
		Changed: []string{"a.lean"}, Current: map[string]string{"X.foo": "h1"}}))
	if !res.Pass {
		t.Fatalf("current generation check: %+v", res)
	}
	must(s.Abandon(ctx, "r", "M-1", 2, "done"))
}

func TestCheckNeverPassesOldGenerationWhenTakeoverIsQueued(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
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
	must(lock.Exec(ctx, `LOCK TABLE events IN SHARE MODE`))
	must(lock.Exec(ctx, `SELECT 1 FROM migrations WHERE repo = $1 AND id = $2 FOR UPDATE`, "r", "M-1"))

	takeover := make(chan error, 1)
	go func() {
		_, err := s.TakeOver(ctx, "r", "M-1", "bob", 1)
		takeover <- err
	}()
	waitForBlockedConnections(t, observer, 1)

	type checkOutcome struct {
		result ledger.CheckResult
		err    error
	}
	checked := make(chan checkOutcome, 1)
	go func() {
		result, err := s.Check(ctx, ledger.CheckRequest{
			Repo: "r", ID: "M-1", Generation: 1, Changed: []string{"a.lean"},
		})
		checked <- checkOutcome{result: result, err: err}
	}()
	waitForBlockedConnections(t, observer, 2)

	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-takeover; err != nil {
		t.Fatal(err)
	}
	got := <-checked
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.result.Pass || !hasViolation(got.result, ledger.ViolationStaleGeneration) {
		t.Fatalf("check passed an old generation after takeover: %+v", got.result)
	}
}

func waitForBlockedConnections(t *testing.T, conn *pgx.Conn, least int64) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var n int64
		if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= least {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d blocked database connections", least)
		case <-tick.C:
		}
	}
}

func hasViolation(res ledger.CheckResult, kind string) bool {
	for _, v := range res.Violations {
		if v.Kind == kind {
			return true
		}
	}
	return false
}

func violation(t *testing.T, res ledger.CheckResult, kind string) ledger.Violation {
	t.Helper()
	for _, v := range res.Violations {
		if v.Kind == kind {
			return v
		}
	}
	t.Fatalf("no %s violation in %+v", kind, res)
	return ledger.Violation{}
}

func TestCheckFlagsUndeclaredWritesAndChangedReadsAndPassesACleanChange(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"src/a.lean", "src/new/"}, map[string]string{"X.foo": "h1", "X.bar": "h2"}))
	check := func(changed []string, current map[string]string) ledger.CheckResult {
		return must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1, Changed: changed, Current: current}))
	}
	clean := map[string]string{"X.foo": "h1", "X.bar": "h2"}

	if res := check([]string{"src/a.lean", "./src/new/b/c.lean"}, clean); !res.Pass || len(res.Violations) != 0 {
		t.Fatalf("clean change refused: %+v", res)
	}
	res := check([]string{"src/a.lean", "src/newer.lean", "src/new"}, clean)
	if res.Pass {
		t.Fatal("undeclared write passed")
	}
	got := map[string]bool{}
	for _, v := range res.Violations {
		if v.Kind == ledger.ViolationUndeclaredWrite {
			got[v.Path] = true
		}
	}
	if len(got) != 2 || !got["src/newer.lean"] || !got["src/new"] {
		t.Fatalf("undeclared writes %v, want src/newer.lean and src/new", got)
	}

	res = check([]string{"src/a.lean"}, map[string]string{"X.foo": "h1", "X.bar": "h3"})
	v := violation(t, res, ledger.ViolationStaleRead)
	if res.Pass || v.Name != "X.bar" || v.Recorded != "h2" || v.Current != "h3" {
		t.Fatalf("changed read: %+v", res)
	}
	// A read that no longer exists is stale; a read nobody hashed cannot be decided, so it does not pass.
	res = check([]string{"src/a.lean"}, map[string]string{"X.foo": "h1", "X.bar": ""})
	if res.Pass || violation(t, res, ledger.ViolationStaleRead).Name != "X.bar" {
		t.Fatalf("vanished read: %+v", res)
	}
	res = check([]string{"src/a.lean"}, map[string]string{"X.foo": "h1"})
	if res.Pass || violation(t, res, ledger.ViolationUnverifiedRead).Name != "X.bar" {
		t.Fatalf("unhashed read: %+v", res)
	}
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	must(conn.Exec(ctx, `INSERT INTO migrations (repo, id, owner) VALUES ($1, $2, $3)`, "r", "M-overlap", "bob"))
	must(conn.Exec(ctx, `INSERT INTO migration_writes (repo, migration, path, is_prefix) VALUES ($1, $2, $3, false)`,
		"r", "M-overlap", "src/a.lean"))
	res = check([]string{"src/a.lean"}, clean)
	if res.Pass || !hasViolation(res, ledger.ViolationOverlap) {
		t.Fatalf("overlapping migration passed: %+v", res)
	}

}

func TestCheckOfAClosedOrUnknownMigration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(s.Abandon(ctx, "r", "M-1", 1, ""))
	res := must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1, Changed: []string{"a.lean"}}))
	if res.Pass || !hasViolation(res, ledger.ViolationNotOpen) {
		t.Fatalf("closed migration: %+v", res)
	}
	_, err := s.Abandon(ctx, "r", "M-1", 1, "")
	wantRefusal(t, err, ledger.ReasonNotOpen)
	_, err = s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-nope", Generation: 1})
	var nf *ledger.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("unknown migration: %v", err)
	}
}

func TestAbandonReleasesEveryPathSoABlockedOpenSucceeds(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-big", "alice", []string{"a/", "b/x.lean", "c/y.lean"}, nil))
	for _, w := range [][]string{{"a/z.lean"}, {"b/"}, {"c/y.lean"}} {
		_, err := open(t, s, "M-small", "bob", w, nil)
		wantRefusal(t, err, ledger.ReasonOverlap)
	}
	m := must(s.Abandon(ctx, "r", "M-big", 1, "split into smaller migrations"))
	if m.State != ledger.StateAbandoned || m.ClosedAt == nil {
		t.Fatalf("after abandon: %+v", m)
	}
	must(open(t, s, "M-small", "bob", []string{"a/z.lean", "b/", "c/y.lean"}, nil))
	got := must(s.Migration(ctx, "r", "M-big"))
	if len(got.Writes) != 3 {
		t.Fatalf("an abandoned migration keeps its declared writes as history: %+v", got.Writes)
	}
}

func TestOpenAndCheckRejectPathsWithLeadingOrTrailingWhitespace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, declared := range []string{" leading.lean", "trailing.lean ", " directory/file.lean"} {
		_, err := open(t, s, "M-"+declared, "alice", []string{declared}, nil)
		var invalid *ledger.InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("declared path %q: want invalid, got %v", declared, err)
		}
	}
	must(open(t, s, "M-good", "alice", []string{"owned.lean"}, nil))
	for _, candidate := range []string{" owned.lean", "owned.lean "} {
		_, err := s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-good", Generation: 1, Changed: []string{candidate}})
		var invalid *ledger.InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("candidate path %q: want invalid, got %v", candidate, err)
		}
	}
}

func TestOpenAndExtendRejectReadNamesWithLeadingOrTrailingWhitespace(t *testing.T) {
	for _, name := range []string{" a.lean", "a.lean ", " X.declaration"} {
		t.Run("open "+name, func(t *testing.T) {
			s := newStore(t)
			_, err := open(t, s, "M-open", "alice", []string{"owned.lean"}, map[string]string{name: "hash"})
			var invalid *ledger.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("open read %q: want invalid, got %v", name, err)
			}
		})
	}
	t.Run("extend", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		migration := must(open(t, s, "M-extend", "alice", []string{"owned.lean"}, nil))
		for _, name := range []string{" b.lean", "b.lean "} {
			_, err := s.ExtendMigration(ctx, "r", migration.ID, migration.Generation, nil, map[string]string{name: "hash"})
			var invalid *ledger.InvalidError
			if !errors.As(err, &invalid) {
				t.Errorf("extend read %q: want invalid, got %v", name, err)
			}
		}
	})
}

func TestExtendAddsPathsUnlessTheyOverlap(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"X.a": "1"}))
	must(open(t, s, "M-2", "bob", []string{"lib/"}, nil))
	_, err := s.ExtendMigration(ctx, "r", "M-1", 1, []string{"b.lean", "lib/c.lean"}, nil)
	r := wantRefusal(t, err, ledger.ReasonOverlap)
	if r.Other != "M-2" {
		t.Fatalf("refusal names %q, want M-2", r.Other)
	}
	if m := must(s.Migration(ctx, "r", "M-1")); len(m.Writes) != 1 {
		t.Fatalf("a refused extend changed the write set: %v", m.Writes)
	}
	m := must(s.ExtendMigration(ctx, "r", "M-1", 1, []string{"b.lean"}, map[string]string{"X.b": "2"}))
	if len(m.Writes) != 2 || m.Reads["X.b"] != "2" || m.Reads["X.a"] != "1" {
		t.Fatalf("after extend: %+v", m)
	}
	res := must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1,
		Changed: []string{"a.lean", "b.lean"}, Current: map[string]string{"X.a": "1", "X.b": "2"}}))
	if !res.Pass {
		t.Fatalf("check after extend: %+v", res)
	}
}

func TestMigrationEscalatesWhenItUsesUpItsLandingRounds(t *testing.T) {
	s := newStore(t) // three rounds allowed
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(open(t, s, "M-2", "bob", []string{"b.lean"}, nil))
	for i := 1; i <= 2; i++ {
		m := must(s.RecordRound(ctx, "r", "M-1", 1, fmt.Sprintf("build failed %d", i)))
		if m.LandingRounds != i || m.EscalatedAt != nil {
			t.Fatalf("after round %d: %+v", i, m)
		}
	}
	_, err := s.Escalate(ctx, "r", "M-1", 3)
	wantRefusal(t, err, ledger.ReasonBelowLimit)

	m := must(s.RecordRound(ctx, "r", "M-1", 1, "build failed 3"))
	if m.EscalatedAt == nil || m.State != ledger.StateOpen {
		t.Fatalf("third failed round should escalate and leave it open: %+v", m)
	}
	st := must(s.Status(ctx, "r"))
	if len(st.Escalated) != 1 || st.Escalated[0].ID != "M-1" {
		t.Fatalf("status escalated: %+v", st.Escalated)
	}
	res := must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1, Changed: []string{"a.lean"}}))
	if res.Pass || !hasViolation(res, ledger.ViolationEscalated) {
		t.Fatalf("an escalated migration must wait for the steward: %+v", res)
	}

	m = must(s.Retry(ctx, "r", "M-1"))
	if m.EscalatedAt != nil || m.LandingRounds != 0 {
		t.Fatalf("after retry: %+v", m)
	}
	if res := must(s.Check(ctx, ledger.CheckRequest{Repo: "r", ID: "M-1", Generation: 1, Changed: []string{"a.lean"}})); !res.Pass {
		t.Fatalf("after retry: %+v", res)
	}
	_, err = s.Retry(ctx, "r", "M-1")
	wantRefusal(t, err, ledger.ReasonNotEscalated)

	// A sweep with a lower limit flags every open migration at or over it, once.
	must(s.RecordRound(ctx, "r", "M-2", 1, "kicked"))
	flagged := must(s.Escalate(ctx, "r", "", 1))
	if len(flagged) != 1 || flagged[0].ID != "M-2" {
		t.Fatalf("sweep flagged %+v, want M-2", flagged)
	}
	if again := must(s.Escalate(ctx, "r", "", 1)); len(again) != 0 {
		t.Fatalf("second sweep flagged %+v", again)
	}
}

func TestEventLogRecordsChangesAndRefusesEdits(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, nil))
	must(s.TakeOver(ctx, "r", "M-1", "bob", 1))
	must(s.Abandon(ctx, "r", "M-1", 2, "x"))
	evs := must(s.Events(ctx, "r", 0, "M-1"))
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"migration.opened", "migration.taken_over", "migration.abandoned"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	for _, q := range []string{`UPDATE events SET actor = 'x'`, `DELETE FROM events`, `TRUNCATE events`} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("%s succeeded on the event log", q)
		}
	}
}

func TestEventCursorDoesNotSkipAcrossOutOfOrderCommits(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(open(t, s, "M-a", "alice", []string{"a.lean"}, nil))
	must(open(t, s, "M-b", "bob", []string{"b.lean"}, nil))

	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	must(observer.Exec(ctx, `CREATE FUNCTION block_event_a() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.subject = 'M-a' AND NEW.kind = 'migration.taken_over' THEN
				PERFORM pg_advisory_xact_lock(730000001);
			END IF;
			RETURN NEW;
		END
	$$`))
	must(observer.Exec(ctx, `CREATE TRIGGER block_event_a AFTER INSERT ON events
		FOR EACH ROW EXECUTE FUNCTION block_event_a()`))

	blocker := must(pgx.Connect(ctx, dsn))
	defer blocker.Close(ctx)
	block := must(blocker.Begin(ctx))
	blocked := true
	defer func() {
		if blocked {
			_ = block.Rollback(ctx)
		}
	}()
	must(block.Exec(ctx, `SELECT pg_advisory_xact_lock(730000001)`))

	first := make(chan error, 1)
	go func() {
		_, err := s.TakeOver(ctx, "r", "M-a", "carol", 1)
		first <- err
	}()
	waitForBlockedConnections(t, observer, 1)

	second := make(chan error, 1)
	go func() {
		_, err := s.TakeOver(ctx, "r", "M-b", "dave", 1)
		second <- err
	}()
	waitForBlockedConnections(t, observer, 2)
	inFlight := must(s.Events(ctx, "r", 0, ""))
	if len(inFlight) < 2 {
		t.Fatalf("reader while writers blocked saw %+v, want the committed setup events", inFlight)
	}
	for _, event := range inFlight {
		if event.Kind == "migration.taken_over" {
			t.Fatalf("reader while writers blocked saw an unblocked writer event: %+v", event)
		}
	}
	readerCursor := inFlight[len(inFlight)-1].ID

	if err := block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	blocked = false
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}

	var a, b ledger.Event
	for _, event := range must(s.Events(ctx, "r", 0, "")) {
		if event.Kind != "migration.taken_over" {
			continue
		}
		switch event.Subject {
		case "M-a":
			a = event
		case "M-b":
			b = event
		}
	}
	if a.ID == 0 || b.ID == 0 {
		t.Fatalf("takeover events missing: A=%+v B=%+v", a, b)
	}
	if a.ID >= b.ID {
		t.Fatalf("event IDs are not commit-ordered: A=%d B=%d", a.ID, b.ID)
	}
	readerContinuation := must(s.Events(ctx, "r", readerCursor, ""))
	if len(readerContinuation) != 2 {
		t.Fatalf("reader continuation after %d: %+v", readerCursor, readerContinuation)
	}
	if readerContinuation[0].ID != a.ID || readerContinuation[1].ID != b.ID {
		t.Fatalf("reader continuation after %d: %+v; want A=%d then B=%d", readerCursor, readerContinuation, a.ID, b.ID)
	}
	after := must(s.Events(ctx, "r", a.ID, ""))
	if len(after) != 1 || after[0].ID != b.ID {
		t.Fatalf("events after A=%d: %+v; want B=%d", a.ID, after, b.ID)
	}
}
