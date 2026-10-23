package blame

import "testing"

func TestScanDeclarationsQualifiesScopesAndModifiers(t *testing.T) {
	source := `namespace Alpha
namespace Beta
@[tag] noncomputable unsafe partial def annotated := 0
protected def guarded := 1
private def hidden := 2
section Local
def scoped := 3
end Local
end Beta
namespace A.B
def dotted := 4
end A.B
def _root_.rooted := 5
end Alpha
`
	got := ScanDeclarations("Declarations.lean", source)
	want := []Declaration{
		{File: "Declarations.lean", Name: "Alpha.Beta.annotated", Text: "@[tag] noncomputable unsafe partial def annotated := 0"},
		{File: "Declarations.lean", Name: "Alpha.Beta.guarded", Text: "protected def guarded := 1", Protected: true},
		{File: "Declarations.lean", Name: "Alpha.Beta.hidden", Text: "private def hidden := 2", Private: true},
		{File: "Declarations.lean", Name: "Alpha.Beta.scoped", Text: "def scoped := 3"},
		{File: "Declarations.lean", Name: "Alpha.A.B.dotted", Text: "def dotted := 4"},
		{File: "Declarations.lean", Name: "rooted", Text: "def _root_.rooted := 5"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() returned %d declarations: %#v, want %d", len(got), got, len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("declaration %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestScanDeclarationsDoesNotScopeCommentsOrStrings(t *testing.T) {
	source := `namespace Alpha
-- namespace Beta
/- namespace Gamma
  /- end Alpha -/
-/
def first := "end Alpha"
def second := 0
end Alpha
`
	got := ScanDeclarations("Comments.lean", source)
	want := []Declaration{
		{File: "Comments.lean", Name: "Alpha.first", Text: `def first := "end Alpha"`},
		{File: "Comments.lean", Name: "Alpha.second", Text: "def second := 0"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() returned %d declarations: %#v, want %d", len(got), got, len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("declaration %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}
