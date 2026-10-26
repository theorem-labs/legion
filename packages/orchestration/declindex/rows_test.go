package declindex

import (
	"reflect"
	"slices"
	"testing"
)

func TestDottedPrefixesYieldTheNameThenEachShorterPrefix(t *testing.T) {
	for name, want := range map[string][]string{
		"A.B.c": {"A.B.c", "A.B", "A"},
		"A":     {"A"},
		"":      nil,
		".x":    {".x"},
		"a..b":  {"a..b", "a.", "a"},
	} {
		if got := slices.Collect(dottedPrefixes(name)); !reflect.DeepEqual(got, want) {
			t.Errorf("dottedPrefixes(%q) = %q, want %q", name, got, want)
		}
	}
}
