package ledger_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

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

func TestRecordAgreementRefusesASupersededVersion(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	migration := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	must(s.Land(ctx, "r", migration.ID, migration.Generation, "commit-1", map[string]string{"C": "hash-2"}, nil))
	_, err := s.RecordAgreement(ctx, "r", "A-old", []string{"alice"}, map[string]int64{"C": 1})
	wantRefusal(t, err, ledger.ReasonStaleVersion)
}

func TestLandAdvancesAContractAndStalesExactlyOlderAgreements(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	must(s.RecordAgreement(ctx, "r", "A-old", []string{"alice"}, map[string]int64{"C": 1}))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	landed := must(s.Land(ctx, "r", m.ID, m.Generation, "commit-1", map[string]string{"C": "hash-2"}, nil))
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
}

func TestLandRefusesAPinnedContractWithoutPersonApproval(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", true))
	m := must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "hash-1"}))
	_, err := s.Land(ctx, "r", m.ID, m.Generation, "commit-1", map[string]string{"C": "hash-2"}, nil)
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
			_, err := s.Land(ctx, "r", attempt.migration.ID, attempt.migration.Generation, "commit-1",
				map[string]string{"C": attempt.hash}, nil)
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
