package gitx

import (
	"context"
	"testing"
)

func TestReviewRebuildDropsCarrierOfExcludedMember(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	excluded := repo.branch(base, "bad", "bad\n")
	carrier := repo.branch(excluded, "carrier", "carrier\n")

	result, err := Rebuild(context.Background(), repo.path, base, []Member{{ID: "bad", Head: excluded}, {ID: "carrier", Head: carrier}}, map[string]struct{}{"bad": {}})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "carrier", Reason: "carrier"}}; !sameDrops(got, want) {
		t.Fatalf("Rebuild() drops = %#v, want %#v", got, want)
	}
	if got := repo.contents(result.Commit, "bad"); got != "" {
		t.Fatalf("rebuilt tree retains excluded content %q", got)
	}
}
