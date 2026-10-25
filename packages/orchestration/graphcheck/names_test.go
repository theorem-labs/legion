package graphcheck_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// open opens the fixture's candidate and index and closes them when the test ends.
func open(t *testing.T, f *graphchecktest.Fixture, tree string) (*graphcheck.Candidate, *declindex.Store) {
	t.Helper()
	c, idx, err := graphcheck.Open(context.Background(), f.Cfg, f.Base, tree)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = idx.Close()
	})
	return c, idx
}

func names(t *testing.T, f *graphchecktest.Fixture, tree string) []landing.Finding {
	t.Helper()
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Names(context.Background(), c, idx, f.Cfg)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	return hits
}

func find(hits []landing.Finding, verdict string, kind blame.Kind, decl, path, rule string) (landing.Finding, bool) {
	for _, h := range hits {
		if h.Verdict == verdict && h.Kind == kind && h.Declaration == decl && h.Path == path && strings.HasPrefix(h.Reason, rule+":") {
			return h, true
		}
	}
	return landing.Finding{}, false
}

func ruleHits(hits []landing.Finding, rule string) []landing.Finding {
	var out []landing.Finding
	for _, h := range hits {
		if strings.HasPrefix(h.Reason, rule+":") {
			out = append(out, h)
		}
	}
	return out
}

func TestNamesRefusesAPrivatisedDeclarationThatAnUnchangedModuleUses(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def helper", "private def helper")
	hits := names(t, f, tree)
	h, ok := find(hits, landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Use.lean", "N1")
	if !ok || h.Line != 5 || h.Col != 36 {
		t.Fatalf("no N1 refusal at Lib/Use.lean:5:36 in %v", hits)
	}
	if want := "N1: Lib/Core.lean no longer declares Lib.Core.helper publicly; 3 uses in 2 unchanged modules, first here"; h.Reason != want {
		t.Fatalf("reason %q, want %q", h.Reason, want)
	}
	if _, ok := find(hits, landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Open.lean", "N1"); !ok {
		t.Fatalf("no N1 refusal of the use through open in Lib/Open.lean in %v", hits)
	}
}

func TestNamesRefusesARemovedFieldThatAProjectionNames(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "  val : Nat", "  value : Nat")
	h, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Field, "Lib.Core.Box.val", "Lib/Use.lean", "N1")
	if !ok || h.Line != 8 {
		t.Fatal("no N1 field refusal at Lib/Use.lean:8")
	}
	if want := "N1: structure Lib.Core.Box in Lib/Core.lean no longer has field val"; !strings.HasPrefix(h.Reason, want) {
		t.Fatalf("reason %q, want prefix %q", h.Reason, want)
	}
}

// The fixture's Make.ilean records `{ val := 1 }` as a use of Lib.Core.Box.val.
func TestNamesRefusesARemovedFieldThatAStructureInstanceNames(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "  val : Nat", "  value : Nat")
	if h, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Field, "Lib.Core.Box.val", "Lib/Make.lean", "N1"); !ok || h.Line != 5 {
		t.Fatal("no N1 field refusal at Lib/Make.lean:5")
	}
}

// Another declaration has the removed name's last component, but Lean decides: Lib/Open.lean opens only Lib.Core, so
// its `helper` resolves to nothing, the module does not compile, and N1 refuses it.
func TestNamesRefusesWhatLeanConfirmsDespiteAShortNameElsewhere(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":  {"def helper", "def helper2"},
		"Lib/Other.lean": {"", "namespace Lib.Core.Alt\ndef helper (n : Nat) : Nat := n\nend Lib.Core.Alt\n"},
		"Lib.lean":       {"import Lib.Open", "import Lib.Open\nimport Lib.Other"},
	})
	hits := names(t, f, tree)
	if _, ok := find(hits, landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Open.lean", "N1"); !ok {
		t.Fatalf("no N1 refusal at Lib/Open.lean in %v", hits)
	}
}

// A private declaration is invisible to other modules, so its short name makes no removal uncertain.
func TestNamesRefusesARemovalWhoseShortNameOnlyAPrivateDeclarationHas(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":  {"def helper", "def helper2"},
		"Lib/Other.lean": {"", "namespace Lib.Core.Alt\nprivate def helper (n : Nat) : Nat := n\nend Lib.Core.Alt\n"},
		"Lib.lean":       {"import Lib.Open", "import Lib.Open\nimport Lib.Other"},
	})
	if _, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Open.lean", "N1"); !ok {
		t.Fatal("no N1 refusal at Lib/Open.lean")
	}
}

// A field a parent structure now gives still resolves: Lean compiles Lib/Use.lean's `b.val`, so N1 says nothing
// there.
func TestNamesSaysNothingOfAFieldAParentStillGives(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "structure Box where\n  val : Nat", "structure Base where\n  val : Nat\n\nstructure Box extends Base where\n  value : Nat")
	for _, h := range names(t, f, tree) {
		if h.Path == "Lib/Use.lean" && h.Declaration == "Lib.Core.Box.val" {
			t.Fatalf("finding %+v for a field the parent structure gives", h)
		}
	}
}

func TestNamesRefusesASecondDeclarationInABuiltModule(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/New.lean": {"", "namespace Lib.Core\ndef helper (n : Nat) : Nat := n\nend Lib.Core\n"},
		"Lib.lean":     {"import Lib.Open", "import Lib.Open\nimport Lib.New"},
	})
	h, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Added, "Lib.Core.helper", "Lib/New.lean", "N2")
	if !ok || h.Line != 2 || h.Col != 1 {
		t.Fatal("no N2 refusal at Lib/New.lean:2:1")
	}
	if want := "N2: Lib.Core.helper is also declared in Lib/Core.lean:3; a built module imports both"; h.Reason != want {
		t.Fatalf("reason %q, want %q", h.Reason, want)
	}
}

func TestNamesIgnoresASecondDeclarationNoBuildReaches(t *testing.T) {
	f := graphchecktest.New(t)
	f.Cfg.RootNewModules = false
	tree := f.Edit(t, "Lib/New.lean", "", "namespace Lib.Core\ndef helper (n : Nat) : Nat := n\nend Lib.Core\n")
	if _, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Added, "Lib.Core.helper", "Lib/New.lean", "N2"); ok {
		t.Fatal("N2 refuses a module no build root reaches")
	}
}

// A module that the build roots no longer import is not built: N1 says nothing about its uses.
func TestNamesIgnoresAUseInAModuleTheBuildNoLongerReaches(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean": {"def helper", "private def helper"},
		"Lib.lean":      {"import Lib.Open\n", ""},
	})
	for _, h := range names(t, f, tree) {
		if h.Path == "Lib/Open.lean" {
			t.Fatalf("finding %+v in a module no build root imports", h)
		}
	}
}

// A declaration moved into a module its users reach through their imports is not removed for them.
func TestNamesIgnoresADeclarationMovedWhereItsUsersReachIt(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":  {"namespace Lib.Core\n\ndef helper (n : Nat) : Nat := n + 1\n", "import Lib.Core2\n\nnamespace Lib.Core\n"},
		"Lib/Core2.lean": {"", "namespace Lib.Core\ndef helper (n : Nat) : Nat := n + 1\nend Lib.Core\n"},
	})
	for _, h := range names(t, f, tree) {
		if h.Declaration == "Lib.Core.helper" {
			t.Fatalf("finding %+v of a moved declaration its users still reach", h)
		}
	}
}

// A declaration moved into a module that its users do not import is gone for them: Lean fails them, and N1
// refuses.
func TestNamesRefusesADeclarationMovedWhereItsUsersCannotReachIt(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":     {"def helper (n : Nat) : Nat := n + 1\n", ""},
		"Lib/Separate.lean": {"", "namespace Lib.Core\ndef helper (n : Nat) : Nat := n + 1\nend Lib.Core\n"},
	})
	if _, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Use.lean", "N1"); !ok {
		t.Fatal("no N1 refusal at Lib/Use.lean for a declaration moved out of its reach")
	}
}

func TestNamesRefusesTheUsesOfADeletedFile(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "", "")
	h, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Use.lean", "N1")
	if !ok || h.Line != 5 {
		t.Fatal("no N1 refusal at Lib/Use.lean:5")
	}
	if !strings.HasPrefix(h.Reason, "N1: Lib/Core.lean no longer declares Lib.Core.helper;") {
		t.Fatalf("reason %q", h.Reason)
	}
}

// An index read from the reference files alone (a corpus index) knows no field, but every declaration: the names
// check still decides, refuses the removed declaration, and reports no field rather than a wrong one.
func TestNamesWithoutTheCompiledPartRefusesDeclarationsAndSkipsFields(t *testing.T) {
	f := graphchecktest.NewWithoutCompiledPart(t)
	tree := f.Edit(t, "Lib/Core.lean", "def helper (n : Nat) : Nat := n + 1\n\ndef size : Nat := 3\n\nstructure Box where\n  val : Nat",
		"private def helper (n : Nat) : Nat := n + 1\n\ndef size : Nat := 3\n\nstructure Box where\n  value : Nat")
	hits := names(t, f, tree)
	if _, ok := find(hits, landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Use.lean", "N1"); !ok {
		t.Fatalf("no N1 refusal of Lib.Core.helper in %v", hits)
	}
	for _, h := range hits {
		if h.Kind == blame.Field {
			t.Fatalf("field finding %+v from an index with no compiled part", h)
		}
	}
}

// A new root module that redeclares a name no built module imports together with the original builds: Lean meets
// two declarations of one name only in one environment, so N2 says nothing.
func TestNamesIgnoresASecondDeclarationNoModuleImportsTogether(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/New.lean", "", "namespace Lib.Core\ndef helper (n : Nat) : Nat := n\nend Lib.Core\n")
	if hits := ruleHits(names(t, f, tree), "N2"); len(hits) != 0 {
		t.Fatalf("N2 findings %v for two modules no built module imports together", hits)
	}
}

// A command macro makes a constant that the reference file records no declaration range for: the constant still
// exists, its users still build, and N1 refuses nothing.
func TestNamesKeepsAConstantACommandMacroMakes(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size : Nat := 3",
		"macro \"mk_size\" : command => `(def $(Lean.mkIdent `size) : Nat := 3)\n\nmk_size")
	for _, h := range names(t, f, tree) {
		if h.Declaration == "Lib.Core.size" {
			t.Fatalf("finding %+v for a constant the macro still makes", h)
		}
	}
}

func TestNamesIsUnableWhenTheIndexIsOfAnotherTree(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size", "def size2")
	var unable *graphcheck.UnableError
	if _, _, err := graphcheck.Open(context.Background(), f.Cfg, f.HeadOf(t, tree), tree); !errors.As(err, &unable) {
		t.Fatalf("Open = %v, want *UnableError", err)
	}
}

func TestNamesAcceptsAnIndexOfAnotherCommitWithTheSameTree(t *testing.T) {
	f := graphchecktest.New(t)
	c, idx, err := graphcheck.Open(context.Background(), f.Cfg, f.SameTreeCommit(t), f.BaseTree(t))
	if err != nil {
		t.Fatalf("Open = %v, want the index of the same tree accepted", err)
	}
	_ = c.Close()
	_ = idx.Close()
}

func TestNamesIsUnableWhenTheIndexIsIncomplete(t *testing.T) {
	f := graphchecktest.NewWithUnreadableModule(t) // golden/Lib/Make.ilean truncated before the refresh
	var unable *graphcheck.UnableError
	if _, _, err := graphcheck.Open(context.Background(), f.Cfg, f.Base, f.BaseTree(t)); !errors.As(err, &unable) {
		t.Fatalf("Open = %v, want *UnableError", err)
	}
}

func TestNamesIsUnableForACommitTheRepositoryLacks(t *testing.T) {
	f := graphchecktest.New(t)
	var unable *graphcheck.UnableError
	missing := strings.Repeat("ab", 20)
	if _, _, err := graphcheck.Open(context.Background(), f.Cfg, f.Base, missing); !errors.As(err, &unable) {
		t.Fatalf("Open with an unknown tree = %v, want *UnableError", err)
	}
	if _, _, err := graphcheck.Open(context.Background(), f.Cfg, missing, f.BaseTree(t)); !errors.As(err, &unable) {
		t.Fatalf("Open with an unknown base = %v, want *UnableError", err)
	}
}
