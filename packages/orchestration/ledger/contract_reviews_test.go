package ledger_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestPerContractReviews(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		owners        map[string]string
		reviewedHead  string
		reviews       []ledger.Review
		wantRefusal   bool
		messageFields []string
	}{
		{
			name:         "one owner accepts a reviewed head",
			owners:       map[string]string{"C": "alice", "D": "alice"},
			reviewedHead: "head-1",
		},
		{
			name:          "two owners reject one reviewed head",
			owners:        map[string]string{"C": "alice", "D": "bob"},
			reviewedHead:  "head-1",
			wantRefusal:   true,
			messageFields: []string{"C", "D", "same owner"},
		},
		{
			name:   "two owners accept one review each",
			owners: map[string]string{"C": "alice", "D": "bob"},
			reviews: []ledger.Review{
				{Contract: "C", Reviewer: "alice", Head: "head-1"},
				{Contract: "D", Reviewer: "bob", Head: "head-1"},
			},
		},
		{
			name:        "wrong reviewer does not clear a contract",
			owners:      map[string]string{"C": "alice"},
			reviews:     []ledger.Review{{Contract: "C", Reviewer: "bob", Head: "head-1"}},
			wantRefusal: true,
		},
		{
			name:        "wrong review head does not clear a contract",
			owners:      map[string]string{"C": "alice"},
			reviews:     []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-0"}},
			wantRefusal: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			reads, changes := map[string]string{}, map[string]string{}
			for name, owner := range test.owners {
				must(s.RegisterContract(ctx, "r", name, owner, "hash-1", false))
				reads[name] = "hash-1"
				changes[name] = "hash-2"
			}
			migration := must(open(t, s, "M-1", "alice", []string{"a.lean"}, reads))
			request := ledger.LandRequest{Repo: "r", Migration: migration.ID, Generation: migration.Generation, Commit: "commit-1", ChangedPaths: []string{"a.lean"}, Current: reads, StatementRecords: ledger.StatementRecords{ChangedContracts: changes, Head: "head-1", ReviewedHead: test.reviewedHead, Reviews: test.reviews}}
			check := must(s.Check(ctx, checkRequestFor(request)))
			if test.wantRefusal {
				if check.Pass || len(check.Violations) != 1 || check.Violations[0].Kind != ledger.ReasonStatementUnreviewed {
					t.Fatalf("check result: %+v", check)
				}
				for _, field := range test.messageFields {
					if !strings.Contains(check.Violations[0].Message, field) {
						t.Errorf("check message %q does not contain %q", check.Violations[0].Message, field)
					}
				}
				refusal := wantRefusal(t, landError(s, request), ledger.ReasonStatementUnreviewed)
				for _, field := range test.messageFields {
					if !strings.Contains(refusal.Message, field) {
						t.Errorf("land message %q does not contain %q", refusal.Message, field)
					}
				}
				return
			}
			if !check.Pass {
				t.Fatalf("check result: %+v", check)
			}
			if _, err := s.Land(ctx, request); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestUpstreamProvenanceNeitherClearsNorBlocks catches upstream provenance that acts as an authorization: a migration
// of any owner may carry an upstream commit through ready, check and land, and the commit still clears neither the
// statement rule nor a pinned contract.
func TestUpstreamProvenanceNeitherClearsNorBlocks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	provenance := must(open(t, s, "M-provenance", "alice", []string{"a.lean"}, nil))
	provenanceRequest := ledger.LandRequest{Repo: "r", Migration: provenance.ID, Generation: provenance.Generation, Commit: "commit-provenance", ChangedPaths: []string{"a.lean"}, Current: map[string]string{}, StatementRecords: ledger.StatementRecords{Upstream: "upstream-commit"}}
	must(s.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: provenance.ID, Generation: provenance.Generation, StatementRecords: ledger.StatementRecords{Head: "head-provenance", Upstream: "upstream-commit"}}))
	if check := must(s.Check(ctx, checkRequestFor(provenanceRequest))); !check.Pass {
		t.Fatalf("upstream provenance check: %+v", check)
	}
	must(s.Land(ctx, provenanceRequest))

	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	unreviewed := must(open(t, s, "M-unreviewed", "bob", []string{"b.lean"}, map[string]string{"C": "hash-1"}))
	unreviewedRequest := ledger.LandRequest{Repo: "r", Migration: unreviewed.ID, Generation: unreviewed.Generation, Commit: "commit-unreviewed", ChangedPaths: []string{"b.lean"}, Current: map[string]string{"C": "hash-1"}, StatementRecords: ledger.StatementRecords{ChangedContracts: map[string]string{"C": "hash-2"}, Head: "head-unreviewed", Upstream: "upstream-commit"}}
	if _, err := s.Ready(ctx, ledger.ReadyRequest{Repo: "r", ID: unreviewed.ID, Generation: unreviewed.Generation, StatementRecords: ledger.StatementRecords{Head: "head-unreviewed", ChangedContracts: map[string]string{"C": "hash-2"}, Upstream: "upstream-commit"}}); err != nil {
		t.Fatalf("ready of an unreviewed statement change with upstream provenance: %v", err)
	}
	unreviewedCheck := must(s.Check(ctx, checkRequestFor(unreviewedRequest)))
	if unreviewedCheck.Pass || len(unreviewedCheck.Violations) != 1 || unreviewedCheck.Violations[0].Kind != ledger.ReasonStatementUnreviewed {
		t.Fatalf("unreviewed upstream check: %+v", unreviewedCheck)
	}
	wantRefusal(t, landError(s, unreviewedRequest), ledger.ReasonStatementUnreviewed)

	must(s.RegisterContract(ctx, "r", "P", "alice", "hash-p1", true))
	pinned := must(open(t, s, "M-pinned", "upstream", []string{"c.lean"}, map[string]string{"P": "hash-p1"}))
	pinnedRequest := ledger.LandRequest{Repo: "r", Migration: pinned.ID, Generation: pinned.Generation, Commit: "commit-pinned", ChangedPaths: []string{"c.lean"}, Current: map[string]string{"P": "hash-p1"}, StatementRecords: ledger.StatementRecords{ChangedContracts: map[string]string{"P": "hash-p2"}, Head: "head-pinned", Upstream: "upstream-commit", Reviews: []ledger.Review{{Contract: "P", Reviewer: "alice", Head: "head-pinned"}}}}
	pinnedCheck := must(s.Check(ctx, checkRequestFor(pinnedRequest)))
	if pinnedCheck.Pass || len(pinnedCheck.Violations) != 1 || pinnedCheck.Violations[0].Kind != ledger.ReasonPinned {
		t.Fatalf("pinned upstream check: %+v", pinnedCheck)
	}
	wantRefusal(t, landError(s, pinnedRequest), ledger.ReasonPinned)
}

func TestContractsRecordTheChangeThatAdvancedThem(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		migration  string
		owner      string
		proof      string
		reviews    []ledger.Review
		reviewed   string
		upstream   string
		wantChange any
	}{
		{
			name:      "proof",
			migration: "M-proof",
			owner:     "alice",
			proof:     "Spec.C.compat",
			wantChange: map[string]any{
				"proof": "Spec.C.compat",
			},
		},
		{
			name:      "review",
			migration: "M-review",
			owner:     "alice",
			reviews:   []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			wantChange: map[string]any{
				"review": map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "reviewed head shorthand",
			migration: "M-reviewed-head",
			owner:     "alice",
			reviewed:  "head-1",
			wantChange: map[string]any{
				"review": map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "upstream reviewed head shorthand",
			migration: "M-upstream-reviewed-head",
			owner:     "upstream",
			reviewed:  "head-1",
			upstream:  "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"review":   map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "upstream",
			migration: "M-upstream",
			owner:     "upstream",
			reviews:   []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			upstream:  "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"review":   map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "upstream reviewed head with an unrelated review",
			migration: "M-upstream-unrelated",
			owner:     "upstream",
			reviews:   []ledger.Review{{Contract: "Other", Reviewer: "bob", Head: "head-1"}},
			reviewed:  "head-1",
			upstream:  "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"review":   map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "upstream explicit review wins over the reviewed head",
			migration: "M-upstream-explicit",
			owner:     "upstream",
			reviews: []ledger.Review{
				{Contract: "Other", Reviewer: "bob", Head: "head-1"},
				{Contract: "C", Reviewer: "alice", Head: "head-1"},
			},
			reviewed: "head-1",
			upstream: "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"review":   map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "explicit review wins over the reviewed head",
			migration: "M-explicit",
			owner:     "alice",
			reviews:   []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			reviewed:  "head-1",
			wantChange: map[string]any{
				"review": map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"},
			},
		},
		{
			name:      "upstream proof",
			migration: "M-upstream-proof",
			owner:     "upstream",
			proof:     "Spec.C.compat",
			reviews:   []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			upstream:  "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"proof":    "Spec.C.compat",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
			migration := must(open(t, s, test.migration, test.owner, []string{"a.lean"}, map[string]string{"C": "hash-1"}))
			request := ledger.LandRequest{Repo: "r", Migration: migration.ID, Generation: migration.Generation, Commit: "commit-1", ChangedPaths: []string{"a.lean"}, Current: map[string]string{"C": "hash-1"}, StatementRecords: ledger.StatementRecords{ChangedContracts: map[string]string{"C": "hash-2"}, Head: "head-1", CompatibilityProof: test.proof, Reviews: test.reviews, ReviewedHead: test.reviewed, Upstream: test.upstream}}
			if _, err := s.Land(ctx, request); err != nil {
				t.Fatal(err)
			}
			contract := must(s.Contract(ctx, "r", "C"))
			assertLastChange(t, contract.LastChange, test.wantChange)
			contracts := must(s.Contracts(ctx, "r"))
			if len(contracts) != 1 {
				t.Fatalf("contracts: %+v", contracts)
			}
			assertLastChange(t, contracts[0].LastChange, test.wantChange)
		})
	}
}

// TestContractJSONReadsBack lands one contract for each kind of last_change record, and leaves one unchanged, then
// writes each contract as the command line does and reads it back: every record decodes into a ledger.Contract equal
// to the one the ledger returned.
func TestContractJSONReadsBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	review := func(name string) []ledger.Review {
		return []ledger.Review{{Contract: name, Reviewer: "alice", Head: "head-" + name}}
	}
	for _, change := range []struct {
		name, owner, proof, reviewed, upstream string
		reviews                                []ledger.Review
	}{
		{name: "Proof", owner: "alice", proof: "Spec.Proof.compat"},
		{name: "Review", owner: "alice", reviews: review("Review")},
		{name: "ReviewedHead", owner: "alice", reviewed: "head-ReviewedHead"},
		{name: "UpstreamProof", owner: "upstream", proof: "Spec.UpstreamProof.compat", upstream: "upstream-commit"},
		{name: "UpstreamReview", owner: "upstream", reviews: review("UpstreamReview"), upstream: "upstream-commit"},
	} {
		must(s.RegisterContract(ctx, "r", change.name, "alice", "hash-1", false))
		migration := must(open(t, s, "M-"+change.name, change.owner, []string{change.name + ".lean"},
			map[string]string{change.name: "hash-1"}))
		must(s.Land(ctx, ledger.LandRequest{Repo: "r", Migration: migration.ID, Generation: migration.Generation, Commit: "commit-" + change.name, ChangedPaths: []string{change.name + ".lean"}, Current: map[string]string{change.name: "hash-1"}, StatementRecords: ledger.StatementRecords{ChangedContracts: map[string]string{change.name: "hash-2"}, Head: "head-" + change.name, CompatibilityProof: change.proof, ReviewedHead: change.reviewed, Reviews: change.reviews, Upstream: change.upstream}}))
	}
	must(s.RegisterContract(ctx, "r", "Unchanged", "alice", "hash-1", false))

	contracts := must(s.Contracts(ctx, "r"))
	if len(contracts) != 6 {
		t.Fatalf("contracts: %+v", contracts)
	}
	for _, contract := range contracts {
		encoded, err := json.Marshal(contract)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ledger.Contract
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("%s does not read back from %s: %v", contract.Name, encoded, err)
		}
		contract.UpdatedAt, decoded.UpdatedAt = contract.UpdatedAt.UTC(), decoded.UpdatedAt.UTC()
		if !reflect.DeepEqual(decoded, contract) {
			t.Fatalf("%s reads back as %+v from %s, want %+v", contract.Name, decoded, encoded, contract)
		}
	}
}

// TestMixedOwnersRecordEachExplicitReview lands two contracts of two owners with both a reviewed head and each
// owner's explicit review. The reviewed head clears neither, because the owners differ, so the explicit reviews clear
// them, and each contract's last_change records its own owner's review rather than a review derived from the reviewed
// head.
func TestMixedOwnersRecordEachExplicitReview(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	must(s.RegisterContract(ctx, "r", "D", "bob", "hash-1", false))
	reads := map[string]string{"C": "hash-1", "D": "hash-1"}
	migration := must(open(t, s, "M-mixed", "alice", []string{"a.lean"}, reads))
	request := ledger.LandRequest{Repo: "r", Migration: migration.ID, Generation: migration.Generation, Commit: "commit-1", ChangedPaths: []string{"a.lean"}, Current: reads, StatementRecords: ledger.StatementRecords{ChangedContracts: map[string]string{"C": "hash-2", "D": "hash-2"}, Head: "head-1", ReviewedHead: "head-1", Reviews: []ledger.Review{
		{Contract: "D", Reviewer: "bob", Head: "head-1"},
		{Contract: "C", Reviewer: "alice", Head: "head-1"},
	}}}
	if check := must(s.Check(ctx, checkRequestFor(request))); !check.Pass {
		t.Fatalf("check of a mixed-owner change with each owner's review: %+v", check)
	}
	if _, err := s.Land(ctx, request); err != nil {
		t.Fatalf("land of a mixed-owner change with each owner's review: %v", err)
	}
	want := map[string]any{
		"C": map[string]any{"review": map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"}},
		"D": map[string]any{"review": map[string]any{"contract": "D", "reviewer": "bob", "head": "head-1"}},
	}
	contracts := must(s.Contracts(ctx, "r"))
	if len(contracts) != 2 {
		t.Fatalf("contracts: %+v", contracts)
	}
	for _, contract := range contracts {
		assertLastChange(t, contract.LastChange, want[contract.Name])
		assertLastChange(t, must(s.Contract(ctx, "r", contract.Name)).LastChange, want[contract.Name])
	}
}

func checkRequestFor(request ledger.LandRequest) ledger.CheckRequest {
	return ledger.CheckRequest{LandRequest: ledger.LandRequest{Repo: request.Repo, Migration: request.Migration, Generation: request.Generation, Commit: request.Commit, ChangedPaths: request.ChangedPaths, Current: request.Current, StatementRecords: ledger.StatementRecords{ChangedContracts: request.ChangedContracts, ApprovedPinned: request.ApprovedPinned, Head: request.Head, CompatibilityProof: request.CompatibilityProof, ReviewedHead: request.ReviewedHead, Reviews: request.Reviews, Upstream: request.Upstream}}}
}

func landError(s *ledger.Store, request ledger.LandRequest) error {
	_, err := s.Land(context.Background(), request)
	return err
}

// assertLastChange compares the JSON that got writes with want, the record as the command line prints it.
func assertLastChange(t *testing.T, got *ledger.LastChange, want any) {
	t.Helper()
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err := json.Unmarshal(data, &actual); err != nil {
		t.Fatalf("last change JSON %q: %v", data, err)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var expected any
	if err := json.Unmarshal(encoded, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("last change %#v, want %#v", actual, expected)
	}
}
