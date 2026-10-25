package graphcheck_test

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
)

// A module that builds under Lake only with the package's leanOptions compiles in the check too: the check
// passes Lean the options Lake gave the base build.
func TestC1CompilesWithTheLakeOptionsOfTheBuild(t *testing.T) {
	f := graphchecktest.NewVariant(t, graphchecktest.Variant{Lakefile: "\n[leanOptions]\nmaxRecDepth = 8192\n"})
	tree := f.Edit(t, "Lib/Deep.lean", "",
		"import Lib.Core\n\ntheorem big : (List.replicate 2000 1).length = 2000 := by decide\n")
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Names(context.Background(), c, idx, f.Cfg)
	if err != nil {
		t.Fatalf("Names = %v, want Lib/Deep.lean compiled with maxRecDepth = 8192", err)
	}
	if c1 := ruleHits(hits, "C1"); len(c1) != 0 {
		t.Fatalf("C1 findings %v for a module that builds under Lake", c1)
	}
}
