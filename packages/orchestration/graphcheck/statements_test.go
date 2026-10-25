package graphcheck_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// statements runs Statements on the fixture's candidate tree. S2 compiles the candidate and exports its constants
// with Lean; graphchecktest.New skips the test without a Lean toolchain (graphchecktest.RequireLean).
func statements(t *testing.T, f *graphchecktest.Fixture, tree string) []landing.Finding {
	t.Helper()
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Statements(context.Background(), c, idx, f.Cfg)
	if err != nil {
		t.Fatalf("Statements: %v", err)
	}
	return hits
}

func TestS2FlagsARestatedStructure(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Copy.lean", "", "namespace Lib.Copy\n\nstructure BoxCopy where\n  val : Nat\n  ok : val > 0 := by decide\n\nend Lib.Copy\n")
	hits := statements(t, f, tree)
	h, ok := find(hits, landing.VerdictFlag, blame.Added, "Lib.Copy.BoxCopy", "Lib/Copy.lean", "S2")
	if !ok || h.Line != 3 || h.Col != 1 {
		t.Fatalf("no S2 flag at Lib/Copy.lean:3:1 in %v", hits)
	}
	if want := "S2: restates Lib.Core.Box (Lib/Core.lean:7); a concept proposal goes to the similarity gate first"; h.Reason != want {
		t.Fatalf("reason %q, want %q", h.Reason, want)
	}
	// The constructor restates Box's too, but it is part of the structure, not a declaration of its own.
	if len(ruleHits(hits, "S2")) != 1 {
		t.Fatalf("S2 findings %v, want only the structure's", hits)
	}
}

func TestS2IgnoresANewDeclarationOfAnotherShape(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Count.lean", "", "def count : Nat := 7\n")
	if hits := ruleHits(statements(t, f, tree), "S2"); len(hits) != 0 {
		t.Fatalf("S2 findings %v", hits)
	}
}

// Two new declarations that restate each other are both flagged, each naming the other.
func TestS2FlagsNewDeclarationsThatRestateEachOther(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/A.lean": {"", "def seven : Nat := 7\n"},
		"Lib/B.lean": {"", "def sept : Nat := 7\n"},
	})
	hits := ruleHits(statements(t, f, tree), "S2")
	if len(hits) != 2 || hits[0].Declaration != "seven" || !strings.Contains(hits[0].Reason, "sept (Lib/B.lean:1)") ||
		hits[1].Declaration != "sept" || !strings.Contains(hits[1].Reason, "seven (Lib/A.lean:1)") {
		t.Fatalf("S2 findings %v", hits)
	}
}

// A renamed declaration restates nothing: its old name is gone from the candidate.
func TestS2IgnoresARename(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size : Nat := 3", "def size2 : Nat := 3")
	if hits := ruleHits(statements(t, f, tree), "S2"); len(hits) != 0 {
		t.Fatalf("S2 findings %v for a rename", hits)
	}
}

// A new declaration that restates one its own modified module keeps is flagged: the candidate declares both.
func TestS2FlagsARestatementOfADeclarationOfAChangedModule(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size : Nat := 3\n", "def size : Nat := 3\n\ndef three : Nat := 3\n")
	hits := statements(t, f, tree)
	h, ok := find(hits, landing.VerdictFlag, blame.Added, "Lib.Core.three", "Lib/Core.lean", "S2")
	if !ok || h.Line != 7 || !strings.Contains(h.Reason, "restates Lib.Core.size (Lib/Core.lean:5)") {
		t.Fatalf("no S2 flag at Lib/Core.lean:7 naming Lib.Core.size in %v", hits)
	}
	if len(ruleHits(hits, "S2")) != 1 {
		t.Fatalf("S2 findings %v, want only the new declaration's", hits)
	}
}

// The index's declarations of a changed module are the base's: the candidate supersedes them, so a new declaration
// that restates what a modified module used to declare restates nothing.
func TestS2IgnoresWhatAChangedModuleNoLongerDeclares(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":  {"def size : Nat := 3", "def size : Nat := 4"},
		"Lib/Three.lean": {"", "def three : Nat := 3\n"},
	})
	if hits := ruleHits(statements(t, f, tree), "S2"); len(hits) != 0 {
		t.Fatalf("S2 findings %v against a statement the candidate no longer has", hits)
	}
}
