package ledger_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// readyMember opens migration id for owner with one declared write and the reads given, readies it at generation 1
// with the head head-<id> and whatever records ready adds, and returns the member that the landing program builds from
// the ready record: its ready event, the read hashes it computed from the candidate (the declared ones) and its
// changed path.
func readyMember(t *testing.T, s *ledger.Store, id, owner, write string, reads map[string]string, ready func(*ledger.ReadyRequest)) ledger.AdmitMember {
	t.Helper()
	must(open(t, s, id, owner, []string{write}, reads))
	return readyAgain(t, s, id, write, reads, ready)
}

// readyAgain readies the open migration id at generation 1 and returns the member that the program builds from it.
func readyAgain(t *testing.T, s *ledger.Store, id, write string, reads map[string]string, ready func(*ledger.ReadyRequest)) ledger.AdmitMember {
	t.Helper()
	request := ledger.ReadyRequest{Repo: "r", ID: id, Generation: 1, Head: "head-" + id}
	if ready != nil {
		ready(&request)
	}
	// Each submission takes its own key, so a second ready of one migration publishes a new record rather than
	// replaying the first.
	request.Key = "ready-" + request.Head
	record := must(s.Ready(context.Background(), request))
	current := map[string]string{}
	for name, hash := range reads {
		current[name] = hash
	}
	return ledger.AdmitMember{Migration: id, ReadyEvent: record.ReadyEvent, CurrentHashes: current, ChangedPaths: []string{write}}
}

// changes makes a ready record change contract to hash, with a review by reviewer at the ready head.
func changes(contract, hash, reviewer string) func(*ledger.ReadyRequest) {
	return func(r *ledger.ReadyRequest) {
		r.ChangedContracts = map[string]string{contract: hash}
		r.Reviews = []ledger.Review{{Contract: contract, Reviewer: reviewer, Head: r.Head}}
	}
}

// admitRequest is the landing program's admission of batch id, which it read at version, for members in that order.
func admitRequest(id string, version int64, members ...ledger.AdmitMember) ledger.AdmitRequest {
	return ledger.AdmitRequest{
		Repo: "r", ID: id, Base: "base-1", CandidateCommit: "candidate-" + id, CandidateTree: "tree-" + id,
		Members: members, ExpectedVersion: version,
		Record: batchRecord("publishing", "candidate", map[string]any{"commit": "candidate-" + id, "tree": "tree-" + id}),
	}
}

// memberRow is one row of batch_members.
type memberRow struct {
	Migration  string
	Position   int
	Generation int64
	Head       string
	ReadyEvent int64
	Admitted   bool
}

// batchMembers reads the frozen members of batch id, in position order.
func batchMembers(t *testing.T, dsn, id string) []memberRow {
	t.Helper()
	ctx := context.Background()
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	rows := must(conn.Query(ctx, `SELECT migration, position, generation, head, ready_event, admitted FROM batch_members
		WHERE repo = 'r' AND batch = $1 ORDER BY position`, id))
	members := must(pgx.CollectRows(rows, pgx.RowToStructByPos[memberRow]))
	if members == nil {
		members = []memberRow{}
	}
	return members
}

// assertNotAdmitted checks that a refused admission recorded nothing: the batch is as before, it has no member, and
// the log has no new event.
func assertNotAdmitted(t *testing.T, s *ledger.Store, dsn string, before ledger.Batch, events int) {
	t.Helper()
	assertBatchUnchanged(t, s, before, events)
	if members := batchMembers(t, dsn, before.ID); len(members) != 0 {
		t.Fatalf("a refused admission recorded members %+v", members)
	}
}

// wantMemberRefusal checks that err refuses reason and names member.
func wantMemberRefusal(t *testing.T, err error, reason, member string) *ledger.Refusal {
	t.Helper()
	refusal := wantRefusal(t, err, reason)
	if refusal.Member != member {
		t.Fatalf("refusal %+v names member %q, want %q", refusal, refusal.Member, member)
	}
	return refusal
}

// AdmitBatch admits ready members from their stored ready records: it freezes them in the requested order with the
// generation, head and ready event of each record, marks them admitted, writes the caller's record at the next version
// with the admission and its digest, and appends batch.admitted. It neither advances a contract nor clears a ready
// record.
func TestAdmitBatchAdmitsReadyMembersFromTheirReadyRecords(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	first := readyMember(t, s, "M-1", "alice", "a.lean", map[string]string{"C": "c1"}, changes("C", "c2", "alice"))
	second := readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
	created := createBatch(t, s, "batch-1", batchRecord("checked"))

	request := admitRequest("batch-1", created.RecordVersion, second, first)
	admitted, err := s.AdmitBatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.RecordVersion != 2 || admitted.Admission != "admitted" || admitted.Phase != "publishing" ||
		len(admitted.AdmissionDigest) != 64 || !sameJSON(t, admitted.Record, request.Record) ||
		!admitted.CreatedAt.Equal(created.CreatedAt) || !admitted.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("admitted %+v from %+v", admitted, created)
	}
	if loaded := must(s.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, admitted) {
		t.Fatalf("loaded %+v, want the admitted %+v", loaded, admitted)
	}
	want := []memberRow{
		{Migration: "M-2", Position: 0, Generation: 1, Head: "head-M-2", ReadyEvent: second.ReadyEvent, Admitted: true},
		{Migration: "M-1", Position: 1, Generation: 1, Head: "head-M-1", ReadyEvent: first.ReadyEvent, Admitted: true},
	}
	if got := batchMembers(t, dsn, "batch-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen members = %+v, want %+v", got, want)
	}
	if got := eventKinds(t, s, "batch-1"); !slices.Equal(got, []string{"batch.created", "batch.admitted"}) {
		t.Fatalf("batch events = %q", got)
	}
	if ready := must(s.ReadyMigrations(ctx, "r")); len(ready) != 2 {
		t.Fatalf("admission changed the ready records: %+v", ready)
	}
	if contract := must(s.Contract(ctx, "r", "C")); contract.Version != 1 || contract.StatementHash != "c1" {
		t.Fatalf("admission advanced contract C: %+v", contract)
	}
	for _, kind := range eventKinds(t, s, "") {
		if kind == "migration.checked" {
			t.Fatalf("admission appended a check event: %q", eventKinds(t, s, ""))
		}
	}

	var notFound *ledger.NotFoundError
	if _, err := s.AdmitBatch(ctx, admitRequest("batch-2", 1, second)); !errors.As(err, &notFound) || notFound.What != "batch" {
		t.Fatalf("admission of a missing batch: %v", err)
	}
	missing := admitRequest("batch-1", 2, second)
	missing.Repo = "missing"
	if _, err := s.AdmitBatch(ctx, missing); !errors.As(err, &notFound) || notFound.What != "repository" {
		t.Fatalf("admission in a missing repository: %v", err)
	}
}

// The request has no field for reviews or approvals: the Check that admission runs uses the ones that the owner gave
// to Ready, which the ledger stores in the ready record.
func TestAdmitBatchUsesTheReviewsAndApprovalsGivenToReady(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		reviews []ledger.Review
		pinned  []string
		reason  string
	}{
		{"the owner's review and an approval", []ledger.Review{{Contract: "P", Reviewer: "carol", Head: "head-M-1"}}, []string{"P"}, ""},
		{"no approval of the pinned contract", []ledger.Review{{Contract: "P", Reviewer: "carol", Head: "head-M-1"}}, nil, ledger.ReasonPinned},
		{"a review by another reviewer", []ledger.Review{{Contract: "P", Reviewer: "alice", Head: "head-M-1"}}, []string{"P"}, ledger.ReasonStatementUnreviewed},
		{"a review at another head", []ledger.Review{{Contract: "P", Reviewer: "carol", Head: "head-0"}}, []string{"P"}, ledger.ReasonStatementUnreviewed},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, dsn := newStoreDSN(t)
			must(s.RegisterContract(ctx, "r", "P", "carol", "p1", true))
			member := readyMember(t, s, "M-1", "alice", "a.lean", map[string]string{"P": "p1"}, func(r *ledger.ReadyRequest) {
				r.ChangedContracts = map[string]string{"P": "p2"}
				r.Reviews, r.ApprovedPinned = test.reviews, test.pinned
			})
			created := createBatch(t, s, "batch-1", batchRecord("checked"))
			events := len(must(s.Events(ctx, "r", 0, "")))
			admitted, err := s.AdmitBatch(ctx, admitRequest("batch-1", 1, member))
			if test.reason == "" {
				if err != nil || admitted.Admission != "admitted" {
					t.Fatalf("admission with the owner's review and an approval: %+v, %v", admitted, err)
				}
				return
			}
			refusal := wantMemberRefusal(t, err, test.reason, "M-1")
			if len(refusal.Violations) != 1 || refusal.Violations[0].Kind != test.reason {
				t.Fatalf("refusal violations = %+v, want one %s", refusal.Violations, test.reason)
			}
			assertNotAdmitted(t, s, dsn, created, events)
		})
	}
}

// A member that is not ready, or whose ready record changed after the program read it, is refused and named, and the
// admission records nothing. Both are races with the member's owner or another writer.
func TestAdmitBatchRefusesAMemberWhoseReadyRecordIsGoneOrChanged(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		move   func(t *testing.T, s *ledger.Store, member *ledger.AdmitMember)
		reason string
	}{
		{"never readied", func(t *testing.T, s *ledger.Store, member *ledger.AdmitMember) {
			must(open(t, s, "M-2", "bob", []string{"b.lean"}, nil))
			*member = ledger.AdmitMember{Migration: "M-2", ReadyEvent: 0, CurrentHashes: map[string]string{}, ChangedPaths: []string{"b.lean"}}
		}, ledger.ReasonNotReady},
		{"cleared by a takeover after the program read it", func(t *testing.T, s *ledger.Store, member *ledger.AdmitMember) {
			*member = readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
			must(s.TakeOver(ctx, "r", "M-2", "carol", 1))
		}, ledger.ReasonNotReady},
		{"cleared by a round after the program read it", func(t *testing.T, s *ledger.Store, member *ledger.AdmitMember) {
			*member = readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
			must(s.RecordRound(ctx, ledger.RoundRequest{Repo: "r", ID: "M-2", Generation: 1, Reason: "build failed", Key: "round-1",
				ReadyEvent: member.ReadyEvent}))
		}, ledger.ReasonNotReady},
		{"readied again after the program read it", func(t *testing.T, s *ledger.Store, member *ledger.AdmitMember) {
			*member = readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
			readyAgain(t, s, "M-2", "b.lean", nil, func(r *ledger.ReadyRequest) { r.Head = "head-M-2b" })
		}, ledger.ReasonReadyChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, dsn := newStoreDSN(t)
			first := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
			var second ledger.AdmitMember
			test.move(t, s, &second)
			created := createBatch(t, s, "batch-1", batchRecord("checked"))
			events := len(must(s.Events(ctx, "r", 0, "")))
			_, err := s.AdmitBatch(ctx, admitRequest("batch-1", 1, first, second))
			wantMemberRefusal(t, err, test.reason, "M-2")
			assertNotAdmitted(t, s, dsn, created, events)
		})
	}
}

// Admission checks each member against the state with the earlier members landed. So a member that changes a contract
// an earlier member reads or changes, or that reads a contract an earlier member changes, is refused batch_conflict
// and named, with the earlier member as the other one; two members that only read a contract are admitted together.
func TestAdmitBatchRefusesAnInBatchContractConflict(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	reads := map[string]string{"C": "c1"}
	changer := readyMember(t, s, "M-1", "alice", "a.lean", reads, changes("C", "c2", "alice"))
	reader := readyMember(t, s, "M-2", "bob", "b.lean", reads, nil)
	otherChanger := readyMember(t, s, "M-3", "carol", "c.lean", reads, changes("C", "c3", "alice"))
	otherReader := readyMember(t, s, "M-4", "dave", "d.lean", reads, nil)
	created := createBatch(t, s, "batch-1", batchRecord("checked"))
	events := len(must(s.Events(ctx, "r", 0, "")))
	for _, test := range []struct {
		name          string
		members       []ledger.AdmitMember
		member, other string
	}{
		{"a reader after a changer", []ledger.AdmitMember{changer, reader}, "M-2", "M-1"},
		{"a changer after a reader", []ledger.AdmitMember{reader, changer}, "M-1", "M-2"},
		{"two changers", []ledger.AdmitMember{changer, otherChanger}, "M-3", "M-1"},
	} {
		_, err := s.AdmitBatch(ctx, admitRequest("batch-1", 1, test.members...))
		if refusal := wantMemberRefusal(t, err, ledger.ReasonBatchConflict, test.member); refusal.Other != test.other {
			t.Fatalf("%s: batch_conflict names the other member %q, want %q", test.name, refusal.Other, test.other)
		}
		assertNotAdmitted(t, s, dsn, created, events)
	}
	if admitted, err := s.AdmitBatch(ctx, admitRequest("batch-1", 1, reader, otherReader)); err != nil || admitted.Admission != "admitted" {
		t.Fatalf("two readers of one contract: %+v, %v", admitted, err)
	}
}

// A member that Check refuses is named with every reason Check gives for the same candidate, and the admission records
// nothing. The refusal's reason is the first of them, and its message names the member and every reason, so that the
// text form tells an operator which member to drop.
func TestAdmitBatchNamesAMemberThatCheckRefusesWithItsReasons(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "D", "bob", "d1", false))
	first := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
	second := readyMember(t, s, "M-2", "bob", "b.lean", map[string]string{"D": "d1"}, nil)
	created := createBatch(t, s, "batch-1", batchRecord("checked"))
	for _, test := range []struct {
		name   string
		change func(*ledger.AdmitMember)
		kinds  []string
	}{
		{"a path outside the lock and a read with no hash", func(m *ledger.AdmitMember) {
			m.ChangedPaths, m.CurrentHashes = []string{"outside.lean"}, map[string]string{}
		}, []string{ledger.ReasonUndeclaredWrite, ledger.ReasonUnverifiedRead}},
		{"a changed read", func(m *ledger.AdmitMember) {
			m.CurrentHashes = map[string]string{"D": "d0"}
		}, []string{ledger.ReasonStaleRead}},
		{"no changed paths", func(m *ledger.AdmitMember) { m.ChangedPaths = nil }, []string{ledger.ReasonInvalidCandidate}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := second
			test.change(&candidate)
			events := len(must(s.Events(ctx, "r", 0, "")))
			_, err := s.AdmitBatch(ctx, admitRequest("batch-1", 1, first, candidate))
			refusal := wantMemberRefusal(t, err, test.kinds[0], "M-2")
			assertNotAdmitted(t, s, dsn, created, events)

			checked := must(s.Check(ctx, ledger.CheckRequest{LandRequest: ledger.LandRequest{
				Repo: "r", Migration: "M-2", Generation: 1, Commit: "candidate-batch-1", ChangedPaths: candidate.ChangedPaths,
				Current: candidate.CurrentHashes, StatementRecords: ledger.StatementRecords{Head: "head-M-2"},
			}}))
			if !reflect.DeepEqual(refusal.Violations, checked.Violations) {
				t.Fatalf("admission's reasons %+v, Check's %+v", refusal.Violations, checked.Violations)
			}
			kinds := []string{}
			for _, violation := range refusal.Violations {
				kinds = append(kinds, violation.Kind)
			}
			if !slices.Equal(kinds, test.kinds) {
				t.Fatalf("reasons %q, want %q", kinds, test.kinds)
			}
			if !strings.HasPrefix(refusal.Message, "member M-2: ") {
				t.Fatalf("refusal message %q does not name the member", refusal.Message)
			}
			for _, violation := range refusal.Violations {
				if !strings.Contains(refusal.Message, violation.Kind+": "+violation.Message) {
					t.Fatalf("refusal message %q does not give %s", refusal.Message, violation.Kind)
				}
			}
		})
	}
}

// Every admission names the version it read. After a competing save, an admission from the old version is refused
// with stale_batch and records nothing, and it succeeds after a reload.
func TestAdmitBatchComparesTheRecordVersion(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	member := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
	created := createBatch(t, s, "batch-1", batchRecord("checked"))
	competing := must(s.SaveBatch(ctx, "r", "batch-1", batchRecord("publishing"), created.RecordVersion))
	events := len(must(s.Events(ctx, "r", 0, "")))
	_, err := s.AdmitBatch(ctx, admitRequest("batch-1", created.RecordVersion, member))
	wantRefusal(t, err, ledger.ReasonStaleBatch)
	assertNotAdmitted(t, s, dsn, competing, events)

	reloaded := must(s.LoadBatch(ctx, "r", "batch-1"))
	admitted, err := s.AdmitBatch(ctx, admitRequest("batch-1", reloaded.RecordVersion, member))
	if err != nil || admitted.RecordVersion != 3 || admitted.Admission != "admitted" {
		t.Fatalf("admission after a reload: %+v, %v", admitted, err)
	}
}

// admissionDigest is the digest that AdmitBatch stores: SHA-256 over the canonical JSON of the base, the candidate
// commit and tree, and each member in order with its migration, ready event, sorted changed paths and sorted current
// hashes.
func admissionDigest(t *testing.T, request ledger.AdmitRequest) string {
	t.Helper()
	type member struct {
		Migration     string            `json:"migration"`
		ReadyEvent    int64             `json:"ready_event"`
		ChangedPaths  []string          `json:"changed_paths"`
		CurrentHashes map[string]string `json:"current_hashes"`
	}
	members := []member{}
	for _, m := range request.Members {
		paths := slices.Clone(m.ChangedPaths)
		slices.Sort(paths)
		members = append(members, member{m.Migration, m.ReadyEvent, paths, m.CurrentHashes})
	}
	body := must(json.Marshal(struct {
		Base            string   `json:"base"`
		CandidateCommit string   `json:"candidate_commit"`
		CandidateTree   string   `json:"candidate_tree"`
		Members         []member `json:"members"`
	}{request.Base, request.CandidateCommit, request.CandidateTree, members}))
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// A retry of an admission whose response was lost carries the same request, and it returns the stored batch, record
// and version and changes nothing, whatever version and record it carries; changed paths in another order are the same
// request. A request that differs in any field of the digest is another request: from the old version it is refused
// with stale_batch, and at the current version with batch_admitted, because the batch is already admitted. Neither
// changes anything.
func TestAdmitBatchReplaysTheSameRequestOnly(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "c1", false))
	first := readyMember(t, s, "M-1", "alice", "a/", map[string]string{"C": "c1"}, nil)
	first.ChangedPaths = []string{"a/2.lean", "a/1.lean"}
	second := readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
	createBatch(t, s, "batch-1", batchRecord("checked"))
	request := admitRequest("batch-1", 1, first, second)
	admitted := must(s.AdmitBatch(ctx, request))
	if want := admissionDigest(t, request); admitted.AdmissionDigest != want {
		t.Fatalf("admission digest %s, want %s", admitted.AdmissionDigest, want)
	}
	members := batchMembers(t, dsn, "batch-1")
	events := len(must(s.Events(ctx, "r", 0, "")))

	retry := request
	retry.Record = batchRecord("other", "round", 9)
	retry.Members = []ledger.AdmitMember{first, second}
	retry.Members[0].ChangedPaths = []string{"a/1.lean", "a/2.lean"}
	for _, version := range []int64{1, 7} {
		retry.ExpectedVersion = version
		if replayed, err := s.AdmitBatch(ctx, retry); err != nil || !reflect.DeepEqual(replayed, admitted) {
			t.Fatalf("retry at version %d returned %+v, %v; want the stored %+v", version, replayed, err, admitted)
		}
		assertBatchUnchanged(t, s, admitted, events)
	}

	for _, test := range []struct {
		name   string
		change func(*ledger.AdmitRequest)
	}{
		{"base", func(r *ledger.AdmitRequest) { r.Base = "base-2" }},
		{"candidate commit", func(r *ledger.AdmitRequest) { r.CandidateCommit = "candidate-2" }},
		{"candidate tree", func(r *ledger.AdmitRequest) { r.CandidateTree = "tree-2" }},
		{"member order", func(r *ledger.AdmitRequest) { r.Members = []ledger.AdmitMember{second, first} }},
		{"a changed path", func(r *ledger.AdmitRequest) {
			r.Members = []ledger.AdmitMember{first, second}
			r.Members[0].ChangedPaths = []string{"a/1.lean"}
		}},
		{"a current hash", func(r *ledger.AdmitRequest) {
			r.Members = []ledger.AdmitMember{first, second}
			r.Members[0].CurrentHashes = map[string]string{"C": "c0"}
		}},
		{"a ready event", func(r *ledger.AdmitRequest) {
			r.Members = []ledger.AdmitMember{first, second}
			r.Members[1].ReadyEvent++
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.change(&changed)
			if admissionDigest(t, changed) == admitted.AdmissionDigest {
				t.Fatalf("the changed request has the admitted digest")
			}
			changed.ExpectedVersion = 1
			_, err := s.AdmitBatch(ctx, changed)
			wantRefusal(t, err, ledger.ReasonStaleBatch)
			assertBatchUnchanged(t, s, admitted, events)
			changed.ExpectedVersion = admitted.RecordVersion
			_, err = s.AdmitBatch(ctx, changed)
			wantRefusal(t, err, ledger.ReasonBatchAdmitted)
			assertBatchUnchanged(t, s, admitted, events)
			if got := batchMembers(t, dsn, "batch-1"); !reflect.DeepEqual(got, members) {
				t.Fatalf("members after a refused admission = %+v, want %+v", got, members)
			}
		})
	}
}

// A restart after an admission finds the record and version that it wrote, the batch active, and its members frozen.
func TestAdmitBatchSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	member := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
	createBatch(t, s, "batch-1", batchRecord("checked"))
	admitted := must(s.AdmitBatch(ctx, admitRequest("batch-1", 1, member)))
	members := batchMembers(t, dsn, "batch-1")
	s.Close()
	restarted := must(ledger.Open(ctx, dsn))
	defer restarted.Close()
	if loaded := must(restarted.LoadBatch(ctx, "r", "batch-1")); !reflect.DeepEqual(loaded, admitted) {
		t.Fatalf("after a restart the batch is %+v, want the admitted %+v", loaded, admitted)
	}
	if active := activeIDs(t, restarted); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("active batches after a restart = %q", active)
	}
	if got := batchMembers(t, dsn, "batch-1"); len(got) != 1 || !got[0].Admitted || !reflect.DeepEqual(got, members) {
		t.Fatalf("members after a restart = %+v", got)
	}
}

// AdmitBatch writes the caller's record, so it too can leave a batch landed_unrecorded. Like every write of a batch
// row, it then enqueues landing:<id> for the record's escalate_to in its own transaction, and a retry of the same
// request replays without enqueuing it again.
func TestAdmitBatchLandedUnrecordedEnqueuesItsEscalation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	member := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
	createBatch(t, s, "batch-1", batchRecord("checked"))
	request := admitRequest("batch-1", 1, member)
	request.Record = unrecordedRecord(escalationOwner)
	admitted := must(s.AdmitBatch(ctx, request))
	if admitted.Phase != ledger.PhaseLandedUnrecorded {
		t.Fatalf("admitted batch = %+v", admitted)
	}
	if item := must(s.Item(ctx, "r", "landing:batch-1")); item.Owner != escalationOwner || item.State != "open" {
		t.Fatalf("escalation item = %+v", item)
	}
	if replayed := must(s.AdmitBatch(ctx, request)); !reflect.DeepEqual(replayed, admitted) {
		t.Fatalf("the retry returned %+v, want the stored %+v", replayed, admitted)
	}
	if got := eventKinds(t, s, "landing:batch-1"); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q, want one item.enqueued", got)
	}
}

func deadlocksNow(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var deadlocks int64
	if err := conn.QueryRow(context.Background(), `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&deadlocks); err != nil {
		t.Fatal(err)
	}
	return deadlocks
}

// Two admissions of the same two migrations in opposite orders, on two connections, start together: one is admitted
// and the other is refused with batch_admitted. Neither meets a deadlock, which the ledger would retry and so hide:
// Postgres counts each deadlock it breaks, and the count stays the same. The test holds the repository's row lock
// until both admissions wait on a lock, so they run at once.
func TestConcurrentAdmissionsInOppositeOrdersAdmitOne(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	first := readyMember(t, s, "M-1", "alice", "a.lean", nil, nil)
	second := readyMember(t, s, "M-2", "bob", "b.lean", nil, nil)
	batches := map[string]ledger.Batch{
		"batch-1": createBatch(t, s, "batch-1", batchRecord("checked")),
		"batch-2": createBatch(t, s, "batch-2", batchRecord("checked")),
	}
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	deadlocksBefore := deadlocksNow(t, observer)

	racers := openStoreWithPool(t, dsn, "admission_race", 2)
	holder := must(observer.Begin(ctx))
	must(holder.Exec(ctx, `SELECT 1 FROM repositories WHERE name = 'r' FOR UPDATE`))
	requests := map[string]ledger.AdmitRequest{
		"batch-1": admitRequest("batch-1", 1, first, second),
		"batch-2": admitRequest("batch-2", 1, second, first),
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	results := map[string]error{}
	for id, request := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := racers.AdmitBatch(ctx, request)
			mu.Lock()
			results[id] = err
			mu.Unlock()
		}()
	}
	watcher := must(pgx.Connect(ctx, dsn))
	defer watcher.Close(ctx)
	waitForBlockedConnections(t, watcher, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	racers.Close()
	waitForNoBackends(t, observer, "admission_race")

	winner, loser := "batch-1", "batch-2"
	if results["batch-1"] != nil {
		winner, loser = loser, winner
	}
	if results[winner] != nil {
		t.Fatalf("neither admission succeeded: %v", results)
	}
	wantRefusal(t, results[loser], ledger.ReasonBatchAdmitted)
	if deadlocks := deadlocksNow(t, observer); deadlocks != deadlocksBefore {
		t.Fatalf("the admissions met %d deadlocks", deadlocks-deadlocksBefore)
	}
	if admitted := must(s.LoadBatch(ctx, "r", winner)); admitted.Admission != "admitted" || len(batchMembers(t, dsn, winner)) != 2 {
		t.Fatalf("the winning batch is %+v", admitted)
	}
	if after := must(s.LoadBatch(ctx, "r", loser)); !reflect.DeepEqual(after, batches[loser]) || len(batchMembers(t, dsn, loser)) != 0 {
		t.Fatalf("the refused batch changed: %+v", after)
	}
}
