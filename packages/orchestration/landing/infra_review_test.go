package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

func TestReviewBaseInfrastructureFailureDoesNotBecomeBlame(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{
		"tree-one":  {Green: false, Output: "✖ [1/1] Building Alpha.Mod\nerror: Alpha/Mod.lean:1:1: Unknown identifier `gone`\n"},
		"tree-base": {InfraClass: "timeout"},
	}}
	runner := testRunner(ledger, builder)
	runner.ModuleRoots = []string{"Alpha"}
	runner.InfraRetries = 0

	batch, err := runner.Run(context.Background(), Input{ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.Reason, "infra"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if len(ledger.returned) != 0 {
		t.Fatalf("returned = %#v, want none", ledger.returned)
	}
}
