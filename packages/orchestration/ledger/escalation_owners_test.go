package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// repositoryConfigured reads the escalation owners of each repository.configured event of repository r, oldest first.
func repositoryConfigured(t *testing.T, s *ledger.Store) [][]string {
	t.Helper()
	configured := [][]string{}
	for _, e := range must(s.Events(context.Background(), "r", 0, "r")) {
		if e.Kind != "repository.configured" {
			continue
		}
		var data struct {
			EscalationOwners []string `json:"escalation_owners"`
		}
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		configured = append(configured, data.EscalationOwners)
	}
	return configured
}

// EnsureRepo stores the escalation owners it is given as a sorted set, keeps them when it is given none, replaces
// them when it is given others, and clears them when it is given an empty list. Each change appends one
// repository.configured event naming them; a re-ensure with the same values appends nothing.
func TestEnsureRepoConfiguresEscalationOwners(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	owners := func() []string {
		t.Helper()
		return must(s.Status(ctx, "r")).Repository.EscalationOwners
	}
	if got := owners(); len(got) != 0 {
		t.Fatalf("escalation owners of a new repository = %q, want none", got)
	}

	set := must(s.EnsureRepo(ctx, "r", 3, ledger.WithEscalationOwners("steward:b", "steward:a", "steward:b")))
	if want := []string{"steward:a", "steward:b"}; !slices.Equal(set.EscalationOwners, want) || !slices.Equal(owners(), want) {
		t.Fatalf("escalation owners = %q (returned %q), want %q", owners(), set.EscalationOwners, want)
	}
	must(s.EnsureRepo(ctx, "r", 3, ledger.WithEscalationOwners("steward:a", "steward:b")))
	kept := must(s.EnsureRepo(ctx, "r", 2))
	if want := []string{"steward:a", "steward:b"}; !slices.Equal(kept.EscalationOwners, want) || !slices.Equal(owners(), want) {
		t.Fatalf("escalation owners after an ensure without them = %q, want %q", owners(), want)
	}
	must(s.EnsureRepo(ctx, "r", 2, ledger.WithUpstreamOwner("upstream"), ledger.WithEscalationOwners("steward:c")))
	if got := must(s.Status(ctx, "r")).Repository; !slices.Equal(got.EscalationOwners, []string{"steward:c"}) || got.UpstreamOwner != "upstream" {
		t.Fatalf("repository after replacing its escalation owners = %+v", got)
	}
	must(s.EnsureRepo(ctx, "r", 2, ledger.WithEscalationOwners()))
	if got := owners(); len(got) != 0 {
		t.Fatalf("escalation owners after clearing = %q, want none", got)
	}

	want := [][]string{{}, {"steward:a", "steward:b"}, {"steward:a", "steward:b"}, {"steward:c"}, {}}
	if got := repositoryConfigured(t, s); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("repository.configured escalation owners = %q, want %q", got, want)
	}
}

// An empty or blank escalation owner names nobody who could take an escalation, so it is invalid and changes nothing.
func TestEnsureRepoRefusesABlankEscalationOwner(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	must(s.EnsureRepo(ctx, "r", 3, ledger.WithEscalationOwners("steward:a")))
	for _, owners := range [][]string{{""}, {" \t"}, {"steward:b", ""}} {
		_, err := s.EnsureRepo(ctx, "r", 1, ledger.WithEscalationOwners(owners...))
		var invalid *ledger.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("escalation owners %q: want invalid input, got %v", owners, err)
		}
	}
	got := must(s.Status(ctx, "r")).Repository
	if !slices.Equal(got.EscalationOwners, []string{"steward:a"}) || got.MaxLandingRounds != 3 {
		t.Fatalf("repository after refused ensures = %+v", got)
	}
}
