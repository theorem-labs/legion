package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func landRequest(m ledger.Migration, commit string, paths []string, current, changes map[string]string, approved []string) ledger.LandRequest {
	return ledger.LandRequest{
		Repo:             "r",
		Migration:        m.ID,
		Generation:       m.Generation,
		Commit:           commit,
		ChangedPaths:     paths,
		Current:          current,
		ChangedContracts: changes,
		ApprovedPinned:   approved,
	}
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
	wantRefusal(t, err, ledger.ReasonStaleRead)
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
	}{
		{name: "changed", current: map[string]string{"C": "hash-2"}},
		{name: "vanished", current: map[string]string{"C": ""}},
		{name: "missing", current: map[string]string{}},
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
			wantRefusal(t, err, ledger.ReasonStaleRead)
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
	var invalid *ledger.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("missing changed paths: want invalid, got %v", err)
	}
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
	if !errors.As(err, &invalid) {
		t.Fatalf("empty changed paths with changed contract: want invalid, got %v", err)
	}
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
