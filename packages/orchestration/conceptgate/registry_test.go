package conceptgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# The concept registry.
version: 1
imports: [Fixture]
opens: [Fx]
concepts:
  - name: Fx.SpAligned
    home: Fixture.lean
    meaning: Register x2 of a register file is 8-aligned.
    forms:
      - "fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64"
  - name: Fx.Frame
    home: Fixture.lean
    meaning: A window of n bytes below an aligned word.
    extends: Fx.SpAligned
`

func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRegistry(t *testing.T) {
	r, err := Load(writeRegistry(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Concepts) != 2 || r.Concepts[1].Extends != "Fx.SpAligned" || len(r.Concepts[0].Forms) != 1 {
		t.Fatalf("registry %+v", r)
	}
	fm := r.FormsModule()
	if fm.Imports[0] != "Fixture" || fm.Opens[0] != "Fx" || len(fm.Concepts) != 2 || fm.Concepts[1].Forms != nil {
		t.Fatalf("forms module %+v", fm)
	}
	if c, ok := r.Find("Fx.Frame"); !ok || c.Home != "Fixture.lean" {
		t.Fatalf("find: %+v %v", c, ok)
	}
}

func TestLoadRefusesAnInvalidRegistry(t *testing.T) {
	for name, body := range map[string]string{
		"version":       strings.Replace(sample, "version: 1", "version: 2", 1),
		"no home":       strings.Replace(sample, "    home: Fixture.lean\n    meaning: Register", "    meaning: Register", 1),
		"two sentences": strings.Replace(sample, "is 8-aligned.", "is 8-aligned. It holds at entry.", 1),
		"no meaning":    strings.Replace(sample, "    meaning: A window of n bytes below an aligned word.\n", "", 1),
		"extends":       strings.Replace(sample, "extends: Fx.SpAligned", "extends: Fx.Nothing", 1),
		"duplicate":     sample + "  - name: Fx.Frame\n    home: X.lean\n    meaning: Again.\n",
		"unknown key":   sample + "    colour: red\n",
		"bad form":      strings.Replace(sample, `- "fun (R`, `- "def x := (R`, 1),
	} {
		if _, err := Load(writeRegistry(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAddAppendsAndKeepsTheFileAsWritten(t *testing.T) {
	p := writeRegistry(t, sample)
	c := Concept{Name: "Fx.NewThing", Home: "New.lean", Meaning: "A thing that nothing states yet.", Extends: "Fx.Frame",
		Proposal: "p-7"}
	if err := Add(p, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(raw), sample) {
		t.Fatalf("the existing text changed:\n%s", raw)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Find("Fx.NewThing")
	if !ok || got.Meaning != c.Meaning || got.Extends != "Fx.Frame" || got.Proposal != "p-7" {
		t.Fatalf("added %+v %v", got, ok)
	}
	if err := Add(p, c); err == nil {
		t.Fatal("a second concept of the same name was added")
	}
}

func TestLoadRegistryDirectoryUsesItsRegistryFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Concepts) != 2 {
		t.Fatalf("directory registry has %d concepts", len(got.Concepts))
	}
	if err := Add(dir, Concept{Name: "Fx.NewThing", Home: "New.lean", Meaning: "A thing that nothing states yet."}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "registry.yaml")); err != nil {
		t.Fatal(err)
	}
}
