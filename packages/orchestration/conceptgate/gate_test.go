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

type recordingLibrarian struct {
	got []ReviewRequest
	err error
}

func (r *recordingLibrarian) Review(_ context.Context, request ReviewRequest) error {
	r.got = append(r.got, request)
	return r.err
}

func requireSandboxTest(t *testing.T) {
	t.Helper()
	if os.Getenv("DECLINDEX_SANDBOX_TEST") != "1" {
		t.Skip("sandboxed Lean tests run only with DECLINDEX_SANDBOX_TEST=1, in a disposable container or CI runner")
	}
}

// gateForTest builds the fixture tree, its index with the registry's facts, and a gate over them. The Lean
// compilation exercises confinement, so it runs only in the Docker sandbox test environment.
func gateForTest(t *testing.T) (*Gate, *recordingLibrarian) {
	t.Helper()
	return gateWith(t, true)
}

// gateWith is gateForTest with an index with or without its compiled part.
func gateWith(t *testing.T, compiled bool) (*Gate, *recordingLibrarian) {
	t.Helper()
	requireSandboxTest(t)
	if _, err := exec.LookPath("lake"); err != nil {
		t.Fatal("DECLINDEX_SANDBOX_TEST is set and no lake is on PATH")
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
	registry := filepath.Join(tree, "registry")
	if err := os.MkdirAll(filepath.Join(registry, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"registry.yaml", "concepts/Fx.SpAligned.yaml", "concepts/Fx.Frame.yaml"} {
		body, err := os.ReadFile(filepath.Join("testdata", "registry", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(registry, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, tree, "init", "-q")
	runGit(t, tree, "config", "user.email", "test@example.com")
	runGit(t, tree, "config", "user.name", "Test")
	runGit(t, tree, "add", ".")
	runGit(t, tree, "commit", "-qm", "fixture")
	s, err := declindex.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r, defects, err := Load(registry)
	if err != nil || len(defects) != 0 {
		t.Fatalf("registry: %v, defects: %v", err, defects)
	}
	co := &declindex.CompiledOptions{
		Exporter: &declindex.Exporter{Lean: l, Bin: exporterBin, SearchPath: []string{tree}, Jobs: 1},
		Forms:    r.FormsModule(),
		Dir:      t.TempDir(),
	}
	spec := declindex.Tree{Source: tree, Lib: tree, Roots: declindex.Roots{"Fixture": true}}
	opt := declindex.RefreshOptions{Commit: gitHead(t, tree)}
	if compiled {
		opt.Compiled = func(context.Context) (*declindex.CompiledOptions, error) { return co, nil }
	}
	if _, err := s.Refresh(ctx, spec, opt); err != nil {
		t.Fatal(err)
	}
	lib := &recordingLibrarian{}
	return &Gate{Index: s, Tree: spec, Registry: registry, Compiled: co, Librarian: lib, WorkDir: t.TempDir()}, lib
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func proposal(id, name, body string) Proposal {
	return Proposal{ID: id, Concept: Concept{Name: name, Home: "New.lean", Meaning: "A proposed concept."},
		Source: "import Fixture\n\n" + body + "\n"}
}

func TestGateDecidesEachKindOfProposal(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	options := DecideOptions{Proposer: "function:open_file", ProposalBytes: []byte(`{"id":"p"}`)}
	for _, c := range []struct {
		p        Proposal
		outcome  Outcome
		existing string
	}{
		{proposal("p1", "MySp", "def MySp (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"), Refused, "Fx.SpAligned"},
		{proposal("p2", "MyWrap", "def MyWrap (y : BitVec 64) : Prop := Fx.Frame y 16"), Refused, "Fx.Wrap"},
		{proposal("p3", "Fx.Frame", "def Other : Nat := 1"), NameTaken, "Fx.Frame"},
		{proposal("p4", "MyFrame", "def MyFrame (R : BitVec 5 → BitVec 64) (n : Nat) : Prop := n < 3 ∧ R 2#5 &&& 7#64 = 0#64"), Near, ""},
		{proposal("p5", "MyOuter", "def myInner (x : BitVec 64) : Prop := x &&& 7#64 = 0#64\ndef MyOuter (R : BitVec 5 → BitVec 64) : Prop := myInner (R 2#5) ∧ True"), Near, ""},
		{proposal("p6", "UsesSp", "def UsesSp (R : BitVec 5 → BitVec 64) : Prop := Fx.SpAligned R ∧ R 3#5 = 0"), Added, ""},
		{proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1"), Added, ""},
	} {
		v, err := g.Decide(ctx, c.p, options)
		if err != nil {
			t.Fatalf("%s: %v", c.p.ID, err)
		}
		if v.Outcome != c.outcome || v.Existing != c.existing {
			t.Errorf("%s: %s naming %q (%s, %+v), want %s naming %q", c.p.ID, v.Outcome, v.Existing, v.Reason, v.Matches, c.outcome, c.existing)
		}
		if c.p.ID == "p7" {
			if v.Addition == nil || v.Addition.Base != gitHead(t, g.Tree.Source) || v.Addition.Path != "registry/concepts/"+v.CompiledHash+".yaml" || !strings.Contains(v.Addition.Content, "source: |") {
				t.Fatalf("addition: %+v", v.Addition)
			}
			file := filepath.Join(g.Tree.Source, filepath.FromSlash(v.Addition.Path))
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("the gate wrote its addition: %v", err)
			}
			if status := exec.Command("git", "-C", g.Tree.Source, "status", "--porcelain").Run(); status != nil {
				t.Fatal(status)
			}
			if err := os.WriteFile(file, []byte(v.Addition.Content), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, g.Tree.Source, "add", "registry")
			runGit(t, g.Tree.Source, "commit", "-qm", "land Fresh")
		}
	}
	copyVerdict, err := g.Decide(ctx, proposal("p9", "FreshCopy", "def FreshCopy (y : Nat) : Prop := y < 3 ∧ y ≠ 1"), options)
	if err != nil || copyVerdict.Outcome != Refused || copyVerdict.Existing != "Fresh" {
		t.Fatalf("pending exact duplicate: %+v, %v", copyVerdict, err)
	}
	nearVerdict, err := g.Decide(ctx, proposal("p10", "FreshRestatement", "theorem FreshRestatement (y : Nat) (_h : y < 3 ∧ y ≠ 1) : True := trivial"), DecideOptions{Proposer: options.Proposer, AcceptedBy: "librarian", ProposalBytes: options.ProposalBytes})
	if err != nil || nearVerdict.Outcome != Added || nearVerdict.Addition == nil || !strings.Contains(nearVerdict.Addition.Content, "accepted_near:\n    - Fresh") {
		t.Fatalf("accepted pending near: %+v, %v", nearVerdict, err)
	}
	if len(lib.got) != 2 || lib.got[0].ID != lib.got[0].Verdict.VerdictID || lib.got[0].Proposer != options.Proposer || lib.got[0].Kind != "near" {
		t.Fatalf("librarian got %+v", lib.got)
	}
}

func TestGateFailsWhenLibrarianCannotDeliver(t *testing.T) {
	g, _ := gateForTest(t)
	script := filepath.Join(t.TempDir(), "fail-librarian.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho unreachable >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.Librarian = CommandLibrarian{Command: []string{script}, WorkDir: t.TempDir()}
	_, err := g.Decide(context.Background(), proposal("p", "Near", "def Near (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64 ∧ True"), DecideOptions{Proposer: "role"})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("librarian failure: %v", err)
	}
}

func TestGateNeedsTheCompiledIndex(t *testing.T) {
	g, lib := gateWith(t, false)
	v, err := g.Decide(context.Background(), proposal("q2", "MySp2", "def MySp2 (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"), DecideOptions{Proposer: "role"})
	if err == nil || len(lib.got) != 0 {
		t.Fatalf("decided %+v without the compiled index", v)
	}
}

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
		v, err := g.Decide(context.Background(), proposal("evil", "Evil", "def Evil : Nat := 1\n"+body), DecideOptions{Proposer: "role"})
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

func TestGateWithoutASandboxCannotDecide(t *testing.T) {
	g, lib := gateForTest(t)
	g.CheckSandbox = func(context.Context, declindex.Lean) error { return errors.New("no sandbox here") }
	work := g.WorkDir
	v, err := g.Decide(context.Background(), proposal("q3", "Fresh3", "def Fresh3 (x : Nat) : Prop := x < 4"), DecideOptions{Proposer: "role"})
	if err == nil || !strings.Contains(err.Error(), "no sandbox here") {
		t.Fatalf("decided %+v (%v) without a sandbox", v, err)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("the gate wrote %d entries without a sandbox", len(entries))
	}
	if _, defects, err := Load(g.Registry); err != nil || len(lib.got) != 0 || len(defects) != 0 {
		t.Fatal("the gate acted without a sandbox")
	}
}

// A pending concept whose stored source no longer compiles (here it names a constant the tree does not have) is a
// defect of that concept, left out of the forms: it does not stop the gate from deciding an unrelated proposal.
func TestAStalePendingConceptIsADefectNotAnInability(t *testing.T) {
	g, _ := gateForTest(t)
	ctx := context.Background()
	options := DecideOptions{Proposer: "function:open_file", ProposalBytes: []byte(`{"id":"p"}`)}
	fresh := proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1")
	if v, err := g.Decide(ctx, fresh, options); err != nil || v.Outcome != Added || len(v.Defects) != 0 {
		t.Fatalf("control: %+v, %v", v, err)
	}
	const key = "00112233445566778899aabbccddeeff"
	pending := "name: Old.Pending\nhome: Old.lean\nmeaning: A concept that an earlier landing added.\nproposal: old\n" +
		"proposer: function:old_owner\ncompiled_hash: " + key + "\nsource: |\n" +
		"  import Fixture\n  def Old.Pending (x : Nat) : Prop := Fx.RenamedSince x\n"
	if err := os.WriteFile(filepath.Join(g.Registry, "concepts", key+".yaml"), []byte(pending), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := g.Decide(ctx, fresh, options)
	if err != nil || v.Outcome != Added {
		t.Fatalf("an unrelated proposal after one stale pending concept: %+v, %v", v, err)
	}
	if len(v.Defects) != 1 || v.Defects[0].Path != "concepts/"+key+".yaml" || !strings.Contains(v.Defects[0].Message, "Old.Pending") {
		t.Fatalf("defects %+v, want the stale pending concept", v.Defects)
	}
}

// accepted_near records a librarian's decision; a proposal cannot supply it. Without --accepted-by the addition
// carries no accepted_near, whatever the proposal says, so it cannot suppress the post-landing recheck.
func TestGateDropsAProposalsOwnAcceptedNear(t *testing.T) {
	g, _ := gateForTest(t)
	p := proposal("p8", "Fresh8", "def Fresh8 (x : Nat) : Prop := x < 5 ∧ x ≠ 2")
	p.Concept.AcceptedNear = []string{"Fx.Frame"}
	v, err := g.Decide(context.Background(), p, DecideOptions{Proposer: "role"})
	if err != nil || v.Outcome != Added || v.Addition == nil {
		t.Fatalf("decide: %+v, %v", v, err)
	}
	if strings.Contains(v.Addition.Content, "accepted_near") {
		t.Fatalf("the addition keeps the proposal's accepted_near:\n%s", v.Addition.Content)
	}
}
