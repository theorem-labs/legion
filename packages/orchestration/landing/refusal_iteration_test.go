package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestReviewRefusingFirstMemberDoesNotPanic(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{"first": "refused"}}
	runner := testRunner(ledger, &builderFake{})
	batch := Batch{
		Repository: "repo",
		Candidate:  gitx.Result{Tree: "tree"},
		ActiveMembers: []Member{
			member("first"),
			member("second"),
		},
	}

	if err := runner.checkLedger(context.Background(), &batch); err != nil {
		t.Fatalf("checkLedger() error = %v", err)
	}
	if got, want := batch.ActiveMembers, []Member{member("second")}; !sameMembers(got, want) {
		t.Fatalf("active members = %#v, want %#v", got, want)
	}
}
