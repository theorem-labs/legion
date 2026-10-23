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

func TestLoadRefusesInvalidCompiledHash(t *testing.T) {
	dir := writeRegistry(t, t.TempDir(), map[string]string{
		"not-a-hash.yaml": "name: Fx.Bad\nhome: Bad.lean\nmeaning: A badly keyed fact.\ncompiled_hash: not-a-hash\n",
	})
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "compiled hash") {
		t.Fatalf("invalid compiled hash error: %v", err)
	}
}
