package graphcheck_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// libRoot is the fixture's Lib.lean; a variant that adds a module to the build appends its import.
const libRoot = "import Lib.Core\nimport Lib.Use\nimport Lib.Make\nimport Lib.Open\nimport Lib.Spec.LinkSpec\nimport Lib.Iface\nimport Lib.Drv.Base\n"

func isUnable(t *testing.T, err error) {
	t.Helper()
	var unable *graphcheck.UnableError
	if !errors.As(err, &unable) {
		t.Fatalf("err = %v, want *UnableError", err)
	}
}

// Without the base build's library the candidate cannot be compiled against it: unable (exit 3), not a failure.
func TestAMissingBaseLibraryIsUnable(t *testing.T) {
	f := graphchecktest.New(t)
	if err := os.RemoveAll(filepath.Join(f.Repo, ".lake", "build", "lib", "lean")); err != nil {
		t.Fatal(err)
	}
	tree := f.Edit(t, "Lib/Core.lean", "def helper", "private def helper")
	_, _, err := graphcheck.Open(context.Background(), f.Cfg, f.Base, tree)
	isUnable(t, err)
}

// An unchanged module between two changed ones is rebuilt as the build rebuilds it: its base .olean was elaborated
// against the old version of the module it imports, and compiling the downstream changed module against that stale
// output would fail where the build succeeds.
func TestC1CompilesAgainstARebuiltUnchangedIntermediate(t *testing.T) {
	f := graphchecktest.NewVariant(t, graphchecktest.Variant{Files: map[string]string{
		"Lib/Chain/A.lean": "namespace Lib.Chain\nstructure Old where\n  n : Nat\nstructure New where\n  n : Nat\ndef token : Old := ⟨1⟩\nend Lib.Chain\n",
		"Lib/Chain/B.lean": "import Lib.Chain.A\nnamespace Lib.Chain\ndef pass := token\nend Lib.Chain\n",
		"Lib/Chain/C.lean": "import Lib.Chain.B\nnamespace Lib.Chain\ndef result := pass\nend Lib.Chain\n",
		"Lib.lean":         libRoot + "import Lib.Chain.C\n",
	}})
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Chain/A.lean": {"def token : Old := ⟨1⟩", "def token : New := ⟨1⟩"},
		"Lib/Chain/C.lean": {"def result := pass", "def result : New := pass"},
	})
	if hits := ruleHits(names(t, f, tree), "C1"); len(hits) != 0 {
		t.Fatalf("C1 findings %v for a chain the build rebuilds and accepts", hits)
	}
}

// A use inside a tactic that has a working fallback: the base recorded it, but the unchanged module still builds,
// and Lean, compiling it against the candidate, says so. N1 refuses nothing there.
func TestNamesRefusesNothingWhereATacticFallbackStillBuilds(t *testing.T) {
	f := graphchecktest.NewVariant(t, graphchecktest.Variant{Files: map[string]string{
		"Lib/Fallback.lean": "import Lib.Core\n\ntheorem fallback : True := by\n  first\n  | have h := Lib.Core.helper 0\n    trivial\n  | trivial\n",
		"Lib.lean":          libRoot + "import Lib.Fallback\n",
	}})
	tree := f.Edit(t, "Lib/Core.lean", "def helper", "private def helper")
	hits := names(t, f, tree)
	for _, h := range hits {
		if h.Path == "Lib/Fallback.lean" {
			t.Fatalf("finding %+v in a module whose fallback still builds", h)
		}
	}
	if _, ok := find(hits, landing.VerdictRefuse, blame.Unknown, "Lib.Core.helper", "Lib/Use.lean", "N1"); !ok {
		t.Fatalf("no N1 refusal at Lib/Use.lean in %v", hits)
	}
}

// A new declaration named like a constructor Lean generates for an unchanged structure collides with it wherever
// one built module imports both: Lib imports Lib.Core and Lib.Iface.
func TestNamesRefusesANewDeclarationOfAGeneratedConstructor(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Iface.lean", "namespace Lib.Iface", "def Lib.Core.Box.mk : Nat := 1\n\nnamespace Lib.Iface")
	if _, ok := find(names(t, f, tree), landing.VerdictRefuse, blame.Added, "Lib.Core.Box.mk", "Lib/Iface.lean", "N2"); !ok {
		t.Fatal("no N2 refusal of Lib.Core.Box.mk in Lib/Iface.lean")
	}
}

// A module that imports another Lake package of the project compiles here as it does in the build: the search
// path is Lake's own.
func TestACandidateModuleCompilesAgainstAnotherPackage(t *testing.T) {
	f := graphchecktest.NewVariant(t, graphchecktest.Variant{
		Lakefile: "\n[[require]]\nname = \"dep\"\npath = \"vendor/Dep\"\n",
		Files: map[string]string{
			"vendor/Dep/lakefile.toml": "name = \"dep\"\ndefaultTargets = [\"Dep\"]\n\n[[lean_lib]]\nname = \"Dep\"\n",
			"vendor/Dep/Dep.lean":      "def Dep.answer : Nat := 42\n",
			"Lib/WithDep.lean":         "import Dep\n\ndef Lib.withDep : Nat := Dep.answer\n",
			"Lib.lean":                 libRoot + "import Lib.WithDep\n",
		},
	})
	tree := f.Edit(t, "Lib/WithDep.lean", "def Lib.withDep : Nat := Dep.answer", "def Lib.withDep : Nat := Dep.answer + 1")
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Names(context.Background(), c, idx, f.Cfg)
	if err != nil || len(ruleHits(hits, "C1")) != 0 {
		t.Fatalf("Names = %v, %v; want a decided check with no C1 finding", hits, err)
	}
	if compiled := c.CompiledModules(); len(compiled) == 0 {
		t.Fatal("nothing compiled")
	}
}
