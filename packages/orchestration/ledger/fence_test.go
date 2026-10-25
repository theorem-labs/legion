package ledger_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// fenceFixture is repository r with batch-1 admitted at version 2. Its member M-1 reads and changes contract C and has
// failed one landing round; its member M-2 reads contract D and the declaration E, which is not a registered contract.
// Outside every batch, M-3 changes C, M-4 changes D and M-5 reads C, each ready. batch-2 exists at version 1.
type fenceFixture struct {
	s       *ledger.Store
	dsn     string
	members map[string]ledger.AdmitMember
}

func admittedFixture(t *testing.T) fenceFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "d1", false))
	must(open(t, s, "M-1", "alice", []string{"a.lean"}, map[string]string{"C": "c1"}))
	must(s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Key: "round-0"}))
	members := map[string]ledger.AdmitMember{
		"M-1": readyAgain(t, s, "M-1", "a.lean", map[string]string{"C": "c1"}, changes("C", "c2", "alice")),
		"M-2": readyMember(t, s, "M-2", "bob", "b.lean", map[string]string{"D": "d1", "E": "e1"}, nil),
	}
	createBatch(t, s, "batch-1", batchRecord("checked"))
	must(s.AdmitBatch(ctx, admitRequest("batch-1", 1, members["M-1"], members["M-2"])))
	members["M-3"] = readyMember(t, s, "M-3", "carol", "c.lean", map[string]string{"C": "c1"}, changes("C", "c3", "alice"))
	members["M-4"] = readyMember(t, s, "M-4", "dave", "d.lean", map[string]string{"D": "d1"}, changes("D", "d2", "bob"))
	members["M-5"] = readyMember(t, s, "M-5", "erin", "e.lean", map[string]string{"C": "c1"}, nil)
	createBatch(t, s, "batch-2", batchRecord("checked"))
	return fenceFixture{s: s, dsn: dsn, members: members}
}

// release ends batch-1's admission as DeclineBatch does: its members are no longer admitted.
func (f fenceFixture) release(t *testing.T) {
	t.Helper()
	batchSQL(t, f.dsn, `UPDATE batch_members SET admitted = false WHERE repo = 'r' AND batch = 'batch-1'`)
	setAdmission(t, f.dsn, "batch-1", "declined", "")
}

// land lands migration id as a single migration with changes, from its declared writes and reads.
func (f fenceFixture) land(id string, changes map[string]string) error {
	ctx := context.Background()
	m := must(f.s.Migration(ctx, "r", id))
	_, err := f.s.Land(ctx, landRequest(m, "landed-"+id, m.Writes, m.Reads, changes, nil))
	return err
}

// ledgerSnapshot is everything a fenced mutation could change.
type ledgerSnapshot struct {
	Status    ledger.Status
	Ready     []ledger.ReadyRecord
	Contracts []ledger.Contract
	Batches   []ledger.Batch
	Members   [][]memberRow
	Events    int
}

func (f fenceFixture) snapshot(t *testing.T) ledgerSnapshot {
	t.Helper()
	ctx := context.Background()
	return ledgerSnapshot{
		Status:    must(f.s.Status(ctx, "r")),
		Ready:     must(f.s.ReadyMigrations(ctx, "r")),
		Contracts: must(f.s.Contracts(ctx, "r")),
		Batches:   []ledger.Batch{must(f.s.LoadBatch(ctx, "r", "batch-1")), must(f.s.LoadBatch(ctx, "r", "batch-2"))},
		Members:   [][]memberRow{batchMembers(t, f.dsn, "batch-1"), batchMembers(t, f.dsn, "batch-2")},
		Events:    len(must(f.s.Events(ctx, "r", 0, ""))),
	}
}

// While a migration is admitted, every mutation that admission checked for it is refused with batch_admitted and
// changes nothing: of the member itself, of a contract that a member changes or reads, and an admission of another batch
// that would rely on what the admitted batch changes. After DeclineBatch releases the admission, each succeeds.
func TestFenceRefusesEveryMutationThatAdmissionChecked(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		prepare func(f fenceFixture)
		mutate  func(f fenceFixture) error
		then    func(t *testing.T, f fenceFixture)
	}{
		{name: "a takeover of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.TakeOver(ctx, "r", "M-1", "zoe", 1)
			return err
		}},
		{name: "an extension of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.ExtendMigration(ctx, "r", "M-1", 1, []string{"z.lean"}, nil)
			return err
		}},
		{name: "an abandon of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.Abandon(ctx, "r", "M-1", 1, "stopped")
			return err
		}},
		{name: "a ready of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: "M-1", Generation: 1, Head: "head-M-1b"})
			return err
		}},
		{name: "a round of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "failed", Key: "round-1"})
			return err
		}},
		{
			name:    "a round of a member that would escalate it",
			prepare: func(f fenceFixture) { must(f.s.EnsureRepo(ctx, "r", 2)) },
			mutate: func(f fenceFixture) error {
				_, err := f.s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "failed", Key: "round-1"})
				return err
			},
			then: func(t *testing.T, f fenceFixture) {
				if m := must(f.s.Migration(ctx, "r", "M-1")); m.EscalatedAt == nil || m.LandingRounds != 2 {
					t.Fatalf("after the release the round did not escalate: %+v", m)
				}
			},
		},
		{name: "an escalation of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.Escalate(ctx, "r", "M-1", 1)
			return err
		}},
		{name: "a single landing of a member", mutate: func(f fenceFixture) error {
			return f.land("M-1", map[string]string{"C": "c2"})
		}},
		{name: "a transfer of a contract a member changes", mutate: func(f fenceFixture) error {
			_, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe")
			return err
		}},
		{name: "a registration of a declaration a member reads", mutate: func(f fenceFixture) error {
			_, err := f.s.RegisterContract(ctx, "r", "E", "bob", "e1", false)
			return err
		}},
		{name: "a single landing that advances a contract a member changes", mutate: func(f fenceFixture) error {
			return f.land("M-3", map[string]string{"C": "c3"})
		}},
		{name: "a single landing that advances a contract a member reads", mutate: func(f fenceFixture) error {
			return f.land("M-4", map[string]string{"D": "d2"})
		}},
		{name: "an admission of a member", mutate: func(f fenceFixture) error {
			_, err := f.s.AdmitBatch(ctx, admitRequest("batch-2", 1, f.members["M-1"]))
			return err
		}},
		{name: "an admission that advances a contract a member changes", mutate: func(f fenceFixture) error {
			_, err := f.s.AdmitBatch(ctx, admitRequest("batch-2", 1, f.members["M-3"]))
			return err
		}},
		{name: "an admission that advances a contract a member reads", mutate: func(f fenceFixture) error {
			_, err := f.s.AdmitBatch(ctx, admitRequest("batch-2", 1, f.members["M-4"]))
			return err
		}},
		{name: "an admission of a reader of a contract a member changes", mutate: func(f fenceFixture) error {
			_, err := f.s.AdmitBatch(ctx, admitRequest("batch-2", 1, f.members["M-5"]))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := admittedFixture(t)
			if test.prepare != nil {
				test.prepare(f)
			}
			before := f.snapshot(t)
			wantRefusal(t, test.mutate(f), ledger.ReasonBatchAdmitted)
			if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused mutation changed the ledger:\n got %+v\nwant %+v", after, before)
			}
			f.release(t)
			if err := test.mutate(f); err != nil {
				t.Fatalf("after the release: %v", err)
			}
			if test.then != nil {
				test.then(t, f)
			}
		})
	}
}

// Check reports what a single Land would refuse while a batch is admitted: a candidate of an admitted member, and a
// candidate that advances a contract an admitted member reads or changes, each a batch_admitted violation that names
// the contract and the member that relies on it. A candidate that only reads a contract an admitted member changes is
// not fenced: it changes nothing that the admitted batch relies on.
func TestCheckReportsWhatTheFenceRefuses(t *testing.T) {
	ctx := context.Background()
	f := admittedFixture(t)
	check := func(id string, changes map[string]string) ledger.CheckResult {
		m := must(f.s.Migration(ctx, "r", id))
		return must(f.s.Check(ctx, ledger.CheckRequest{
			Repo: "r", ID: id, Generation: 1, Commit: "landed-" + id, Changed: m.Writes, Current: m.Reads,
			ChangedContracts: changes, Head: "head-" + id, ReviewedHead: "head-" + id,
		}))
	}
	for _, test := range []struct {
		id          string
		changes     map[string]string
		name, other string
	}{
		{"M-1", map[string]string{"C": "c2"}, "", ""},
		{"M-3", map[string]string{"C": "c3"}, "C", "M-1"},
		{"M-4", map[string]string{"D": "d2"}, "D", "M-2"},
	} {
		result := check(test.id, test.changes)
		if result.Pass || len(result.Violations) != 1 {
			t.Fatalf("check of %s while batch-1 is admitted: %+v, want one batch_admitted violation", test.id, result)
		}
		if v := result.Violations[0]; v.Kind != ledger.ViolationBatchAdmitted || v.Name != test.name || v.Other != test.other {
			t.Fatalf("check of %s reports %+v, want batch_admitted naming %q and %q", test.id, v, test.name, test.other)
		}
	}
	if result := check("M-5", nil); !result.Pass {
		t.Fatalf("check of a reader of an admitted member's contract: %+v", result)
	}
	f.release(t)
	for _, id := range []string{"M-1", "M-3"} {
		if result := check(id, map[string]string{"C": "c-" + id}); !result.Pass {
			t.Fatalf("check of %s after the release: %+v", id, result)
		}
	}
}

// The fence covers only what admission relies on. While batch-1 is admitted: a contract that its members only read can
// be transferred, because landing reads the owner of a changed contract only; an unrelated declaration can be
// registered; the repository's landing-round limit can change; a migration that reads a contract a member only reads
// can be admitted in another batch, and one that reads a contract a member changes can land on its own.
func TestFenceLeavesWhatAdmissionDoesNotRelyOn(t *testing.T) {
	ctx := context.Background()
	f := admittedFixture(t)
	if transferred, err := f.s.TransferContract(ctx, "r", "D", "bob", "zoe"); err != nil || transferred.Owner != "zoe" {
		t.Fatalf("transfer of a contract that members only read: %+v, %v", transferred, err)
	}
	if _, err := f.s.RegisterContract(ctx, "r", "F", "zoe", "f1", false); err != nil {
		t.Fatalf("registration of an unrelated declaration: %v", err)
	}
	if repository, err := f.s.EnsureRepo(ctx, "r", 5); err != nil || repository.MaxLandingRounds != 5 {
		t.Fatalf("a change of the landing-round limit: %+v, %v", repository, err)
	}
	reader := readyMember(t, f.s, "M-6", "frank", "f.lean", map[string]string{"D": "d1"}, nil)
	if admitted, err := f.s.AdmitBatch(ctx, admitRequest("batch-2", 1, reader)); err != nil || admitted.Admission != "admitted" {
		t.Fatalf("admission of a reader of a contract a member reads: %+v, %v", admitted, err)
	}
	if err := f.land("M-5", nil); err != nil {
		t.Fatalf("a single landing that reads a contract a member changes: %v", err)
	}
}

// What the fence leaves to the rules before it. Retry has no fence of its own: the fence keeps an admitted member from
// being escalated, so a Retry of one refuses not_escalated. A repeat of a round key that the member recorded before its
// admission is a replay, which returns the migration as it is before the fence is asked.
func TestFenceLeavesRetryAndReplaysToTheirOwnRules(t *testing.T) {
	ctx := context.Background()
	f := admittedFixture(t)
	before := f.snapshot(t)
	_, err := f.s.Retry(ctx, "r", "M-1")
	wantRefusal(t, err, ledger.ReasonNotEscalated)
	replayed, err := f.s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Key: "round-0"})
	if err != nil || replayed.LandingRounds != 1 {
		t.Fatalf("a repeat of a known round key of an admitted member: %+v, %v", replayed, err)
	}
	if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("the retry or the replay changed the ledger:\n got %+v\nwant %+v", after, before)
	}
}

// A registration of a name and an admission whose member reads that name, started together on two connections: exactly
// one commits and the other is refused and changes nothing. A registration has no contract row to lock, so only the
// admission's FOR UPDATE on the repository row, against the registration's FOR SHARE, orders the two. The test holds
// the repository's event lock, which every call takes after its checks and before it commits, until both calls wait on
// a lock: so their checks overlap whenever their row locks let them.
func TestRegistrationRacingAnAdmissionCommitsOne(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	member := readyMember(t, s, "M-1", "alice", "a.lean", map[string]string{"X": "x1"}, nil)
	created := createBatch(t, s, "batch-1", batchRecord("checked"))

	racers := openStoreWithPool(t, dsn, "registration_race", 2)
	holder := must(pgx.Connect(ctx, dsn))
	defer holder.Close(ctx)
	must(holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('r', 0))`))
	var wait sync.WaitGroup
	var admitErr, registerErr error
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, admitErr = racers.AdmitBatch(ctx, admitRequest("batch-1", 1, member))
	}()
	go func() {
		defer wait.Done()
		_, registerErr = racers.RegisterContract(ctx, "r", "X", "bob", "x9", false)
	}()
	watcher := must(pgx.Connect(ctx, dsn))
	defer watcher.Close(ctx)
	waitForBlockedConnections(t, watcher, 2)
	must(holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('r', 0))`))
	wait.Wait()

	var refusal *ledger.Refusal
	switch {
	case admitErr == nil && registerErr == nil:
		t.Fatalf("both the admission and the registration committed")
	case admitErr == nil:
		if !errors.As(registerErr, &refusal) || refusal.Reason != ledger.ReasonBatchAdmitted {
			t.Fatalf("the registration after the admission: %v, want batch_admitted", registerErr)
		}
		var notFound *ledger.NotFoundError
		if _, err := s.Contract(ctx, "r", "X"); !errors.As(err, &notFound) {
			t.Fatalf("the refused registration left contract X: %v", err)
		}
	case registerErr == nil:
		if !errors.As(admitErr, &refusal) || refusal.Reason != ledger.ReasonStaleRead {
			t.Fatalf("the admission after the registration: %v, want stale_read", admitErr)
		}
		if after := must(s.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(after, created) {
			t.Fatalf("the refused admission changed the batch: %+v", after)
		}
		if members := batchMembers(t, dsn, "batch-1"); len(members) != 0 {
			t.Fatalf("the refused admission recorded members %+v", members)
		}
	default:
		t.Fatalf("both calls were refused: admission %v, registration %v", admitErr, registerErr)
	}
}

// A sweep of every migration at the limit does not escalate an admitted member, and escalates the others. After the
// release, the member is escalated too.
func TestEscalateSweepSkipsAnAdmittedMember(t *testing.T) {
	ctx := context.Background()
	f := admittedFixture(t)
	must(open(t, f.s, "M-7", "gina", []string{"g.lean"}, nil))
	must(f.s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-7", Generation: 1, Reason: "failed", Key: "round-0"}))
	ids := func(migrations []ledger.Migration) []string {
		out := []string{}
		for _, m := range migrations {
			out = append(out, m.ID)
		}
		return out
	}
	if swept := must(f.s.Escalate(ctx, "r", "", 1)); !slices.Equal(ids(swept), []string{"M-7"}) {
		t.Fatalf("the sweep escalated %q, want only M-7", ids(swept))
	}
	if m := must(f.s.Migration(ctx, "r", "M-1")); m.EscalatedAt != nil {
		t.Fatalf("the sweep escalated the admitted member: %+v", m)
	}
	f.release(t)
	if swept := must(f.s.Escalate(ctx, "r", "", 1)); !slices.Equal(ids(swept), []string{"M-1"}) {
		t.Fatalf("the sweep after the release escalated %q, want M-1", ids(swept))
	}
}
