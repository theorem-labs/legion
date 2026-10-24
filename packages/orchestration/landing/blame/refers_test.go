package blame

import "testing"

// Lean prints a name relative to the namespaces open where the error is
// reported, so a diagnostic's dotted name is often a component suffix of the
// declaration's full name.
func TestRefersMatchesComponentSuffixes(t *testing.T) {
	memdup := Declaration{File: "Link.lean", Name: "LinuxMmu.SK.Fsc.MemdupUserNul"}
	guarded := Declaration{File: "Alpha.lean", Name: "Alpha.helper", Protected: true}
	for _, test := range []struct {
		reference   string
		declaration Declaration
		want        bool
	}{
		{"Fsc.MemdupUserNul", memdup, true},
		{"SK.Fsc.MemdupUserNul", memdup, true},
		{"LinuxMmu.SK.Fsc.MemdupUserNul", memdup, true},
		{"MemdupUserNul", memdup, true},
		{"sc.MemdupUserNul", memdup, false},
		{"Other.Fsc.MemdupUserNul", memdup, false},
		{"Beta.Shared", Declaration{Name: "Alpha.Shared"}, false},
		{"helper", guarded, false},
		{"Alpha.helper", guarded, true},
		{"Outer.Alpha.helper", guarded, false},
	} {
		if got := Refers(test.reference, test.declaration); got != test.want {
			t.Errorf("Refers(%q, %q) = %v, want %v", test.reference, test.declaration.Name, got, test.want)
		}
	}
}
