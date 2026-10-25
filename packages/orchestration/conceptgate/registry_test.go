package conceptgate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeRegistry(t *testing.T, root string, concepts map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "registry.yaml"), []byte("version: 2\nimports: [Fixture]\nopens: [Fx]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range concepts {
		if err := os.WriteFile(filepath.Join(root, "concepts", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadFormat2Directory(t *testing.T) {
	dir := writeRegistry(t, t.TempDir(), map[string]string{
		"Fx.SpAligned.yaml": "name: Fx.SpAligned\nhome: Fixture.lean\nmeaning: Register x2 of a register file is 8-aligned.\nforms:\n  - \"fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64\"\n",
		"Fx.Frame.yaml":     "name: Fx.Frame\nhome: Fixture.lean\nmeaning: A window of n bytes below an aligned word.\nextends: Fx.SpAligned\n",
	})
	r, defects, err := Load(dir)
	if err != nil || len(defects) != 0 {
		t.Fatalf("load: %v, defects: %v", err, defects)
	}
	if len(r.Concepts) != 2 {
		t.Fatalf("registry %+v", r)
	}
	if c, ok := r.Find("Fx.SpAligned"); !ok || len(c.Forms) != 1 {
		t.Fatalf("aligned concept: %+v %v", c, ok)
	}
	if c, ok := r.Find("Fx.Frame"); !ok || c.Home != "Fixture.lean" {
		t.Fatalf("find: %+v %v", c, ok)
	}
}

func TestLoadReportsFormat2Defects(t *testing.T) {
	good := "name: Fx.Frame\nhome: Fixture.lean\nmeaning: A window of n bytes below an aligned word.\n"
	for name, tc := range map[string]struct {
		concepts map[string]string
		want     string
	}{
		"wrong key": {map[string]string{"wrong.yaml": good}, "want concepts/Fx.Frame.yaml"},
		"duplicate name": {map[string]string{
			"Fx.Frame.yaml": good,
			"Fx.Other.yaml": "name: Fx.Frame\nhome: Other.lean\nmeaning: The same name in another file.\n",
		}, "also in"},
		"duplicate compiled hash": {map[string]string{
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.yaml": "name: Fx.A\nhome: A.lean\nmeaning: A first fact.\ncompiled_hash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.yaml": "name: Fx.B\nhome: B.lean\nmeaning: A second fact.\ncompiled_hash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		}, "compiled hash"},
		"missing extension": {map[string]string{"Fx.Frame.yaml": good + "extends: Fx.Missing\n"}, "which is not registered"},
	} {
		t.Run(name, func(t *testing.T) {
			_, defects, err := Load(writeRegistry(t, t.TempDir(), tc.concepts))
			if err != nil {
				t.Fatal(err)
			}
			joined := make([]string, len(defects))
			for i, defect := range defects {
				joined[i] = defect.Error()
			}
			if !strings.Contains(strings.Join(joined, "\n"), tc.want) {
				t.Fatalf("defects %v do not contain %q", defects, tc.want)
			}
		})
	}
}

func TestRenderRoundTripsGateAddition(t *testing.T) {
	concept := Concept{
		Name: "Fx.New", Home: "New.lean", Meaning: "A newly proposed fact.", Proposal: "p1", Proposer: "function:open_file",
		CompiledHash: "0123456789abcdef0123456789abcdef", AcceptedBy: "librarian", AcceptedNear: []string{"Fx.Frame"},
		Source: "import Fixture\n\ndef Fx.New : Prop := True\n",
	}
	body, err := Render(concept)
	if err != nil {
		t.Fatal(err)
	}
	dir := writeRegistry(t, t.TempDir(), map[string]string{Key(concept) + ".yaml": string(body)})
	got, defects, err := Load(dir)
	if err != nil || len(defects) != 0 || len(got.Concepts) != 1 || !reflect.DeepEqual(got.Concepts[0], concept) {
		t.Fatalf("round trip: %+v, defects %v, err %v", got, defects, err)
	}
}

func TestLoadRefusesFormatOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte("version: 1\nconcepts: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "format 2") {
		t.Fatalf("format one error: %v", err)
	}
}

// A concept file with a malformed compiled hash is one concept's defect, as every refused concept file is.
func TestLoadReportsAnInvalidCompiledHashAsADefect(t *testing.T) {
	dir := writeRegistry(t, t.TempDir(), map[string]string{
		"not-a-hash.yaml": "name: Fx.Bad\nhome: Bad.lean\nmeaning: A badly keyed fact.\ncompiled_hash: not-a-hash\n",
	})
	r, defects, err := Load(dir)
	if err != nil || len(defects) != 1 || !strings.Contains(defects[0].Message, "compiled hash") || len(r.Concepts) != 0 {
		t.Fatalf("invalid compiled hash: concepts %+v, defects %v, err %v", r.Concepts, defects, err)
	}
}

// A concept file the loader refuses is a defect of that file, left out of the registry: the other concepts still
// load, so one bad file cannot stop the gate, the ledger or a refresh for every owner.
func TestLoadReportsARefusedConceptFileAsADefect(t *testing.T) {
	good := "name: Fx.Frame\nhome: Fixture.lean\nmeaning: A window of n bytes below an aligned word.\n"
	for name, body := range map[string]string{
		"two sentences":  "name: Fx.Bad\nhome: Bad.lean\nmeaning: Register x2 is aligned. It stays so.\n",
		"unknown key":    "name: Fx.Bad\nhome: Bad.lean\nmeaning: A fact with an unknown key.\ncolour: red\n",
		"not YAML":       "name: [Fx.Bad\n",
		"no home":        "name: Fx.Bad\nmeaning: A fact with no home.\n",
		"malformed form": "name: Fx.Bad\nhome: Bad.lean\nmeaning: A fact with an empty form.\nforms: [\"\"]\n",
		// A home is the path of a .lean file relative to the tree: slash-separated, inside it, and clean.
		"home not a Lean file":       "name: Fx.Bad\nhome: Bad.md\nmeaning: A fact homed in a text file.\n",
		"home outside the tree":      "name: Fx.Bad\nhome: ../Bad.lean\nmeaning: A fact homed outside the tree.\n",
		"absolute home":              "name: Fx.Bad\nhome: /Bad.lean\nmeaning: A fact homed at an absolute path.\n",
		"home with a dot step":       "name: Fx.Bad\nhome: Sub/../Bad.lean\nmeaning: A fact homed through a dot step.\n",
		"home with no module name":   "name: Fx.Bad\nhome: Sub/.lean\nmeaning: A fact homed in a file with no name.\n",
		"home with a back separator": "name: Fx.Bad\nhome: Sub\\Bad.lean\nmeaning: A fact homed with a backslash.\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, defects, err := Load(writeRegistry(t, t.TempDir(), map[string]string{"Fx.Frame.yaml": good, "Fx.Bad.yaml": body}))
			if err != nil {
				t.Fatalf("one refused file stopped the loader: %v", err)
			}
			if len(defects) != 1 || defects[0].Path != "concepts/Fx.Bad.yaml" {
				t.Fatalf("defects %v, want one for concepts/Fx.Bad.yaml", defects)
			}
			if _, ok := r.Find("Fx.Frame"); !ok || len(r.Concepts) != 1 {
				t.Fatalf("concepts %+v, want only Fx.Frame", r.Concepts)
			}
		})
	}
}

// A refused file still holds its path, and whatever name and compiled hash its YAML gives, so Load keeps them for
// the gate.
func TestLoadKeepsWhatARefusedConceptFileHolds(t *testing.T) {
	const hash = "00112233445566778899aabbccddeeff"
	r, defects, err := Load(writeRegistry(t, t.TempDir(), map[string]string{
		"Fx.Two.yaml":   "name: Fx.Two\nhome: Two.lean\nmeaning: Register x2 is aligned. It stays so.\n",
		hash + ".yaml":  "name: Fx.Keyed\nhome: Keyed.lean\nmeaning: A fact with an unknown key.\ncompiled_hash: " + hash + "\ncolour: red\n",
		"Fx.Mess.yaml":  "name: [Fx.Mess\n",
		"Fx.Frame.yaml": "name: Fx.Frame\nhome: Fixture.lean\nmeaning: A window of n bytes below an aligned word.\n",
	}))
	if err != nil || len(defects) != 3 || len(r.Concepts) != 1 {
		t.Fatalf("load: concepts %+v, defects %v, %v", r.Concepts, defects, err)
	}
	got := map[string]RefusedFile{}
	for _, refused := range r.Refused {
		got[refused.Path] = refused
	}
	for path, want := range map[string]RefusedFile{
		"concepts/Fx.Two.yaml":       {Name: "Fx.Two"},
		"concepts/" + hash + ".yaml": {Name: "Fx.Keyed", CompiledHash: hash},
		"concepts/Fx.Mess.yaml":      {},
	} {
		refused, ok := got[path]
		if !ok {
			t.Errorf("no refused file %s in %+v", path, r.Refused)
			continue
		}
		if refused.Name != want.Name || refused.CompiledHash != want.CompiledHash || refused.Defect.Path != path || refused.Defect.Message == "" {
			t.Errorf("refused file %s: %+v, want name %q and hash %q with its defect", path, refused, want.Name, want.CompiledHash)
		}
	}
	if key := got["concepts/"+hash+".yaml"].Key(); key != hash {
		t.Errorf("key %q, want %q", key, hash)
	}
}
