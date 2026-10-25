package graphcheck_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// A changed module that uses a name the candidate removed does not compile, and C1 refuses it through Lean's own
// error. Lib/Open.lean keeps its use of helper and gains a comment, so that the batch changes it and the check
// compiles it; Lib/Core.lean makes helper private.
func TestC1RefusesAModuleThatUsesARemovedName(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean": {"def helper", "private def helper"},
		"Lib/Open.lean": {"open Lib.Core", "open Lib.Core\n\n-- Lib/Open.lean keeps its use of helper."},
	})
	hits := ruleHits(names(t, f, tree), "C1")
	if len(hits) != 1 {
		t.Fatalf("C1 findings %v, want one", hits)
	}
	h := hits[0]
	if h.Verdict != landing.VerdictRefuse || h.Kind != blame.Unknown || h.Path != "Lib/Open.lean" {
		t.Fatalf("C1 finding %+v, want a refusal of kind unknown in Lib/Open.lean", h)
	}
	if !strings.HasSuffix(h.Declaration, "helper") {
		t.Fatalf("C1 declaration %q, want the name Lean reported for helper", h.Declaration)
	}
	if h.Line < 1 || h.Col < 1 {
		t.Fatalf("C1 finding %+v, want the position of Lean's error", h)
	}
	if !strings.Contains(h.Reason, "helper") {
		t.Fatalf("C1 reason %q, want Lean's first error line", h.Reason)
	}
}

// A compile failure whose errors name no declaration leaves the check unable: the batch gets Lean's reason, and
// no finding says the batch is clean.
func TestC1IsUnableWhenLeanNamesNoName(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Open.lean", "def viaOpen : Nat := helper 2", "def viaOpen : Nat := helper 2\n\ndef broken : Nat := (")
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Names(context.Background(), c, idx, f.Cfg)
	var unable *graphcheck.UnableError
	if !errors.As(err, &unable) {
		t.Fatalf("Names = %v, %v; want an *UnableError", hits, err)
	}
	if !strings.Contains(unable.Reason, "Lib/Open.lean") {
		t.Fatalf("reason %q, want the module that does not compile", unable.Reason)
	}
}

// The check compiles the changed modules and nothing downstream of them: that is what keeps it far shorter than
// the build it runs before.
func TestC1CompilesOnlyTheChangedModules(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size : Nat := 3", "def size : Nat := 4")
	c, _ := open(t, f, tree)
	if compiled := c.CompiledModules(); !slices.Equal(compiled, []string{"Lib.Core"}) {
		t.Fatalf("compiled %v, want only the changed module Lib.Core", compiled)
	}
}

// A changed module that no build target imports is never compiled by the build, so a scratch compile of it must
// refuse nothing: the build would accept the batch.
func TestC1IgnoresAModuleOutsideTheBuildClosure(t *testing.T) {
	f := graphchecktest.New(t)
	f.Cfg.RootNewModules = false
	tree := f.Edit(t, "Lib/Loose.lean", "", "def broken : Nat := (\n")
	hits := names(t, f, tree)
	if len(hits) != 0 {
		t.Fatalf("findings %v for a module no build root imports", hits)
	}
	if compiled := c0(t, f, tree); slices.Contains(compiled, "Lib.Loose") {
		t.Fatalf("compiled %v, want Lib.Loose left alone", compiled)
	}
}

// c0 is the candidate's compiled modules for one tree.
func c0(t *testing.T, f *graphchecktest.Fixture, tree string) []string {
	t.Helper()
	c, _ := open(t, f, tree)
	return c.CompiledModules()
}
