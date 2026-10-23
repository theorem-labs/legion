package conceptgate

import (
	"context"
	"errors"
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
	return gateWith(t, gateRegistry, true)
}

// gateWith is gateForTest with another registry, and an index with or without its compiled part.
func gateWith(t *testing.T, registry string, compiled bool) (*Gate, *recordingLibrarian) {
	t.Helper()
	if _, err := exec.LookPath("lake"); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_LEAN") != "" {
			t.Fatal("DECLINDEX_REQUIRE_LEAN is set and no lake on PATH")
		}
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
	if err := os.WriteFile(reg, []byte(registry), 0o644); err != nil {
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
	opt := declindex.RefreshOptions{Commit: "c"}
	if compiled {
		opt.Compiled = co
	}
	if _, err := s.Refresh(ctx, spec, opt); err != nil {
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
		{proposal("p3", "Fx.Frame", "def Other : Nat := 1"), NameTaken, "Fx.Frame"},
		{proposal("p4", "MyFrame", "def MyFrame (R : BitVec 5 → BitVec 64) (n : Nat) : Prop := n < 3 ∧ R 2#5 &&& 7#64 = 0#64"), Near, ""},
		{proposal("p5", "MyOuter", "def myInner (x : BitVec 64) : Prop := x &&& 7#64 = 0#64\n"+
			"def MyOuter (R : BitVec 5 → BitVec 64) : Prop := myInner (R 2#5) ∧ True"), Near, ""},
		{proposal("p6", "UsesSp", "def UsesSp (R : BitVec 5 → BitVec 64) : Prop := Fx.SpAligned R ∧ R 3#5 = 0"), Added, ""},
		{proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1"), Added, ""},
		{proposal("p8", "OtherBox", "structure OtherBox (R : BitVec 5 → BitVec 64) : Prop where\n  other : R 2#5 &&& 7#64 = 0#64"), Near, ""},
		// Fresh was added by p7 and has not landed: the index has no compiled entry for it, the registry has.
		{proposal("p9", "FreshCopy", "def FreshCopy (y : Nat) : Prop := y < 3 ∧ y ≠ 1"), Refused, "Fresh"},
	} {
		v, err := g.Decide(ctx, c.p)
		if err != nil {
			t.Fatalf("%s: %v", c.p.ID, err)
		}
		if v.Outcome != c.outcome || v.Existing != c.existing {
			t.Errorf("%s: %s naming %q (%s, %+v), want %s naming %q", c.p.ID, v.Outcome, v.Existing, v.Reason, v.Matches, c.outcome, c.existing)
		}
	}
	if len(lib.got) != 3 || lib.got[0].Proposal != "p4" || lib.got[1].Proposal != "p5" || lib.got[2].Proposal != "p8" {
		t.Fatalf("the librarian got %+v, want exactly the three near matches", lib.got)
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

// A registered concept that has not landed, has no form and no compiled hash cannot be compared: a proposal goes
// to the librarian, never into the registry.
func TestGateRoutesWhatItCannotCompare(t *testing.T) {
	g, lib := gateWith(t, gateRegistry+"  - name: Fx.Ghost\n    home: Ghost.lean\n    meaning: A concept not declared yet.\n", true)
	v, err := g.Decide(context.Background(), proposal("q1", "Fresh2", "def Fresh2 (x : Nat) : Prop := x < 5"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != Near || !strings.Contains(v.Reason, "Fx.Ghost") || len(lib.got) != 1 {
		t.Fatalf("verdict %+v, librarian %d", v, len(lib.got))
	}
	r, _ := Load(g.Registry)
	if _, ok := r.Find("Fresh2"); ok {
		t.Fatal("added a proposal the gate could not compare")
	}
}

// An index read without its compiled part has no compiled statements to compare: the gate cannot decide.
func TestGateNeedsTheCompiledIndex(t *testing.T) {
	g, lib := gateWith(t, gateRegistry, false)
	v, err := g.Decide(context.Background(), proposal("q2", "MySp2", "def MySp2 (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"))
	if err == nil {
		t.Fatalf("decided %+v without the compiled index", v)
	}
	r, _ := Load(g.Registry)
	if _, ok := r.Find("MySp2"); ok || len(lib.got) != 0 {
		t.Fatal("the gate acted without the compiled index")
	}
}

// A proposal is declarations only: one that would run code while it is compiled is refused, and none of its code
// runs. The last two hide their code from the text check: a character literal `'"'` opens what the text check
// reads as a string, and `by_elab` is not on its list.
func TestGateRefusesAProposalThatRunsCode(t *testing.T) {
	g, lib := gateForTest(t)
	target := filepath.Join(t.TempDir(), "written")
	write := "IO.FS.writeFile \"" + target + "\" \"x\""
	for _, body := range []string{
		"#eval " + write,
		"run_cmd (" + write + " : IO Unit)",
		"initialize (" + write + " : IO Unit)",
		"def quote : Char := '\"'\n#eval " + write + "\n-- '\"'",
		"def run : Nat := by_elab do " + write + "; return Lean.mkNatLit 1",
	} {
		v, err := g.Decide(context.Background(), proposal("evil", "Evil", "def Evil : Nat := 1\n"+body))
		if err == nil {
			t.Fatalf("%q decided %+v", body, v)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("%q wrote a file", body)
		}
	}
	if len(lib.got) != 0 {
		t.Fatal("the librarian got a refused proposal")
	}
}

// The gate compiles a proposal only inside the sandbox. Where there is none, it cannot decide, and compiles
// nothing.
func TestGateWithoutASandboxCannotDecide(t *testing.T) {
	g, lib := gateForTest(t)
	g.CheckSandbox = func(context.Context, declindex.Lean) error { return errors.New("no sandbox here") }
	work := g.WorkDir
	v, err := g.Decide(context.Background(), proposal("q3", "Fresh3", "def Fresh3 (x : Nat) : Prop := x < 4"))
	if err == nil || !strings.Contains(err.Error(), "no sandbox here") {
		t.Fatalf("decided %+v (%v) without a sandbox", v, err)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("the gate wrote %d entries without a sandbox", len(entries))
	}
	if r, _ := Load(g.Registry); len(lib.got) != 0 || len(r.Concepts) != 2 {
		t.Fatal("the gate acted without a sandbox")
	}
}

// Forms are the librarian's: a proposal carries none, since the gate would add them to the registry, and the
// index compiles the registry's forms outside the sandbox.
func TestGateRefusesAProposalWithForms(t *testing.T) {
	g, _ := gateForTest(t)
	p := proposal("q4", "Fresh4", "def Fresh4 (x : Nat) : Prop := x < 4")
	p.Concept.Forms = []string{"fun (x : Nat) => x < 4"}
	if v, err := g.Decide(context.Background(), p); err == nil {
		t.Fatalf("decided %+v", v)
	}
}
