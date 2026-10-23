package ledger_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestL4PerContractReviews(t *testing.T) {
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
			request := ledger.LandRequest{
				Repo:             "r",
				Migration:        migration.ID,
				Generation:       migration.Generation,
				Commit:           "commit-1",
				ChangedPaths:     []string{"a.lean"},
				Current:          reads,
				ChangedContracts: changes,
				Head:             "head-1",
				ReviewedHead:     test.reviewedHead,
				Reviews:          test.reviews,
			}
			check := must(s.Check(ctx, l4CheckRequest(request)))
			if test.wantRefusal {
				if check.Pass || len(check.Violations) != 1 || check.Violations[0].Kind != ledger.ReasonStatementUnreviewed {
					t.Fatalf("check result: %+v", check)
				}
				for _, field := range test.messageFields {
					if !strings.Contains(check.Violations[0].Message, field) {
						t.Errorf("check message %q does not contain %q", check.Violations[0].Message, field)
					}
				}
				refusal := wantRefusal(t, l4Land(s, request), ledger.ReasonStatementUnreviewed)
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

func TestL4UpstreamProvenanceStillNeedsAuthorizationAndApproval(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must(s.EnsureRepo(ctx, "r", 3, ledger.WithUpstreamOwner("upstream")))

	denied := must(open(t, s, "M-denied", "alice", []string{"a.lean"}, nil))
	deniedRequest := ledger.LandRequest{
		Repo: "r", Migration: denied.ID, Generation: denied.Generation, Commit: "commit-denied",
		ChangedPaths: []string{"a.lean"}, Current: map[string]string{}, Upstream: "upstream-commit",
	}
	_, err := s.Ready(ctx, ledger.ReadyRequest{
		Repo: "r", ID: denied.ID, Generation: denied.Generation, Head: "head-denied", Upstream: "upstream-commit",
	})
	wantRefusal(t, err, ledger.ReasonUpstreamNotAllowed)
	deniedCheck := must(s.Check(ctx, l4CheckRequest(deniedRequest)))
	if deniedCheck.Pass || len(deniedCheck.Violations) != 1 || deniedCheck.Violations[0].Kind != ledger.ReasonUpstreamNotAllowed {
		t.Fatalf("denied upstream check: %+v", deniedCheck)
	}
	wantRefusal(t, l4Land(s, deniedRequest), ledger.ReasonUpstreamNotAllowed)

	must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
	unreviewed := must(open(t, s, "M-unreviewed", "upstream", []string{"b.lean"}, map[string]string{"C": "hash-1"}))
	unreviewedRequest := ledger.LandRequest{
		Repo: "r", Migration: unreviewed.ID, Generation: unreviewed.Generation, Commit: "commit-unreviewed",
		ChangedPaths: []string{"b.lean"}, Current: map[string]string{"C": "hash-1"}, ChangedContracts: map[string]string{"C": "hash-2"},
		Head: "head-unreviewed", Upstream: "upstream-commit",
	}
	if _, err := s.Ready(ctx, ledger.ReadyRequest{
		Repo: "r", ID: unreviewed.ID, Generation: unreviewed.Generation, Head: "head-unreviewed",
		ChangedContracts: map[string]string{"C": "hash-2"}, Upstream: "upstream-commit",
	}); err != nil {
		t.Fatalf("ready accepts upstream owner provenance: %v", err)
	}
	unreviewedCheck := must(s.Check(ctx, l4CheckRequest(unreviewedRequest)))
	if unreviewedCheck.Pass || len(unreviewedCheck.Violations) != 1 || unreviewedCheck.Violations[0].Kind != ledger.ReasonStatementUnreviewed {
		t.Fatalf("unreviewed upstream check: %+v", unreviewedCheck)
	}
	wantRefusal(t, l4Land(s, unreviewedRequest), ledger.ReasonStatementUnreviewed)

	must(s.RegisterContract(ctx, "r", "P", "alice", "hash-p1", true))
	pinned := must(open(t, s, "M-pinned", "upstream", []string{"c.lean"}, map[string]string{"P": "hash-p1"}))
	pinnedRequest := ledger.LandRequest{
		Repo: "r", Migration: pinned.ID, Generation: pinned.Generation, Commit: "commit-pinned",
		ChangedPaths: []string{"c.lean"}, Current: map[string]string{"P": "hash-p1"}, ChangedContracts: map[string]string{"P": "hash-p2"},
		Head: "head-pinned", Upstream: "upstream-commit", Reviews: []ledger.Review{{Contract: "P", Reviewer: "alice", Head: "head-pinned"}},
	}
	pinnedCheck := must(s.Check(ctx, l4CheckRequest(pinnedRequest)))
	if pinnedCheck.Pass || len(pinnedCheck.Violations) != 1 || pinnedCheck.Violations[0].Kind != ledger.ReasonPinned {
		t.Fatalf("pinned upstream check: %+v", pinnedCheck)
	}
	wantRefusal(t, l4Land(s, pinnedRequest), ledger.ReasonPinned)
}

func TestL4ContractsRecordTheChangeThatAdvancedThem(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		migration  string
		owner      string
		proof      string
		reviews    []ledger.Review
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
			name:      "upstream",
			migration: "M-upstream",
			owner:     "upstream",
			reviews:   []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}},
			upstream:  "upstream-commit",
			wantChange: map[string]any{
				"upstream": "upstream-commit",
				"reviews":  []any{map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newStore(t)
			if test.upstream != "" {
				must(s.EnsureRepo(ctx, "r", 3, ledger.WithUpstreamOwner("upstream")))
			}
			must(s.RegisterContract(ctx, "r", "C", "alice", "hash-1", false))
			migration := must(open(t, s, test.migration, test.owner, []string{"a.lean"}, map[string]string{"C": "hash-1"}))
			request := ledger.LandRequest{
				Repo: "r", Migration: migration.ID, Generation: migration.Generation, Commit: "commit-1",
				ChangedPaths: []string{"a.lean"}, Current: map[string]string{"C": "hash-1"}, ChangedContracts: map[string]string{"C": "hash-2"},
				Head: "head-1", CompatibilityProof: test.proof, Reviews: test.reviews, Upstream: test.upstream,
			}
			if _, err := s.Land(ctx, request); err != nil {
				t.Fatal(err)
			}
			contract := must(s.Contract(ctx, "r", "C"))
			assertL4JSON(t, contract.LastChange, test.wantChange)
			contracts := must(s.Contracts(ctx, "r"))
			if len(contracts) != 1 {
				t.Fatalf("contracts: %+v", contracts)
			}
			assertL4JSON(t, contracts[0].LastChange, test.wantChange)
		})
	}
}

func l4CheckRequest(request ledger.LandRequest) ledger.CheckRequest {
	return ledger.CheckRequest{
		Repo: request.Repo, ID: request.Migration, Generation: request.Generation, Commit: request.Commit,
		Changed: request.ChangedPaths, Current: request.Current, ChangedContracts: request.ChangedContracts,
		ApprovedPinned: request.ApprovedPinned, Head: request.Head, CompatibilityProof: request.CompatibilityProof,
		ReviewedHead: request.ReviewedHead, Reviews: request.Reviews, Upstream: request.Upstream,
	}
}

func l4Land(s *ledger.Store, request ledger.LandRequest) error {
	_, err := s.Land(context.Background(), request)
	return err
}

func assertL4JSON(t *testing.T, got ledger.LastChange, want any) {
	t.Helper()
	var actual any
	if err := json.Unmarshal([]byte(got), &actual); err != nil {
		t.Fatalf("last change JSON %q: %v", got, err)
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
