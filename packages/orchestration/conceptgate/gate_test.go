package conceptgate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

var (
	exporterOnce sync.Once
	exporterBin  string
	exporterErr  error
)

type recordingLibrarian struct{ got []Verdict }

func (r *recordingLibrarian) Review(_ context.Context, v Verdict, _ Proposal) error {
	r.got = append(r.got, v)
	return nil
}

const gateRegistry = `version: 1
imports: [Fixture]
concepts:
  - name: Fx.SpAligned
    home: Fixture.lean
    meaning: Register x2 of a register file is 8-aligned.
    forms:
      - "fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64"
  - name: Fx.Frame
    home: Fixture.lean
    meaning: A window of n bytes at an aligned word.
`

// gateForTest builds the fixture tree, its index with the registry's facts, and a gate over them. It needs a
// Lean toolchain (CI installs one); without one it skips.
func gateForTest(t *testing.T) (*Gate, *recordingLibrarian) {
	t.Helper()
	if _, err := exec.LookPath("lake"); err != nil {
		t.Skip("no lake on PATH: the gate tests need a Lean toolchain")
	}
	ctx := context.Background()
	l, err := declindex.FindLean(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	exporterOnce.Do(func() {
		exporterBin, exporterErr = declindex.BuildExporter(ctx, l, "", filepath.Join(os.TempDir(), "conceptgate-test-exporter"))
	})
	if exporterErr != nil {
		t.Fatal(exporterErr)
	}
	tree := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "declindex", "testdata", "lean", "Fixture.lean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "Fixture.lean"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(ctx, l, nil, tree, "Fixture"); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(reg, []byte(gateRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := declindex.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r, err := Load(reg)
	if err != nil {
		t.Fatal(err)
	}
	co := &declindex.CompiledOptions{
		Exporter: &declindex.Exporter{Lean: l, Bin: exporterBin, SearchPath: []string{tree}, Jobs: 1},
		Forms:    r.FormsModule(),
		Dir:      t.TempDir(),
	}
	spec := declindex.Tree{Source: tree, Lib: tree, Roots: declindex.Roots{"Fixture": true}}
	if _, err := s.Refresh(ctx, spec, declindex.RefreshOptions{Commit: "c", Compiled: co}); err != nil {
		t.Fatal(err)
	}
	lib := &recordingLibrarian{}
	return &Gate{Index: s, Tree: spec, Registry: reg, Compiled: co, Librarian: lib, WorkDir: t.TempDir()}, lib
}

func proposal(id, name, body string) Proposal {
	return Proposal{ID: id, Concept: Concept{Name: name, Home: "New.lean", Meaning: "A proposed concept."},
		Source: "import Fixture\n\n" + body + "\n"}
}

func TestGateDecidesEachKindOfProposal(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	for _, c := range []struct {
		p        Proposal
		outcome  Outcome
		existing string
	}{
		{proposal("p1", "MySp", "def MySp (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"), Refused, "Fx.SpAligned"},
		{proposal("p2", "MyWrap", "def MyWrap (y : BitVec 64) : Prop := Fx.Frame y 16"), Refused, "Fx.Wrap"},
		{proposal("p3", "Fx.Frame", "def Other : Nat := 1"), Refused, "Fx.Frame"},
		{proposal("p4", "MyFrame", "def MyFrame (R : BitVec 5 → BitVec 64) (n : Nat) : Prop := n < 3 ∧ R 2#5 &&& 7#64 = 0#64"), Near, ""},
		{proposal("p5", "MyOuter", "def myInner (x : BitVec 64) : Prop := x &&& 7#64 = 0#64\n"+
			"def MyOuter (R : BitVec 5 → BitVec 64) : Prop := myInner (R 2#5) ∧ True"), Near, ""},
		{proposal("p6", "UsesSp", "def UsesSp (R : BitVec 5 → BitVec 64) : Prop := Fx.SpAligned R ∧ R 3#5 = 0"), Added, ""},
		{proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1"), Added, ""},
	} {
		v, err := g.Decide(ctx, c.p)
		if err != nil {
			t.Fatalf("%s: %v", c.p.ID, err)
		}
		if v.Outcome != c.outcome || v.Existing != c.existing {
			t.Errorf("%s: %s naming %q (%s, %+v), want %s naming %q", c.p.ID, v.Outcome, v.Existing, v.Reason, v.Matches, c.outcome, c.existing)
		}
	}
	if len(lib.got) != 2 || lib.got[0].Proposal != "p4" || lib.got[1].Proposal != "p5" {
		t.Fatalf("the librarian got %+v, want exactly the two near matches", lib.got)
	}
	r, err := Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range r.Concepts {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "Fx.SpAligned,Fx.Frame,UsesSp,Fresh" {
		t.Fatalf("registry after the gate: %v", names)
	}
}

func TestGateCannotDecideAProposalThatDoesNotCompile(t *testing.T) {
	g, lib := gateForTest(t)
	v, err := g.Decide(context.Background(), proposal("bad", "Bad", "def Bad : Nat := Fx.NoSuchThing"))
	if err == nil {
		t.Fatalf("decided %+v", v)
	}
	if len(lib.got) != 0 {
		t.Fatal("the librarian got a proposal the gate could not read")
	}
}
