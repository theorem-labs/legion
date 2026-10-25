package declindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// golden is the reader output that the reference reader gave for testdata/golden: Golden.lean compiled by
// `lean -R . -o Golden.olean -i Golden.ilean Golden.lean`.
type golden struct {
	Module string `json:"module"`
	Decls  []struct {
		Name   string  `json:"name"`
		Short  string  `json:"short"`
		Module string  `json:"module"`
		Kind   *string `json:"kind"`
		Line   int     `json:"line"`
		Hash   *string `json:"hash"`
	} `json:"decls"`
	Uses [][]any `json:"uses"`
}

func readGolden(t *testing.T) (ilean, src []byte, want golden) {
	t.Helper()
	dir := filepath.Join("testdata", "golden")
	var err error
	if ilean, err = os.ReadFile(filepath.Join(dir, "Golden.ilean")); err != nil {
		t.Fatal(err)
	}
	if src, err = os.ReadFile(filepath.Join(dir, "Golden.lean")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	return ilean, src, want
}

func TestParseModuleMatchesReferenceReader(t *testing.T) {
	ilean, src, want := readGolden(t)
	got, err := ParseModule("Golden", ilean, src, Roots{"Golden": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Module != want.Module {
		t.Fatalf("module = %q, want %q", got.Module, want.Module)
	}
	decls := append([]Decl(nil), got.Decls...)
	sort.Slice(decls, func(i, j int) bool { return decls[i].Name < decls[j].Name })
	if len(decls) != len(want.Decls) {
		t.Fatalf("got %d declarations, want %d: %+v", len(decls), len(want.Decls), decls)
	}
	for i, w := range want.Decls {
		g := decls[i]
		wantDecl := Decl{Name: w.Name, Short: w.Short, Module: w.Module, Kind: *w.Kind, Line: w.Line, StmtHash: *w.Hash}
		if g != wantDecl {
			t.Errorf("declaration %d = %+v, want %+v", i, g, wantDecl)
		}
	}
	uses := append([]Use(nil), got.Uses...)
	sortUses(uses)
	var wantUses []Use
	for _, u := range want.Uses {
		// The roots keep only Golden's constants, so Golden is the module of every use's target.
		wantUses = append(wantUses, Use{
			Target: u[0].(string), TargetModule: "Golden", Module: u[1].(string), Line: int(u[2].(float64)),
			Col: int(u[3].(float64)), Parent: u[4].(string), Class: u[5].(string),
		})
	}
	if !reflect.DeepEqual(uses, wantUses) {
		t.Fatalf("uses differ:\n got %+v\nwant %+v", uses, wantUses)
	}
}

// A module's reference file names its direct imports, and the module of each constant it uses: the declaring one,
// as for a use in Sub/Wrap of Golden's Gold.Frame.
func TestParseModuleKeepsImportsAndTheModuleOfEachTarget(t *testing.T) {
	dir := filepath.Join("testdata", "golden", "Sub")
	ilean, err := os.ReadFile(filepath.Join(dir, "Wrap.ilean"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "Wrap.lean"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseModule("Sub/Wrap", ilean, src, Roots{"Golden": true, "Sub": true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Imports, []string{"Golden"}) {
		t.Errorf("imports %q, want Golden", got.Imports)
	}
	modules := map[string]string{}
	for _, u := range got.Uses {
		modules[u.Target] = u.TargetModule
	}
	if modules["Gold.Frame"] != "Golden" || modules["Gold2.FrameAt"] != "Sub.Wrap" {
		t.Errorf("target modules %v; want Gold.Frame's Golden and Gold2.FrameAt's Sub.Wrap", modules)
	}
}

func TestParseModuleWithoutSourceKeepsUsesUnclassified(t *testing.T) {
	ilean, _, _ := readGolden(t)
	got, err := ParseModule("Golden", ilean, nil, Roots{"Golden": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got.Decls {
		if d.Kind != "" || d.StmtHash != "" {
			t.Fatalf("declaration %s has kind %q hash %q without its source", d.Name, d.Kind, d.StmtHash)
		}
	}
	for _, u := range got.Uses {
		if u.Class != "unk" && u.Class != "top" {
			t.Fatalf("use %+v classified without its source", u)
		}
	}
}

func TestParseModuleSkipsReferencesOutsideTheRoots(t *testing.T) {
	ilean, src, _ := readGolden(t)
	got, err := ParseModule("Golden", ilean, src, Roots{"Other": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Uses) != 0 {
		t.Fatalf("uses of modules outside the roots were kept: %+v", got.Uses)
	}
	if len(got.Decls) == 0 {
		t.Fatal("declarations are kept whatever the roots")
	}
}

func TestParseModuleRefusesAnotherFormatVersion(t *testing.T) {
	if _, err := ParseModule("X", []byte(`{"version":4,"module":"X","decls":{},"references":{}}`), nil, Roots{}); err == nil {
		t.Fatal("a version 4 reference file was accepted")
	}
}
