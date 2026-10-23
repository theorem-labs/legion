package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// landRequest is a candidate for migration m. A statement change in it carries an agent's review at its head, so a test
// of another rule is not refused for want of one.
func landRequest(m ledger.Migration, commit string, paths []string, current, changes map[string]string, approved []string) ledger.LandRequest {
	request := ledger.LandRequest{
		Repo:             "r",
		Migration:        m.ID,
		Generation:       m.Generation,
		Commit:           commit,
		ChangedPaths:     paths,
		Current:          current,
		ChangedContracts: changes,
		ApprovedPinned:   approved,
	}
	if len(changes) > 0 {
		request.Head, request.ReviewedHead = commit+"-head", commit+"-head"
	}
	return request
}

func TestRegisterContractAndReadBack(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	registered := must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", true))
	if registered.Name != "C" || registered.Owner != "alice" || registered.StatementHash != "hash-1" ||
		registered.Version != 1 || !registered.Pinned {
		t.Fatalf("registered contract: %+v", registered)
	}
	got := must(s.Contract(ctx, "r", "C"))
	if got != registered {
		t.Fatalf("read contract: %+v, want %+v", got, registered)
	}
	_, err := s.RegisterContract(ctx, "r", "C", "bob", "hash-2", false)
	wantRefusal(t, err, ledger.ReasonExists)
}

func TestTransferContractRequiresCurrentOwner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	registered := must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	transferred := must(s.TransferContract(ctx, "r", "C", "alice", "bob"))
	if transferred.Owner != "bob" || transferred.Version != registered.Version ||
		transferred.StatementHash != registered.StatementHash || transferred.Pinned != registered.Pinned {
		t.Fatalf("transferred contract: %+v", transferred)
	}
	_, err := s.TransferContract(ctx, "r", "C", "alice", "carol")
	wantRefusal(t, err, ledger.ReasonNotOwner)
}

func TestContractsListsTheRepositorysContractsAtTheirCurrentVersions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if got := must(s.Contracts(ctx, "r")); got == nil || len(got) != 0 {
		t.Fatalf("contracts of an empty repository: %#v, want an empty list", got)
	}
	must(s.EnsureRepo(ctx, "other", 0))
	must(s.RegisterContract(ctx, "other", "A.other", "carol", "hash-o", false))
	must(s.RegisterContract(ctx, "r", "Z.last", "alice", "hash-z1", true))
	must(s.RegisterContract(ctx, "r", "B.first", "bob", "hash-b1", false))
	m := must(open(t, s, "M-1", "bob", []string{"b.lean"}, map[string]string{"B.first": "hash-b1"}))
	must(s.Land(ctx, landRequest(m, "commit-1", []string{"b.lean"}, map[string]string{"B.first": "hash-b1"},
		map[string]string{"B.first": "hash-b2"}, nil)))

	got := must(s.Contracts(ctx, "r"))
	want := []ledger.Contract{
		must(s.Contract(ctx, "r", "B.first")),
		must(s.Contract(ctx, "r", "Z.last")),
	}
	if len(got) != len(want) {
		t.Fatalf("contracts: %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("contract %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if b := got[0]; b.Name != "B.first" || b.Owner != "bob" || b.StatementHash != "hash-b2" || b.Version != 2 || b.Pinned {
		t.Errorf("advanced contract: %+v", b)
	}
	if z := got[1]; z.Name != "Z.last" || z.Owner != "alice" || z.StatementHash != "hash-z1" || z.Version != 1 || !z.Pinned {
		t.Errorf("unchanged contract: %+v", z)
	}

	_, err := s.Contracts(ctx, "missing")
	var missing *ledger.NotFoundError
	if !errors.As(err, &missing) || missing.What != "repository" || missing.ID != "missing" {
		t.Fatalf("contracts of an unknown repository: want not found, got %v", err)
	}
}

func TestOpenAndExtendCaptureRegisteredContractVersions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "hash-d1", false))
	migration := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1", "external": "x"}))
	if got := migration.ReadVersions["C"]; got == nil || *got != 1 {
		t.Fatalf("recorded contract version: %v", got)
	}
	if got := migration.ReadVersions["external"]; got != nil {
		t.Fatalf("external read recorded version: %v", *got)
	}
	migration = must(s.ExtendMigration(ctx, "r", migration.ID, migration.Generation, []string{"b.lean"}, map[string]string{"D": "hash-d1"}))
	if got := migration.ReadVersions["D"]; got == nil || *got != 1 {
		t.Fatalf("extended contract version: %v", got)
	}
}

func TestRecordAgreementRefusesASupersededVersion(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	migration := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	must(s.Land(ctx, landRequest(migration, "commit-1", []string{"a.lean"}, map[string]string{"C": "hash-1"}, map[string]string{"C": "hash-2"}, nil)))
	_, err := s.RecordAgreement(ctx, "r", "A-old", []string{"alice"}, map[string]int64{"C": 1})
	wantRefusal(t, err, ledger.ReasonStaleVersion)
}

func TestLandAdvancesAContractAndStalesExactlyOlderAgreements(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "hash-d1", false))
	must(s.RecordAgreement(ctx, "r", "A-old", []string{"alice"}, map[string]int64{"C": 1}))
	must(s.RecordAgreement(ctx, "r", "A-unrelated", []string{"bob"}, map[string]int64{"D": 1}))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	landed := must(s.Land(ctx, landRequest(m, "commit-1", []string{"a.lean"}, map[string]string{"C": "hash-1"}, map[string]string{"C": "hash-2"}, nil)))
	if landed.State != ledger.StateLanded || landed.LandedCommit != "commit-1" {
		t.Fatalf("landed migration: %+v", landed)
	}
	c := must(s.Contract(ctx, "r", "C"))
	if c.StatementHash != "hash-2" || c.Version != 2 {
		t.Fatalf("landed contract: %+v", c)
	}
	stale := must(s.StaleAgreements(ctx, "r"))
	if len(stale) != 1 || stale[0].ID != "A-old" || stale[0].StaledAt == nil {
		t.Fatalf("stale agreements: %+v", stale)
	}
	must(s.RecordAgreement(ctx, "r", "A-current", []string{"bob"}, map[string]int64{"C": 2}))
	if stale = must(s.StaleAgreements(ctx, "r")); len(stale) != 1 || stale[0].ID != "A-old" {
		t.Fatalf("stale agreements after current read: %+v", stale)
	}

	advanced, staled := 0, 0
	for _, event := range must(s.Events(ctx, "r", 0, "")) {
		var data struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case "contract.advanced":
			if event.Subject != "C" || data.Version != 2 {
				t.Fatalf("unexpected contract advance: %+v", event)
			}
			advanced++
		case "agreement.staled":
			if event.Subject != "A-old" || data.Version != 2 {
				t.Fatalf("unexpected stale-agreement event: %+v", event)
			}
			staled++
		}
	}
	if advanced != 1 || staled != 1 {
		t.Fatalf("landing event counts: contract.advanced=%d agreement.staled=%d", advanced, staled)
	}
}

func TestLandRefusesAPinnedContractWithoutPersonApproval(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", true))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	_, err := s.Land(ctx, landRequest(m, "commit-1", []string{"a.lean"}, map[string]string{"C": "hash-1"}, map[string]string{"C": "hash-2"}, nil))
	r := wantRefusal(t, err, ledger.ReasonPinned)
	if !strings.Contains(r.Message, "C") || !strings.Contains(r.Message, "person must approve") {
		t.Fatalf("pinned refusal: %+v", r)
	}
	c := must(s.Contract(ctx, "r", "C"))
	if c.StatementHash != "hash-1" || c.Version != 1 {
		t.Fatalf("refused landing changed contract: %+v", c)
	}
	if got := must(s.Migration(ctx, "r", m.ID)); got.State != ledger.StateOpen {
		t.Fatalf("refused landing closed migration: %+v", got)
	}
}

func TestConcurrentLandsFromOneContractVersionGrantOne(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	first := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	second := must(open(t, s, "M-2", "bob", []string{"b.lean"}, map[string]string{"C": "hash-1"}))

	var wg sync.WaitGroup
	var mu sync.Mutex
	won, stale := 0, 0
	start := make(chan struct{})
	for _, attempt := range []struct {
		migration ledger.Migration
		hash      string
	}{
		{first, "hash-2"},
		{second, "hash-3"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Land(ctx, landRequest(attempt.migration, "commit-1", []string{attempt.migration.Writes[0]},
				map[string]string{"C": "hash-1"}, map[string]string{"C": attempt.hash}, nil))
			mu.Lock()
			defer mu.Unlock()
			var r *ledger.Refusal
			switch {
			case err == nil:
				won++
			case errors.As(err, &r) && r.Reason == ledger.ReasonStaleRead:
				stale++
			default:
				t.Errorf("unexpected landing error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won != 1 || stale != 1 {
		t.Fatalf("landings: won=%d stale=%d", won, stale)
	}
}

func TestLandRevalidatesCandidatePathsAndReadOnlyDependencies(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	reader := must(open(t, s, "M-reader", "alice", []string{"reader.lean"}, map[string]string{"C": "hash-1"}))
	writer := must(open(t, s, "M-writer", "bob", []string{"writer.lean"}, map[string]string{"C": "hash-1"}))
	must(s.Land(ctx, ledger.LandRequest{
		Repo:             "r",
		Migration:        writer.ID,
		Generation:       writer.Generation,
		Commit:           "writer-commit",
		ChangedPaths:     []string{"writer.lean"},
		Current:          map[string]string{"C": "hash-1"},
		ChangedContracts: map[string]string{"C": "hash-2"},
		Head:             "writer-head",
		ReviewedHead:     "writer-head",
	}))
	_, err := s.Land(ctx, ledger.LandRequest{
		Repo:         "r",
		Migration:    reader.ID,
		Generation:   reader.Generation,
		Commit:       "reader-commit",
		ChangedPaths: []string{"reader.lean"},
		Current:      map[string]string{"C": "hash-2"},
	})
	wantRefusal(t, err, ledger.ReasonStaleRead)

	plain := must(open(t, s, "M-plain", "carol", []string{"owned.lean"}, nil))
	_, err = s.Land(ctx, ledger.LandRequest{
		Repo:         "r",
		Migration:    plain.ID,
		Generation:   plain.Generation,
		Commit:       "plain-commit",
		ChangedPaths: []string{"outside.lean"},
		Current:      map[string]string{},
	})
	wantRefusal(t, err, ledger.ReasonUndeclaredWrite)

	external := must(open(t, s, "M-external", "carol", []string{"external.lean"}, map[string]string{"X.external": "h1"}))
	_, err = s.Land(ctx, ledger.LandRequest{
		Repo:         "r",
		Migration:    external.ID,
		Generation:   external.Generation,
		Commit:       "external-commit",
		ChangedPaths: []string{"external.lean"},
	})
	wantRefusal(t, err, ledger.ReasonUnverifiedRead)
	landed := must(s.Land(ctx, ledger.LandRequest{
		Repo:         "r",
		Migration:    external.ID,
		Generation:   external.Generation,
		Commit:       "external-commit",
		ChangedPaths: []string{"external.lean"},
		Current:      map[string]string{"X.external": "h1"},
	}))
	if landed.State != ledger.StateLanded {
		t.Fatalf("external read landing: %+v", landed)
	}
}

func TestLandRejectsContractVersionABAAndUndeclaredContract(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	old := must(open(t, s, "M-old", "alice", []string{"old.lean"}, map[string]string{"C": "hash-1"}))
	for _, change := range []struct {
		id, path, before, after string
	}{
		{"M-to-2", "to-2.lean", "hash-1", "hash-2"},
		{"M-back-1", "back-1.lean", "hash-2", "hash-1"},
	} {
		migration := must(open(t, s, change.id, "bob", []string{change.path}, map[string]string{"C": change.before}))
		must(s.Land(ctx, ledger.LandRequest{
			Repo:             "r",
			Migration:        migration.ID,
			Generation:       migration.Generation,
			Commit:           migration.ID + "-commit",
			ChangedPaths:     []string{change.path},
			Current:          map[string]string{"C": change.before},
			ChangedContracts: map[string]string{"C": change.after},
			Head:             migration.ID + "-head",
			ReviewedHead:     migration.ID + "-head",
		}))
	}
	_, err := s.Land(ctx, ledger.LandRequest{
		Repo:             "r",
		Migration:        old.ID,
		Generation:       old.Generation,
		Commit:           "old-commit",
		ChangedPaths:     []string{"old.lean"},
		Current:          map[string]string{"C": "hash-1"},
		ChangedContracts: map[string]string{"C": "hash-3"},
		Head:             "old-head",
		ReviewedHead:     "old-head",
	})
	wantRefusal(t, err, ledger.ReasonStaleRead)

	undeclared := must(open(t, s, "M-undeclared", "carol", []string{"undeclared.lean"}, nil))
	_, err = s.Land(ctx, ledger.LandRequest{
		Repo:             "r",
		Migration:        undeclared.ID,
		Generation:       undeclared.Generation,
		Commit:           "undeclared-commit",
		ChangedPaths:     []string{"undeclared.lean"},
		Current:          map[string]string{},
		ChangedContracts: map[string]string{"C": "hash-4"},
		Head:             "undeclared-head",
		ReviewedHead:     "undeclared-head",
	})
	wantRefusal(t, err, ledger.ReasonUndeclaredContract)
}

func TestConcurrentLandsFromOneVersionRejectsAnUnchangedHash(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	first := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	second := must(open(t, s, "M-2", "bob", []string{"b.lean"}, map[string]string{"C": "hash-1"}))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, migration := range []ledger.Migration{first, second} {
		go func() {
			<-start
			_, err := s.Land(ctx, landRequest(migration, migration.ID+"-commit", []string{migration.Writes[0]},
				map[string]string{"C": "hash-1"}, map[string]string{"C": "hash-1"}, nil))
			results <- err
		}()
	}
	close(start)
	successes, stale := 0, 0
	for range 2 {
		err := <-results
		var refusal *ledger.Refusal
		switch {
		case err == nil:
			successes++
		case errors.As(err, &refusal) && refusal.Reason == ledger.ReasonStaleRead:
			stale++
		default:
			t.Fatalf("unexpected landing error: %v", err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("unchanged-hash landings: successes=%d stale=%d", successes, stale)
	}
	if contract := must(s.Contract(ctx, "r", "C")); contract.Version != 2 {
		t.Fatalf("contract after unchanged-hash landing: %+v", contract)
	}
}

func TestLandRejectsInvalidCurrentHashForRegisteredRead(t *testing.T) {
	tests := []struct {
		name    string
		current map[string]string
		reason  string
	}{
		{name: "changed", current: map[string]string{"C": "hash-2"}, reason: ledger.ReasonStaleRead},
		{name: "vanished", current: map[string]string{"C": ""}, reason: ledger.ReasonStaleRead},
		{name: "missing", current: map[string]string{}, reason: ledger.ReasonUnverifiedRead},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
			migration := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
			_, err := s.Land(ctx, ledger.LandRequest{
				Repo:         "r",
				Migration:    migration.ID,
				Generation:   migration.Generation,
				Commit:       "commit-1",
				ChangedPaths: []string{"a.lean"},
				Current:      test.current,
			})
			wantRefusal(t, err, test.reason)
			if got := must(s.Migration(ctx, "r", migration.ID)); got.State != ledger.StateOpen {
				t.Fatalf("refused landing closed migration: %+v", got)
			}
			for _, event := range must(s.Events(ctx, "r", 0, "")) {
				switch event.Kind {
				case "migration.landed", "contract.advanced", "agreement.staled":
					t.Fatalf("refused landing appended %s: %+v", event.Kind, event)
				}
			}
		})
	}
}

func TestLandRequiresChangedPathsPresenceAndConsistency(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	missing := must(open(t, s, "M-missing", "alice", []string{"a.lean"}, nil))
	_, err := s.Land(ctx, ledger.LandRequest{
		Repo:       "r",
		Migration:  missing.ID,
		Generation: missing.Generation,
		Commit:     "missing-commit",
		Current:    map[string]string{},
	})
	wantRefusal(t, err, ledger.ReasonInvalidCandidate)
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	changed := must(open(t, s, "M-changed", "alice", []string{"b.lean"}, map[string]string{"C": "hash-1"}))
	_, err = s.Land(ctx, ledger.LandRequest{
		Repo:             "r",
		Migration:        changed.ID,
		Generation:       changed.Generation,
		Commit:           "changed-commit",
		ChangedPaths:     []string{},
		Current:          map[string]string{"C": "hash-1"},
		ChangedContracts: map[string]string{"C": "hash-2"},
	})
	wantRefusal(t, err, ledger.ReasonInvalidCandidate)
	empty := must(open(t, s, "M-empty", "alice", []string{"c.lean"}, nil))
	landed := must(s.Land(ctx, ledger.LandRequest{
		Repo:         "r",
		Migration:    empty.ID,
		Generation:   empty.Generation,
		Commit:       "empty-commit",
		ChangedPaths: []string{},
		Current:      map[string]string{},
	}))
	if landed.State != ledger.StateLanded {
		t.Fatalf("empty candidate did not land: %+v", landed)
	}
}

// statementLand lands M-1's change of contract C from hash-1 to hash-2 at head, with the given records.
func statementLand(m ledger.Migration, head, proof, reviewed string, approved []string) ledger.LandRequest {
	request := landRequest(m, "commit-1", []string{"a.lean"}, map[string]string{"C": "hash-1"}, map[string]string{"C": "hash-2"}, approved)
	request.Head, request.CompatibilityProof, request.ReviewedHead = head, proof, reviewed
	return request
}

// landedRecord is the statement record of a migration.landed event. A nil field is a key the event lacks.
type landedRecord struct {
	Head               *string `json:"head"`
	CompatibilityProof *string `json:"compatibility_proof"`
	ReviewedHead       *string `json:"reviewed_head"`
}

// wantLanded fails unless migration id has exactly one migration.landed event, and it carries these records.
func wantLanded(t *testing.T, s *ledger.Store, id, head, proof, reviewed string) {
	t.Helper()
	var landed []landedRecord
	for _, event := range must(s.Events(context.Background(), "r", 0, id)) {
		if event.Kind != "migration.landed" {
			continue
		}
		var record landedRecord
		if err := json.Unmarshal(event.Data, &record); err != nil {
			t.Fatal(err)
		}
		landed = append(landed, record)
	}
	if len(landed) != 1 {
		t.Fatalf("migration %s has %d migration.landed events, want 1", id, len(landed))
	}
	for _, field := range []struct {
		key  string
		got  *string
		want string
	}{
		{"head", landed[0].Head, head},
		{"compatibility_proof", landed[0].CompatibilityProof, proof},
		{"reviewed_head", landed[0].ReviewedHead, reviewed},
	} {
		if field.got == nil {
			t.Errorf("migration.landed has no %s", field.key)
		} else if *field.got != field.want {
			t.Errorf("migration.landed %s = %q, want %q", field.key, *field.got, field.want)
		}
	}
}

// wantUnchanged fails unless contract C is still at hash-1 and version 1, migration m is open, and the event log
// still has exactly events entries.
func wantUnchanged(t *testing.T, s *ledger.Store, m ledger.Migration, events int) {
	t.Helper()
	ctx := context.Background()
	if c := must(s.Contract(ctx, "r", "C")); c.StatementHash != "hash-1" || c.Version != 1 {
		t.Fatalf("refused landing changed contract: %+v", c)
	}
	if got := must(s.Migration(ctx, "r", m.ID)); got.State != ledger.StateOpen || got.LandedCommit != "" {
		t.Fatalf("refused landing closed migration: %+v", got)
	}
	if got := must(s.Events(ctx, "r", 0, "")); len(got) != events {
		t.Fatalf("refused landing appended %d events: %+v", len(got)-events, got[min(events, len(got)):])
	}
}

func TestLandOfProofsOnlyNeedsNoStatementRecord(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	landed := must(s.Land(ctx, landRequest(m, "commit-1", []string{"a.lean"}, map[string]string{"C": "hash-1"}, nil, nil)))
	if landed.State != ledger.StateLanded {
		t.Fatalf("proofs-only landing: %+v", landed)
	}
	wantLanded(t, s, m.ID, "", "", "")
}

func TestLandRefusesAStatementChangeWithNeitherRecord(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	before := len(must(s.Events(ctx, "r", 0, "")))
	_, err := s.Land(ctx, statementLand(m, "head-1", "", "", nil))
	r := wantRefusal(t, err, ledger.ReasonStatementUnreviewed)
	for _, want := range []string{"contract C", "compatibility proof", "review", "head-1"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("refusal %q does not name %q", r.Message, want)
		}
	}
	wantUnchanged(t, s, m, before)
}

// A refused statement change must be refused before Land writes anything, not written and then rolled back: the
// committed state alone cannot tell the two apart. Triggers in this test's own database raise on any write to the
// tables Land changes, and an approved, reviewed land of the same change proves that they are armed.
func TestRefusedStatementLandAttemptsNoWrite(t *testing.T) {
	s, dsn := newStoreDSN(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", true))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	before := len(must(s.Events(ctx, "r", 0, "")))

	db := must(pgx.Connect(ctx, dsn))
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`CREATE FUNCTION trap_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'write trap: %', TG_TABLE_NAME; END $$`)
	tables := []string{"contracts", "agreements", "migrations", "events"}
	for _, table := range tables {
		exec(`CREATE TRIGGER trap_write BEFORE INSERT OR UPDATE OR DELETE ON ` + table +
			` FOR EACH STATEMENT EXECUTE FUNCTION trap_write()`)
	}

	for _, refused := range []struct {
		name    string
		request ledger.LandRequest
		reason  string
	}{
		{"no approval", statementLand(m, "head-1", "", "head-1", nil), ledger.ReasonPinned},
		{"approval and no record", statementLand(m, "head-1", "", "", []string{"C"}), ledger.ReasonStatementUnreviewed},
		{"approval and a review at another head", statementLand(m, "head-1", "", "head-0", []string{"C"}), ledger.ReasonStatementUnreviewed},
	} {
		_, err := s.Land(ctx, refused.request)
		var r *ledger.Refusal
		if !errors.As(err, &r) || r.Reason != refused.reason {
			t.Fatalf("%s: want refusal %q before any write, got %v", refused.name, refused.reason, err)
		}
	}
	wantUnchanged(t, s, m, before)

	approved := statementLand(m, "head-1", "", "head-1", []string{"C"})
	_, err := s.Land(ctx, approved)
	var trapped *pgconn.PgError
	if !errors.As(err, &trapped) || trapped.Code != "P0001" || trapped.Message != "write trap: contracts" {
		t.Fatalf("approved and reviewed land with the trap armed: want the trap on contracts, got %v", err)
	}
	wantUnchanged(t, s, m, before)

	for _, table := range tables {
		exec(`DROP TRIGGER trap_write ON ` + table)
	}
	if landed := must(s.Land(ctx, approved)); landed.State != ledger.StateLanded {
		t.Fatalf("approved and reviewed land with the trap removed: %+v", landed)
	}
}

func TestLandRequiresTheHeadOfAStatementChange(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	before := len(must(s.Events(ctx, "r", 0, "")))
	for _, request := range []ledger.LandRequest{
		statementLand(m, "", "Spec.C_compat", "", nil),
		statementLand(m, "", "", "", nil),
		statementLand(m, "  ", "", "  ", nil),
		statementLand(m, "head-1", "  ", "", nil),
		statementLand(m, "head-1", "", "head-1 ", nil),
	} {
		_, err := s.Land(ctx, request)
		if request.Head == "" {
			wantRefusal(t, err, ledger.ReasonInvalidCandidate)
			continue
		}
		var invalid *ledger.InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("head %q, proof %q, reviewed head %q: want invalid, got %v",
				request.Head, request.CompatibilityProof, request.ReviewedHead, err)
		}
	}
	wantUnchanged(t, s, m, before)
}

func TestLandOfAStatementChangeWithACompatibilityProof(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	landed := must(s.Land(ctx, statementLand(m, "head-1", "Spec.C_compat", "", nil)))
	if landed.State != ledger.StateLanded || landed.LandedCommit != "commit-1" {
		t.Fatalf("landing with a compatibility proof: %+v", landed)
	}
	if c := must(s.Contract(ctx, "r", "C")); c.StatementHash != "hash-2" || c.Version != 2 {
		t.Fatalf("contract after landing with a compatibility proof: %+v", c)
	}
	wantLanded(t, s, m.ID, "head-1", "Spec.C_compat", "")
}

func TestLandOfAStatementChangeReviewedAtItsHead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	landed := must(s.Land(ctx, statementLand(m, "head-1", "", "head-1", nil)))
	if landed.State != ledger.StateLanded {
		t.Fatalf("landing reviewed at its head: %+v", landed)
	}
	if c := must(s.Contract(ctx, "r", "C")); c.StatementHash != "hash-2" || c.Version != 2 {
		t.Fatalf("contract after a reviewed landing: %+v", c)
	}
	wantLanded(t, s, m.ID, "head-1", "", "head-1")
}

func TestLandRefusesAStatementChangeReviewedAtAnotherHead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	before := len(must(s.Events(ctx, "r", 0, "")))
	_, err := s.Land(ctx, statementLand(m, "head-2", "", "head-1", nil))
	r := wantRefusal(t, err, ledger.ReasonStatementUnreviewed)
	for _, want := range []string{"compatibility proof", "head-2", "head-1"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("refusal %q does not name %q", r.Message, want)
		}
	}
	wantUnchanged(t, s, m, before)
}

func TestLandOfAPinnedStatementChangeNeedsTheApprovalAndARecord(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", true))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	before := len(must(s.Events(ctx, "r", 0, "")))
	// The pinned rule comes before the statement rule, so a change with neither is refused as pinned.
	_, err := s.Land(ctx, statementLand(m, "head-1", "", "", nil))
	wantRefusal(t, err, ledger.ReasonPinned)
	_, err = s.Land(ctx, statementLand(m, "head-1", "", "head-1", nil))
	wantRefusal(t, err, ledger.ReasonPinned)
	_, err = s.Land(ctx, statementLand(m, "head-1", "Spec.C_compat", "", nil))
	wantRefusal(t, err, ledger.ReasonPinned)
	_, err = s.Land(ctx, statementLand(m, "head-1", "", "", []string{"C"}))
	wantRefusal(t, err, ledger.ReasonStatementUnreviewed)
	_, err = s.Land(ctx, statementLand(m, "head-1", "", "head-0", []string{"C"}))
	wantRefusal(t, err, ledger.ReasonStatementUnreviewed)
	wantUnchanged(t, s, m, before)
	landed := must(s.Land(ctx, statementLand(m, "head-1", "", "head-1", []string{"C"})))
	if landed.State != ledger.StateLanded {
		t.Fatalf("approved and reviewed pinned landing: %+v", landed)
	}
	if c := must(s.Contract(ctx, "r", "C")); c.StatementHash != "hash-2" || c.Version != 2 {
		t.Fatalf("contract after an approved and reviewed landing: %+v", c)
	}
	wantLanded(t, s, m.ID, "head-1", "", "head-1")
}

func TestCheckReportsAStatementChangeWithNeitherRecordBeforeLand(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "hash-d1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1", "X.external": "x1"}))
	check := func(changes map[string]string, head, proof, reviewed string) ledger.CheckResult {
		t.Helper()
		return must(s.Check(ctx, ledger.CheckRequest{
			Repo: "r", ID: m.ID, Generation: m.Generation, Commit: "candidate", Changed: []string{"a.lean"},
			Current:          map[string]string{"C": "hash-1", "X.external": "x1"},
			ChangedContracts: changes, Head: head, CompatibilityProof: proof, ReviewedHead: reviewed,
		}))
	}
	changeC := map[string]string{"C": "hash-2"}
	for _, unreviewed := range []struct{ name, head, reviewed, names string }{
		{"no record", "head-1", "", "head-1"},
		{"a review at another head", "head-1", "head-0", "head-0"},
	} {
		res := check(changeC, unreviewed.head, "", unreviewed.reviewed)
		if res.Pass || len(res.Violations) != 1 {
			t.Errorf("%s: want one violation, got %+v", unreviewed.name, res)
			continue
		}
		v := res.Violations[0]
		if v.Kind != ledger.ViolationStatementUnreviewed || v.Name != "C" {
			t.Errorf("%s: violation %+v", unreviewed.name, v)
		}
		for _, want := range []string{"contract C", "compatibility proof", "review", unreviewed.names} {
			if !strings.Contains(v.Message, want) {
				t.Errorf("%s: violation %q does not name %q", unreviewed.name, v.Message, want)
			}
		}
	}
	for _, noHead := range []struct {
		proof, reviewed string
	}{
		{"", ""},
		{"Spec.C_compat", ""},
		{"", "head-1"},
	} {
		if res := check(changeC, "", noHead.proof, noHead.reviewed); !hasViolation(res, ledger.ViolationInvalidCandidate) {
			t.Errorf("missing head with proof %q and review %q: %+v", noHead.proof, noHead.reviewed, res)
		}
	}
	for _, passing := range []struct {
		name                  string
		changes               map[string]string
		head, proof, reviewed string
	}{
		{"proofs only", nil, "", "", ""},
		{"a compatibility proof", changeC, "head-1", "Spec.C_compat", ""},
		{"a review at its head", changeC, "head-1", "", "head-1"},
	} {
		if res := check(passing.changes, passing.head, passing.proof, passing.reviewed); !res.Pass {
			t.Errorf("%s: %+v", passing.name, res)
		}
	}
	// Only a changed read that is a registered contract is a statement change; Land refuses the others for
	// their own reasons.
	for _, other := range []map[string]string{{"X.external": "x2"}, {"D": "hash-d2"}} {
		if res := check(other, "head-1", "", ""); hasViolation(res, ledger.ViolationStatementUnreviewed) {
			t.Errorf("change %v is not a changed registered read: %+v", other, res)
		}
	}

	land := func(reviewed string) error {
		request := statementLand(m, "head-1", "", reviewed, nil)
		request.Current["X.external"] = "x1"
		_, err := s.Land(ctx, request)
		return err
	}
	before := len(must(s.Events(ctx, "r", 0, "")))
	wantRefusal(t, land(""), ledger.ReasonStatementUnreviewed)
	wantUnchanged(t, s, m, before)
	if err := land("head-1"); err != nil {
		t.Fatalf("landing after a passing check: %v", err)
	}
	if got := must(s.Migration(ctx, "r", m.ID)); got.State != ledger.StateLanded {
		t.Fatalf("migration after a reviewed landing: %+v", got)
	}
}

func TestCheckRejectsMalformedStatementRecords(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	for _, request := range []ledger.CheckRequest{
		{Head: " head-1"},
		{Head: "head-1", CompatibilityProof: "Spec.C_compat\n"},
		{Head: "  ", ReviewedHead: "  "},
		{Head: "head-1", ChangedContracts: map[string]string{"C": " "}},
		{Head: "head-1", ChangedContracts: map[string]string{"": "hash-2"}},
	} {
		request.Repo, request.ID, request.Generation, request.Commit = "r", m.ID, m.Generation, "candidate"
		request.Changed, request.Current = []string{"a.lean"}, map[string]string{"C": "hash-1"}
		_, err := s.Check(ctx, request)
		var invalid *ledger.InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("check %+v: want invalid, got %v", request, err)
		}
	}
}
