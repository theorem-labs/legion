package declindex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestHeaderImports(t *testing.T) {
	src := []byte("/- doc -/\nprelude\nimport Lib.A\npublic import Lib.B Lib.C -- two\n\nnamespace X\nimport Lib.D\n")
	if got, want := HeaderImports(src), []string{"Lib.A", "Lib.B", "Lib.C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HeaderImports = %v, want %v", got, want)
	}
}

// A comment marker inside a string is no comment, and a block comment can hold a line that reads as an import.
func TestHeaderImportsSkipsCommentsButNotStrings(t *testing.T) {
	src := []byte("/- import Lib.Hidden\n-/ import Lib.A\nimport Lib.B\ndef s := \"-- import Lib.C\"\nimport Lib.D\n")
	if got, want := HeaderImports(src), []string{"Lib.A", "Lib.B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("HeaderImports = %v, want %v", got, want)
	}
}

// HeaderImports reads from a candidate's source text the imports that the index records for a built module from
// its reference file, so graph checks compare a candidate's imports with the base's: for every module of the
// golden fixture, the two agree.
func TestHeaderImportsGivesTheImportsTheIndexStores(t *testing.T) {
	s := refreshedGolden(t)
	ctx := context.Background()
	stored, err := s.Imports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := s.ModulePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(stored) || len(paths) == 0 {
		t.Fatalf("ModulePaths %v and Imports %v name different modules", paths, stored)
	}
	for module, path := range paths {
		src, err := os.ReadFile(filepath.Join("testdata", "golden", filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		header := HeaderImports(src)
		slices.Sort(header)
		if want := stored[module]; !slices.Equal(header, want) {
			t.Errorf("module %s: HeaderImports = %v, the index stores %v", module, header, want)
		}
	}
}
