package conceptgate

import (
	"slices"
	"testing"
)

// Matches sort by concept, then declaration, then how: field by field, so that "a"+"bc" and "ab"+"c" are two
// different keys.
func TestSortMatchesComparesFieldByField(t *testing.T) {
	want := []Match{
		{Concept: "a", Declaration: "bc", How: "direct"},
		{Concept: "ab", Declaration: "c", How: "direct"},
		{Concept: "ab", Declaration: "c", How: "via:x"},
	}
	for _, input := range [][]Match{
		{want[1], want[0], want[2]},
		{want[2], want[1], want[0]},
	} {
		got := slices.Clone(input)
		sortMatches(got)
		if !slices.Equal(got, want) {
			t.Fatalf("sortMatches(%v) = %v, want %v", input, got, want)
		}
	}
}
