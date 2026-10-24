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

// A universe-polymorphic name `S.{u}` declares S, and an instance with a
// priority still declares its name.
func TestScanDeclarationsNamesUniversesAndPriorities(t *testing.T) {
	source := `namespace A
structure S.{u} (α : Sort u) where
  x : α
def T.{u, v} (α : Sort u) (β : Sort v) : Prop := True
instance (priority := low) feedR_drop : Inhabited Nat := ⟨0⟩
end A
`
	got := ScanDeclarations("Universe.lean", source)
	var names []string
	for _, declaration := range got {
		names = append(names, declaration.Name)
	}
	if want := []string{"A.S", "A.T", "A.feedR_drop"}; !sameStrings(names, want) {
		t.Fatalf("ScanDeclarations() names = %q, want %q", names, want)
	}
	if found := DeclarationsFor("A.S", got); len(found) != 1 || found[0].Name != "A.S" {
		t.Fatalf("DeclarationsFor(A.S) = %+v, want the universe-polymorphic structure", found)
	}
}
