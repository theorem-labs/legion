package conceptgate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/internal/indextest"
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

// requireLake skips a test that needs a Lean toolchain where lake is not on PATH, unless DECLINDEX_REQUIRE_LEAN is
// set (CI's Lean job), where a missing toolchain fails it.
func requireLake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lake"); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_LEAN") != "" {
			t.Fatal("DECLINDEX_REQUIRE_LEAN is set and no lake is on PATH")
		}
		t.Skip("no lake on PATH: the gate's Lean tests need a Lean toolchain")
	}
}

// gateForTest builds the fixture tree, its index with the registry's facts, and a gate over them. It needs Lean.
func gateForTest(t *testing.T) (*Gate, *recordingLibrarian) {
	t.Helper()
	return gateWith(t, true)
}

// gateWith is gateForTest with an index with or without its compiled part.
func gateWith(t *testing.T, compiled bool) (*Gate, *recordingLibrarian) {
	t.Helper()
	requireLake(t)
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
	s, err := declindex.Open(ctx, declindex.Target{DSN: indextest.DSN(t), Index: "idx"})
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
		Forms:    registryForms(r),
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
	return &Gate{Index: s, Tree: spec, Registry: registry, AdditionRoot: "registry", Compiled: co, Librarian: lib, WorkDir: t.TempDir()}, lib
}

// indexModule adds the module name, with source, to the gate's tree as one more root, compiles and commits it, and
// refreshes the gate's index, compiled part included, at that commit.
func indexModule(t *testing.T, g *Gate, name, source string) {
	t.Helper()
	ctx := context.Background()
	tree := g.Tree.Source
	if err := os.WriteFile(filepath.Join(tree, name+".lean"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := declindex.CompileModule(ctx, g.Compiled.Exporter.Lean, nil, tree, name); err != nil {
		t.Fatal(err)
	}
	g.Tree.Roots[name] = true
	runGit(t, tree, "add", ".")
	runGit(t, tree, "commit", "-qm", "add "+name)
	opt := declindex.RefreshOptions{Commit: gitHead(t, tree), Compiled: func(context.Context) (*declindex.CompiledOptions, error) { return g.Compiled, nil }}
	if _, err := g.Index.Refresh(ctx, g.Tree, opt); err != nil {
		t.Fatal(err)
	}
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

// registryForms is the forms module of every concept of r, with no stored source in it: what a refresh's forms
// module holds for a registry whose concepts the index all declares.
func registryForms(r Registry) declindex.FormsModule {
	fm := declindex.FormsModule{Imports: r.Imports, Opens: r.Opens}
	for _, c := range r.Concepts {
		fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
	}
	return fm
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

// A numeral's value is its instance's: `@OfNat.ofNat Nat 7 ⟨10⟩` is 10. A proposal that uses it is therefore not an
// exact duplicate of an indexed declaration that uses `@OfNat.ofNat Nat 7 ⟨9⟩`, which Lean proves unequal to it,
// although both write 7. The same numeral by the same instance is an exact duplicate, and so is a numeral of core
// Lean's Nat and BitVec instances however it is written (`2`, `7#64` and a `7` of type `BitVec 64`). The numerals
// sit in statements with parameters: a definition whose value is a numeral alone matches only its own name
// (TestGateMatchesALiteralDefinitionOnlyByItsName).
func TestGateTellsNumeralsApartByTheirInstance(t *testing.T) {
	g, _ := gateForTest(t)
	indexModule(t, g, "Numerals", "def Original (n : Nat) : Nat := n + @OfNat.ofNat Nat 7 ⟨9⟩\ndef Two (n : Nat) : Nat := n + 2\n")
	same := decideWith(t, g, "Numerals", "Same", "def Same (m : Nat) : Nat := m + @OfNat.ofNat Nat 7 ⟨9⟩")
	if same.Outcome != Refused || same.Existing != "Original" {
		t.Fatalf("the same instance: %s naming %q (%s), want refused naming Original", same.Outcome, same.Existing, same.Reason)
	}
	different := decideWith(t, g, "Numerals", "Different", "def Different (m : Nat) : Nat := m + @OfNat.ofNat Nat 7 ⟨10⟩\n"+
		"theorem reallyDifferent : Different 0 ≠ Original 0 := by decide")
	if different.Outcome != Added || different.CompiledHash == same.CompiledHash {
		t.Errorf("another instance: %s naming %q (%s) with compiled hash %s, Original's %s; want added with another hash",
			different.Outcome, different.Existing, different.Reason, different.CompiledHash, same.CompiledHash)
	}
	for _, c := range []struct{ name, body, existing string }{
		{"MyTwo", "def MyTwo (m : Nat) : Nat := m + 2", "Two"},
		{"MyWord", "theorem MyWord (y : BitVec 64) (_h : y &&& 7 = 0) : True := trivial", "Fx.other_word"},
	} {
		if v := decideWith(t, g, "Numerals", c.name, c.body); v.Outcome != Refused || v.Existing != c.existing {
			t.Errorf("%s: %s naming %q (%s), want refused naming %s", c.name, v.Outcome, v.Existing, v.Reason, c.existing)
		}
	}
}

// decideWith is a decision on a proposal that imports Fixture and the given indexed module.
func decideWith(t *testing.T, g *Gate, module, name, body string) Verdict {
	t.Helper()
	p := Proposal{ID: name, Concept: Concept{Name: name, Home: "New.lean", Meaning: "A proposed concept."},
		Source: "import Fixture\nimport " + module + "\n\n" + body + "\n"}
	v, err := g.Decide(context.Background(), p, DecideOptions{Proposer: "role"})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// An enumeration is told apart from another by its constructors' names. Every constructor of an enumeration has no
// field and the same type, so two unrelated enumerations with the same number of constructors state the same
// constructor types: counted alone, those made each new enumeration a copy of any indexed one of its size. A copy
// under another type name with the same constructors is still an exact duplicate.
func TestGateTellsEnumerationsApartByTheirConstructors(t *testing.T) {
	g, _ := gateForTest(t)
	indexModule(t, g, "Enums", "namespace Hue\ninductive Kind where\n  | red | green | blue | cyan | teal\nend Hue\n\n"+
		"namespace Form\ninductive Shape where\n  | circle | square | line\nend Form\n")
	for _, c := range []struct{ name, body, sameSize string }{
		{"Greek.Letter", "namespace Greek\ninductive Letter where\n  | alpha | beta | gamma | delta | omega\nend Greek", "Hue.Kind"},
		{"Pitch.Tone", "namespace Pitch\ninductive Tone where\n  | low | mid | high\nend Pitch", "Form.Shape"},
	} {
		if v := decideWith(t, g, "Enums", c.name, c.body); v.Outcome != Added {
			t.Errorf("%s, of %s's size with other constructors: %s naming %q (%s), want added", c.name, c.sameSize, v.Outcome, v.Existing, v.Reason)
		}
	}
	v := decideWith(t, g, "Enums", "Hue.Color", "namespace Hue\ninductive Color where\n  | red | green | blue | cyan | teal\nend Hue")
	if v.Outcome != Refused || v.Existing != "Hue.Kind" {
		t.Errorf("a copy of Hue.Kind under another name: %s naming %q (%s), want refused naming Hue.Kind", v.Outcome, v.Existing, v.Reason)
	}
}

// A definition whose value is a numeral or a literal and nothing else matches only a declaration of its own name:
// `def size : Nat := 16` is not a copy of every other Nat constant of 16, whatever that one means. So a proposal of
// one is never an exact duplicate of a declaration of another name, and goes on to the name check and the
// registered facts; two such proposals of one value get two addition paths. A definition whose value is not a
// literal is still an exact duplicate of another that states it under another name.
func TestGateMatchesALiteralDefinitionOnlyByItsName(t *testing.T) {
	g, _ := gateForTest(t)
	indexModule(t, g, "Sizes", "namespace Sym\ndef tableSize : Nat := 16\ndef zeroOff : Nat := 0\ndef wordOff : Nat := 8\n"+
		"def zero64 : BitVec 64 := 0#64\ndef mask : UInt64 := 255\ndef tag : String := \"irq\"\ndef neg : Int := -3\n"+
		"def ch : Char := 'a'\ndef half : Float := 0.5\nend Sym\n\n"+
		"namespace Slot\ndef nullOr (P : BitVec 64 → Prop) (s : BitVec 64) : Prop := s = 0#64 ∨ P s\nend Slot\n")
	hashes := map[string]string{}
	for _, c := range []struct{ name, body string }{
		{"Frame.size", "def Frame.size : Nat := 16"},
		{"Frame.lo", "def Frame.lo : Nat := 0"},
		{"Frame.hi", "def Frame.hi : Nat := 8"},
		{"Frame.width", "def Frame.width : Nat := 16"},
		{"Frame.zero", "def Frame.zero : BitVec 64 := 0#64"},
		{"Frame.mask", "def Frame.mask : UInt64 := 255"},
		{"Frame.tag", "def Frame.tag : String := \"irq\""},
		{"Frame.neg", "def Frame.neg : Int := -3"},
		{"Frame.ch", "def Frame.ch : Char := 'a'"},
		{"Frame.half", "def Frame.half : Float := 0.5"},
	} {
		v := decideWith(t, g, "Sizes", c.name, c.body)
		if v.Outcome != Added {
			t.Errorf("%s: %s naming %q (%s), want added", c.name, v.Outcome, v.Existing, v.Reason)
		}
		hashes[c.name] = v.CompiledHash
	}
	if hashes["Frame.size"] == hashes["Frame.width"] {
		t.Errorf("Frame.size and Frame.width, both 16, hash alike (%s): their additions would share one path", hashes["Frame.size"])
	}
	if v := decideWith(t, g, "Sizes", "Sym.tableSize", "def Sym.tableSize : Nat := 16"); v.Outcome != NameTaken || v.Existing != "Sym.tableSize" {
		t.Errorf("the indexed constant again: %s naming %q (%s), want name-taken naming Sym.tableSize", v.Outcome, v.Existing, v.Reason)
	}
	v := decideWith(t, g, "Sizes", "Link.zeroOr", "def Link.zeroOr (P : BitVec 64 → Prop) (x : BitVec 64) : Prop := x = 0#64 ∨ P x")
	if v.Outcome != Refused || v.Existing != "Slot.nullOr" {
		t.Errorf("Slot.nullOr under another name: %s naming %q (%s), want refused naming Slot.nullOr", v.Outcome, v.Existing, v.Reason)
	}
}

// A pending concept (added, not yet landed) is compared by the hash its stored source compiles to now, as well as by
// the one its file recorded. A file that the gate wrote before a change of the hash records a hash that no exporter
// computes any more, here one that is simply stale; a copy of its concept must still be refused.
func TestGateComparesAPendingConceptByItsCompiledSource(t *testing.T) {
	g, _ := gateForTest(t)
	pending := Concept{Name: "Fx.Mode", Home: "Mode.lean", Meaning: "A pending enumeration.", Proposal: "p-mode",
		Proposer: "role", CompiledHash: "0123456789abcdef0123456789abcdef",
		Source: "import Fixture\n\ninductive Fx.Mode where\n  | walk\n  | held\n"}
	body, err := Render(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Registry, "concepts", Key(pending)+".yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, g.Tree.Source, "add", "registry")
	runGit(t, g.Tree.Source, "commit", "-qm", "add pending Fx.Mode")
	v, err := g.Decide(context.Background(), proposal("p-gait", "Fx.Gait", "inductive Fx.Gait where\n  | walk\n  | held"), DecideOptions{Proposer: "role"})
	if err != nil || v.Outcome != Refused || v.Existing != "Fx.Mode" {
		t.Fatalf("a copy of the pending Fx.Mode: %s naming %q (%s), %v; want refused naming Fx.Mode", v.Outcome, v.Existing, v.Reason, err)
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

// A `#` command is a command that Lean's parser allow-list refuses (declindex.CheckSource, lean/DeclExport/Check.lean)
// before anything compiles, wherever it stands: first on its line, indented, after a bracket or a proof, or after
// a term on the same line. The text filter leaves `#` to Lean's parser, since no text pass tells a command from an
// Iris proof-mode pattern (`#Hinv`) that goes on to a line of its own. The refusal is the parser's, the proposal
// runs nothing, and the librarian gets nothing.
func TestGateRefusesAHashCommandWithLeansParser(t *testing.T) {
	g, lib := gateForTest(t)
	target := filepath.Join(t.TempDir(), "written")
	write := "IO.FS.writeFile \"" + target + "\" \"x\""
	for _, body := range []string{
		"#eval " + write,
		"  #eval " + write,
		"theorem t : True := by\n  trivial\n#eval " + write,
		"def p : Nat × Nat := (1,\n  2)\n  #eval " + write,
		"def two : Nat := 2 #eval " + write,
		"#eval[" + write + "]",
		"#print Nat",
	} {
		v, err := g.Decide(context.Background(), proposal("hash", "Hash", "def Hash : Nat := 1\n"+body), DecideOptions{Proposer: "role"})
		if !errors.Is(err, declindex.ErrRefusedSource) || v.Outcome != "" {
			t.Errorf("%q: %+v, %v; want Lean's parser allow-list to refuse it", body, v, err)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("%q wrote a file", body)
		}
	}
	if len(lib.got) != 0 {
		t.Fatal("the librarian got a refused proposal")
	}
}

// A proposal's home is the path of a .lean file relative to the tree, where its declaration lands: the gate refuses
// any other home before it compiles anything, and the error names it. The gate has no index, so a decision that went
// past the home would fail on it.
func TestGateRefusesAProposalWhoseHomeIsNotALeanFileInTheTree(t *testing.T) {
	g := &Gate{Compiled: &declindex.CompiledOptions{Exporter: &declindex.Exporter{}}}
	for _, home := range []string{"Fresh.md", "../Fresh.lean", "/Fresh.lean", "./Fresh.lean", "Sub//Fresh.lean", "Sub/.lean", `Sub\Fresh.lean`} {
		p := proposal("p", "Fresh", "def Fresh : Prop := True")
		p.Concept.Home = home
		v, err := g.Decide(context.Background(), p, DecideOptions{Proposer: "role"})
		if err == nil || !strings.Contains(err.Error(), "home") || !strings.Contains(err.Error(), home) || v.Outcome != "" {
			t.Errorf("home %q: %+v, %v; want an error that names the home", home, v, err)
		}
	}
	p := proposal("p", "Fresh", "def Fresh : Prop := True")
	p.Concept.Home = "Sub/Fresh.lean"
	if err := p.Concept.check(); err != nil {
		t.Errorf("a home in a directory of the tree: %v", err)
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

// decideAccepted is a decision on p with the librarian's acceptance given as acceptedBy ("" for none).
func decideAccepted(t *testing.T, g *Gate, p Proposal, acceptedBy string) (Verdict, error) {
	t.Helper()
	return g.Decide(context.Background(), p, DecideOptions{Proposer: "role", AcceptedBy: acceptedBy})
}

// landAddition writes v's addition file into the gate's registry and commits it, as an owner lands it.
func landAddition(t *testing.T, g *Gate, v Verdict) {
	t.Helper()
	file := filepath.Join(g.Tree.Source, filepath.FromSlash(v.Addition.Path))
	if err := os.WriteFile(file, []byte(v.Addition.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, g.Tree.Source, "add", "registry")
	runGit(t, g.Tree.Source, "commit", "-qm", "land "+v.Name)
}

// An exact duplicate of a declaration that no concept registers is refused, and the verdict lists every copy. When
// the librarian ruled the new name its own concept or the canonical one, --accepted-by admits it: the addition
// records the ruling and the copies, and is keyed and stored like any other. Once it lands, a further copy is a copy
// of the new concept, which --accepted-by does not admit.
func TestGateAdmitsADuplicateOfUnregisteredDeclarationsOnlyWithAcceptedBy(t *testing.T) {
	g, _ := gateForTest(t)
	wrap := proposal("d1", "MyWrap", "def MyWrap (y : BitVec 64) : Prop := Fx.Frame y 16")
	refused, err := decideAccepted(t, g, wrap, "")
	if err != nil || refused.Outcome != Refused || refused.Existing != "Fx.Wrap" || !slices.Equal(refused.Duplicates, []string{"Fx.Wrap"}) || refused.Addition != nil {
		t.Fatalf("no --accepted-by: %+v, %v; want refused naming Fx.Wrap with duplicates [Fx.Wrap]", refused, err)
	}
	added, err := decideAccepted(t, g, wrap, "librarian")
	if err != nil || added.Outcome != Added || added.Addition == nil || !slices.Equal(added.Duplicates, []string{"Fx.Wrap"}) {
		t.Fatalf("--accepted-by librarian: %+v, %v; want added with duplicates [Fx.Wrap]", added, err)
	}
	if !strings.Contains(added.Reason, "Fx.Wrap") || !strings.Contains(added.Reason, "librarian") {
		t.Errorf("reason %q names neither the copy nor the acceptance", added.Reason)
	}
	if want := "registry/concepts/" + added.CompiledHash + ".yaml"; added.CompiledHash == "" || added.Addition.Path != want {
		t.Errorf("addition path %q, want %q", added.Addition.Path, want)
	}
	c, err := decodeConcept([]byte(added.Addition.Content))
	if err != nil {
		t.Fatalf("the addition does not load: %v\n%s", err, added.Addition.Content)
	}
	if c.AcceptedBy != "librarian" || !slices.Equal(c.AcceptedDuplicates, []string{"Fx.Wrap"}) || c.CompiledHash != added.CompiledHash || c.Source != wrap.Source {
		t.Errorf("addition %+v: want accepted_by librarian, accepted_duplicates [Fx.Wrap], the compiled hash and the source", c)
	}
	if !strings.Contains(added.Addition.Content, "accepted_duplicates:\n    - Fx.Wrap\n") {
		t.Errorf("addition content:\n%s", added.Addition.Content)
	}
	landAddition(t, g, added)
	if _, defects, err := Load(g.Registry); err != nil || len(defects) != 0 {
		t.Fatalf("the landed addition: %v, defects %v", err, defects)
	}
	third, err := decideAccepted(t, g, proposal("d3", "ThirdWrap", "def ThirdWrap (z : BitVec 64) : Prop := Fx.Frame z 16"), "librarian")
	if err != nil || third.Outcome != Refused || third.Existing != "MyWrap" || third.Addition != nil {
		t.Fatalf("a copy after the landing: %+v, %v; want refused naming MyWrap", third, err)
	}
}

// --accepted-by admits no copy of a registered concept: one declared in its home file (refused by its compiled
// statement), a pending addition (by its stored source), or one whose declaration the index holds outside its home
// file, which only the exact-duplicate check finds.
func TestGateNeverAdmitsADuplicateOfARegisteredConcept(t *testing.T) {
	g, _ := gateForTest(t)
	if v, err := decideAccepted(t, g, proposal("r1", "MySp", "def MySp (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64"), "librarian"); err != nil || v.Outcome != Refused || v.Existing != "Fx.SpAligned" || v.Addition != nil {
		t.Errorf("a copy of the registered Fx.SpAligned: %+v, %v; want refused naming Fx.SpAligned", v, err)
	}
	fresh, err := decideAccepted(t, g, proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1"), "")
	if err != nil || fresh.Outcome != Added {
		t.Fatalf("control: %+v, %v", fresh, err)
	}
	landAddition(t, g, fresh)
	if v, err := decideAccepted(t, g, proposal("p9", "FreshCopy", "def FreshCopy (y : Nat) : Prop := y < 3 ∧ y ≠ 1"), "librarian"); err != nil || v.Outcome != Refused || v.Existing != "Fresh" || v.Addition != nil {
		t.Errorf("a copy of the pending Fresh: %+v, %v; want refused naming Fresh", v, err)
	}
	indexModule(t, g, "Held", "def Held.Word (x : Nat) : Prop := x < 7 ∧ x ≠ 4\n")
	if err := os.WriteFile(filepath.Join(g.Registry, "concepts", "Held.Word.yaml"), []byte("name: Held.Word\nhome: Elsewhere.lean\nmeaning: A concept declared outside its home file.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := decideAccepted(t, g, proposal("r6", "MyHeldWord", "def MyHeldWord (y : Nat) : Prop := y < 7 ∧ y ≠ 4"), "librarian"); err != nil || v.Outcome != Refused || v.Existing != "Held.Word" || v.Addition != nil || !strings.Contains(v.Reason, "registered") {
		t.Errorf("a copy of the registered Held.Word, declared outside its home: %+v, %v; want refused naming Held.Word", v, err)
	}
}

// A concept file that the loader refused may be the concept of a copy: the gate cannot tell whether --accepted-by
// may admit it until the file is repaired, so with the flag it is unable to decide and names the file. Without the
// flag the copy is refused, whatever the file says.
func TestGateIsUnableWhenARefusedConceptFileHoldsADuplicatesName(t *testing.T) {
	g, lib := gateForTest(t)
	indexModule(t, g, "Kept", "def Kept.Word (x : Nat) : Prop := x < 9 ∧ x ≠ 5\n")
	copied := proposal("r7", "MyKeptWord", "def MyKeptWord (y : Nat) : Prop := y < 9 ∧ y ≠ 5")
	for _, c := range []struct{ file, body string }{
		{"Kept.Word.yaml", "name: [broken\n"},
		{"Elsewhere.yaml", "name: Kept.Word\nhome: Kept.lean\nmeaning: A concept. It has two sentences.\n"},
	} {
		path := filepath.Join(g.Registry, "concepts", c.file)
		if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		at := "registry/concepts/" + c.file
		if v, err := decideAccepted(t, g, copied, "librarian"); err == nil || !strings.Contains(err.Error(), at) || v.Addition != nil {
			t.Errorf("%s with --accepted-by: %+v, %v; want unable, naming %s", c.file, v, err, at)
		}
		if v, err := decideAccepted(t, g, copied, ""); err != nil || v.Outcome != Refused || v.Existing != "Kept.Word" {
			t.Errorf("%s without --accepted-by: %+v, %v; want refused naming Kept.Word", c.file, v, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if len(lib.got) != 0 {
		t.Fatalf("the librarian got %+v", lib.got)
	}
}

// One --accepted-by admits every copy, sorted, and the near matches of the same proposal together: the ruling
// covers both. Without it the copy is refused before any near match goes to the librarian.
func TestGateAcceptsCopiesAndNearMatchesWithOneAcceptedBy(t *testing.T) {
	g, lib := gateForTest(t)
	indexModule(t, g, "Copies", "namespace Dup\ndef b (x : Nat) : Prop := x < 11 ∧ x ≠ 6\ndef a (y : Nat) : Prop := y < 11 ∧ y ≠ 6\nend Dup\n")
	both, err := decideAccepted(t, g, proposal("r5", "MyCopy", "def MyCopy (z : Nat) : Prop := z < 11 ∧ z ≠ 6"), "librarian")
	if err != nil || both.Outcome != Added || both.Addition == nil || !slices.Equal(both.Duplicates, []string{"Dup.a", "Dup.b"}) ||
		!strings.Contains(both.Addition.Content, "accepted_duplicates:\n    - Dup.a\n    - Dup.b\n") {
		t.Fatalf("two indexed copies: %+v, %v; want added with duplicates [Dup.a Dup.b]", both, err)
	}
	nearCopy := proposal("r8", "MyNearSp", "def MyNearSp (Q : BitVec 5 → BitVec 64) (m : Nat) : Prop := m > 0 ∧ Q 2#5 &&& 7#64 = 0#64")
	if v, err := decideAccepted(t, g, nearCopy, ""); err != nil || v.Outcome != Refused || v.Existing != "Fx.NearSp" || len(lib.got) != 0 {
		t.Fatalf("a copy of Fx.NearSp that restates Fx.SpAligned, no --accepted-by: %+v, %v, librarian %+v; want refused naming Fx.NearSp", v, err, lib.got)
	}
	v, err := decideAccepted(t, g, nearCopy, "librarian")
	if err != nil || v.Outcome != Added || v.Addition == nil || len(v.Matches) == 0 {
		t.Fatalf("with --accepted-by: %+v, %v; want added, with its near matches", v, err)
	}
	c, err := decodeConcept([]byte(v.Addition.Content))
	if err != nil || !slices.Equal(c.AcceptedNear, []string{"Fx.SpAligned"}) || !slices.Equal(c.AcceptedDuplicates, []string{"Fx.NearSp"}) {
		t.Fatalf("addition %+v, %v; want accepted_near [Fx.SpAligned] and accepted_duplicates [Fx.NearSp]", c, err)
	}
	if len(lib.got) != 0 {
		t.Fatalf("the librarian got %+v", lib.got)
	}
}

// accepted_duplicates records a librarian's decision, as accepted_near does: the addition never keeps the
// proposal's own, with or without --accepted-by.
func TestGateDropsAProposalsOwnAcceptedDuplicates(t *testing.T) {
	g, _ := gateForTest(t)
	p := proposal("p9d", "Fresh9", "def Fresh9 (x : Nat) : Prop := x < 13 ∧ x ≠ 2")
	p.Concept.AcceptedDuplicates = []string{"Fx.Wrap"}
	for _, acceptedBy := range []string{"", "librarian"} {
		v, err := decideAccepted(t, g, p, acceptedBy)
		if err != nil || v.Outcome != Added || v.Addition == nil {
			t.Fatalf("accepted by %q: %+v, %v", acceptedBy, v, err)
		}
		if strings.Contains(v.Addition.Content, "accepted_duplicates") || len(v.Duplicates) != 0 {
			t.Fatalf("accepted by %q: the addition keeps the proposal's accepted_duplicates:\n%s", acceptedBy, v.Addition.Content)
		}
	}
}

// OpenGate always sets AdditionRoot, the registry's path in the repository. A gate without one gives no addition
// path: it does not derive one from Registry, which OpenGate reads from a snapshot outside the repository.
func TestAnAdditionPathNeedsTheGatesAdditionRoot(t *testing.T) {
	tree := t.TempDir()
	g := &Gate{Tree: declindex.Tree{Source: tree}, Registry: filepath.Join(tree, "registry")}
	if path, err := g.additionPath("k"); err == nil {
		t.Errorf("a gate without AdditionRoot gave the addition path %q", path)
	}
	g.AdditionRoot = "proj/registry"
	if path, err := g.additionPath("k"); err != nil || path != "proj/registry/concepts/k.yaml" {
		t.Errorf("addition path %q, %v; want proj/registry/concepts/k.yaml", path, err)
	}
	g.AdditionRoot = "../registry"
	if path, err := g.additionPath("k"); err == nil {
		t.Errorf("an AdditionRoot outside the repository gave the addition path %q", path)
	}
}

// A concept file that the loader refused still holds its name and its path in the registry. A proposal with its
// name, by the file's key or by the name its YAML gives, is name-taken and names the file. A proposal whose compiled
// statement is the file's key (its addition would replace the file) or the compiled hash that the file records is
// unable to be decided until the file is repaired. In each case the refused file is a defect of the verdict, and
// the librarian gets nothing.
func TestGateKeepsARefusedConceptFilesNameAndPath(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	options := DecideOptions{Proposer: "role"}
	fresh := proposal("p7", "Fresh", "def Fresh (x : Nat) : Prop := x < 3 ∧ x ≠ 1")
	control, err := g.Decide(ctx, fresh, options)
	if err != nil || control.Outcome != Added || control.Addition == nil || len(control.Defects) != 0 {
		t.Fatalf("control: %+v, %v", control, err)
	}
	hash := control.CompiledHash
	hasDefect := func(v Verdict, path string) bool {
		for _, d := range v.Defects {
			if d.Path == path {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		file, body string
		unable     bool
	}{
		{"Fresh.yaml", "name: [Fresh\n", false},
		{"Elsewhere.yaml", "name: Fresh\nhome: Fresh.lean\nmeaning: A concept. It has two sentences.\n", false},
		{hash + ".yaml", "name: [broken\n", true},
		{"Misnamed.yaml", "name: Old.Fresh\nhome: Old.lean\nmeaning: An older copy.\ncompiled_hash: " + hash + "\ncolour: red\n", true},
	} {
		path := filepath.Join(g.Registry, "concepts", c.file)
		if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		at := "registry/concepts/" + c.file
		v, err := g.Decide(ctx, fresh, options)
		switch {
		case c.unable && (err == nil || !strings.Contains(err.Error(), at)):
			t.Errorf("%s: %+v, %v; want unable, naming %s", c.file, v, err, at)
		case !c.unable && (err != nil || v.Outcome != NameTaken || v.Existing != "Fresh" || v.ExistingAt != at || !strings.Contains(v.Reason, "refused")):
			t.Errorf("%s: %+v, %v; want name-taken at %s", c.file, v, err, at)
		case v.Addition != nil:
			t.Errorf("%s: an addition %+v", c.file, v.Addition)
		case !hasDefect(v, "concepts/"+c.file):
			t.Errorf("%s: defects %+v, want the refused file", c.file, v.Defects)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if len(lib.got) != 0 {
		t.Fatalf("the librarian got %+v", lib.got)
	}
}
