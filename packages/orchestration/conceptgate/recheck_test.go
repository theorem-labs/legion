package conceptgate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func exportedHash(t *testing.T, g *Gate, reg Registry, p Proposal) string {
	t.Helper()
	constants, err := g.export(context.Background(), reg, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range constants {
		if c.Name == p.Concept.Name {
			return c.Hash
		}
	}
	t.Fatalf("%s was not exported", p.Concept.Name)
	return ""
}
