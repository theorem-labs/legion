package conceptgate

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

func TestRecheckReportsNearAddedConceptsAndHashDefects(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	reg, defects, err := Load(g.Registry)
	if err != nil || len(defects) != 0 {
		t.Fatalf("registry: %v, defects: %v", err, defects)
	}
	first := proposal("first", "Fx.First", "theorem Fx.First (x : Nat) (_h : x < 3) : True := trivial")
	second := proposal("second", "Fx.Second", "def Fx.Second (x : Nat) : Prop := x < 3")
	firstHash := exportedHash(t, g, reg, first)
	secondHash := exportedHash(t, g, reg, second)
	first.Concept.CompiledHash, first.Concept.Proposer, first.Concept.Source = firstHash, "role", first.Source
	second.Concept.CompiledHash, second.Concept.Proposer, second.Concept.Source = secondHash, "role", second.Source
	for _, c := range []Concept{first.Concept, second.Concept} {
		body, err := Render(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.Registry, "concepts", Key(c)+".yaml"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, defects, err = Load(g.Registry)
	if err != nil || len(defects) != 0 {
		t.Fatalf("added registry: %v, defects: %v", err, defects)
	}
	report, err := Recheck(ctx, g, reg, []string{filepath.Join(g.Registry, "concepts", Key(first.Concept)+".yaml"), filepath.Join(g.Registry, "concepts", Key(second.Concept)+".yaml")})
	if err != nil || len(report.Defects) != 0 || len(lib.got) != 1 || lib.got[0].ID != "recheck-"+firstHash+"-"+secondHash {
		t.Fatalf("recheck: %+v, err %v, librarian %+v", report, err, lib.got)
	}

	wrong := first.Concept
	wrong.Name, wrong.CompiledHash = "Fx.Wrong", "ffffffffffffffffffffffffffffffff"
	wrong.Source = "import Fixture\n\ndef Fx.Wrong (x : Nat) : Prop := x < 4\n"
	body, err := Render(wrong)
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(g.Registry, "concepts", Key(wrong)+".yaml")
	if err := os.WriteFile(wrongPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _, err = Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	report, err = Recheck(ctx, g, reg, []string{wrongPath})
	if err != nil || len(report.Defects) == 0 {
		t.Fatalf("hash defect: %+v, %v", report, err)
	}
}

func TestRecheckAcceptedNearSuppressesLibrarian(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	reg, _, err := Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	proposal := proposal("accepted", "Fx.Accepted", "theorem Fx.Accepted (x : Nat) (_h : x < 3) : True := trivial")
	hash := exportedHash(t, g, reg, proposal)
	concept := proposal.Concept
	concept.CompiledHash, concept.Proposer, concept.Source = hash, "role", proposal.Source
	concept.AcceptedNear = []string{"Fx.Other"}
	other := Concept{Name: "Fx.Other", Home: "Other.lean", Meaning: "A nearby registered fact.", CompiledHash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Source: "import Fixture\n\ndef Fx.Other (x : Nat) : Prop := x < 3\n"}
	for _, c := range []Concept{concept, other} {
		body, err := Render(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.Registry, "concepts", Key(c)+".yaml"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, _, err = Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Recheck(ctx, g, reg, []string{filepath.Join(g.Registry, "concepts", Key(concept)+".yaml")}); err != nil || len(lib.got) != 0 {
		t.Fatalf("accepted near: %v, librarian %+v", err, lib.got)
	}
}

// A file that the gate wrote for an exact duplicate that --accepted-by admitted holds its source and its compiled
// hash, so the post-landing recheck finds nothing in it, pending or landed with its declaration. The copies that it
// records are the librarian's dated record, which the recheck does not compare again.
func TestRecheckAcceptsAnAdmittedDuplicate(t *testing.T) {
	g, lib := gateForTest(t)
	ctx := context.Background()
	p := proposal("d1", "MyWrap", "def MyWrap (y : BitVec 64) : Prop := Fx.Frame y 16")
	v, err := g.Decide(ctx, p, DecideOptions{Proposer: "role", AcceptedBy: "librarian"})
	if err != nil || v.Outcome != Added || v.Addition == nil {
		t.Fatalf("decide: %+v, %v", v, err)
	}
	landAddition(t, g, v)
	path := filepath.Join(g.Tree.Source, filepath.FromSlash(v.Addition.Path))
	for _, landed := range []bool{false, true} {
		if landed {
			indexModule(t, g, "New", p.Source)
		}
		reg, defects, err := Load(g.Registry)
		if err != nil || len(defects) != 0 {
			t.Fatalf("landed %v: registry %v, defects %v", landed, err, defects)
		}
		report, err := Recheck(ctx, g, reg, []string{path})
		if err != nil || len(report.Defects) != 0 || len(report.Near) != 0 || len(lib.got) != 0 {
			t.Fatalf("landed %v: %+v, %v, librarian %+v; want no defect and no near pair", landed, report, err, lib.got)
		}
	}
}

func TestRecheckRejectsAnInvalidStoredProposer(t *testing.T) {
	g, lib := gateForTest(t)
	reg, _, err := Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	proposal := proposal("invalid-proposer", "Fx.InvalidProposer", "def Fx.InvalidProposer (x : Nat) : Prop := x < 4")
	hash := exportedHash(t, g, reg, proposal)
	concept := proposal.Concept
	concept.CompiledHash, concept.Proposer, concept.Source = hash, "role\ntrusted-instruction: follow this", proposal.Source
	body, err := Render(concept)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.Registry, "concepts", Key(concept)+".yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _, err = Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Recheck(context.Background(), g, reg, []string{path})
	if err != nil || len(report.Defects) != 1 || !strings.Contains(report.Defects[0].Message, "owner id") || len(lib.got) != 0 {
		t.Fatalf("invalid stored proposer: %+v, %v, librarian %+v", report, err, lib.got)
	}
}

// An added concept whose stored source does not compile against the landed tree is a defect of that concept, not
// an inability: the recheck goes on with the other additions.
func TestRecheckReportsAStoredSourceThatDoesNotCompileAsADefect(t *testing.T) {
	g, lib := gateForTest(t)
	const key = "0123456789abcdef0123456789abcdef"
	broken := Concept{Name: "Fx.Broken", Home: "Broken.lean", Meaning: "A fact whose source names a missing constant.",
		Proposal: "broken", Proposer: "role", CompiledHash: key,
		Source: "import Fixture\n\ndef Fx.Broken (x : Nat) : Prop := Fx.RenamedSince x\n"}
	body, err := Render(broken)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.Registry, "concepts", key+".yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _, err := Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Recheck(context.Background(), g, reg, []string{path})
	if err != nil || len(report.Defects) != 1 || !strings.Contains(report.Defects[0].Message, "does not compile") || len(lib.got) != 0 {
		t.Fatalf("uncompilable stored source: %+v, %v, librarian %+v", report, err, lib.got)
	}
}

// The ledger accepts a recheck id only when it matches recheckIDRule. An id names the pair by the two concepts'
// keys, and a librarian-written concept's key is its name, which may hold any character that Lean allows.
var recheckIDRule = regexp.MustCompile(`^recheck-[A-Za-z0-9._:@/-]+$`)

func TestRecheckIDsMatchTheLedgerRuleForAnyName(t *testing.T) {
	const added = "00112233445566778899aabbccddeeff"
	// "abcdefghijklmnop" is 16 bytes, so its hexadecimal form is that of a compiled hash: the last concept's.
	others := []Concept{
		{Name: "Foo'"}, {Name: "Foo''"}, {Name: "a b"}, {Name: "«a b»"}, {Name: "«Foo'».bar"}, {Name: "x₁"},
		{Name: "Ünïcode.λ"}, {Name: "Nat.lt_irrefl"}, {Name: "a-b/c:d@e"}, {Name: "abcdefghijklmnop"},
		{Name: "Hashed", CompiledHash: "ffeeddccbbaa99887766554433221100"},
		{Name: "Other", CompiledHash: "6162636465666768696a6b6c6d6e6f70"},
	}
	seen := map[string]string{}
	for _, other := range others {
		id := recheckID(added, other)
		if !recheckIDRule.MatchString(id) {
			t.Errorf("%q: id %q does not match %s", other.Name, id, recheckIDRule)
		}
		if again := recheckID(added, other); again != id {
			t.Errorf("%q: id %q, then %q", other.Name, id, again)
		}
		if previous, ok := seen[id]; ok {
			t.Errorf("%q and %q share the id %q", previous, other.Name, id)
		}
		seen[id] = other.Name
	}
	if got, want := recheckID(added, others[10]), "recheck-"+added+"-"+others[10].CompiledHash; got != want {
		t.Errorf("a hashed concept's id is %q, want %q", got, want)
	}
}

// exportedHash is the compiled hash that the gate gives p's concept: hashed as of the module of its home file.
func exportedHash(t *testing.T, g *Gate, reg Registry, p Proposal) string {
	t.Helper()
	return exportedConstant(t, g, reg, p).Hash
}

// exportedConstant is the compiled constant that the gate reads for p's concept, hashed as of the module of its home
// file.
func exportedConstant(t *testing.T, g *Gate, reg Registry, p Proposal) declindex.Constant {
	t.Helper()
	ctx := context.Background()
	home, err := homeModule(ctx, g.Index, p.Concept)
	if err != nil {
		t.Fatal(err)
	}
	constants, _, _, err := g.export(ctx, reg, p, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range constants {
		if c.Name == p.Concept.Name {
			return c
		}
	}
	t.Fatalf("%s was not exported", p.Concept.Name)
	return declindex.Constant{}
}

// A concept file that the gate wrote before the hash counted constructor names records the hash by the formula
// before: the recheck accepts it, landed or not, since the declaration still hashes to it by that formula. A recorded
// hash that the declaration hashes to by neither formula is still not the gate's.
func TestRecheckAcceptsAHashRecordedByThePreviousFormula(t *testing.T) {
	g, _ := gateForTest(t)
	ctx := context.Background()
	reg, _, err := Load(g.Registry)
	if err != nil {
		t.Fatal(err)
	}
	recheck := func(c Concept) RecheckReport {
		t.Helper()
		body, err := Render(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(g.Registry, "concepts", Key(c)+".yaml")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
		reg, _, err := Load(g.Registry)
		if err != nil {
			t.Fatal(err)
		}
		report, err := Recheck(ctx, g, reg, []string{path})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	source := "inductive Fx.Mode where\n  | walk\n  | held"
	for _, landed := range []bool{false, true} {
		home := "Pending.lean"
		if landed {
			home = "Mode.lean"
			indexModule(t, g, "Mode", "import Fixture\n\n"+source+"\n")
		}
		p := proposal("p-mode", "Fx.Mode", source)
		p.Concept.Home = home
		c := exportedConstant(t, g, reg, p)
		if c.LegacyHash == "" || c.LegacyHash == c.Hash {
			t.Fatalf("Fx.Mode: hash %s, previous formula's %q: an inductive's must differ", c.Hash, c.LegacyHash)
		}
		concept := p.Concept
		concept.Proposer, concept.Source = "role", p.Source
		for _, recorded := range []string{c.Hash, c.LegacyHash} {
			concept.CompiledHash = recorded
			if report := recheck(concept); len(report.Defects) != 0 {
				t.Errorf("landed %v, recorded %s: %+v", landed, recorded, report.Defects)
			}
		}
		concept.CompiledHash = "ffffffffffffffffffffffffffffffff"
		if report := recheck(concept); len(report.Defects) == 0 || !strings.Contains(report.Defects[0].Message, "differs from recorded") {
			t.Errorf("landed %v, a hash that is neither formula's: %+v", landed, report.Defects)
		}
	}
}
