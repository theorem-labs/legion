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

func declarationNames(declarations []Declaration) []string {
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		names = append(names, declaration.Name)
	}
	return names
}

// Each source holds a construct that must not change the namespace that
// `after` is declared in.
func TestScanDeclarationsKeepsScopeAcrossConstructs(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{"char literal quote", "namespace Alpha\ndef q := '\"' /- \"\nnamespace Hidden\n-/\ndef after := 0\nend Alpha\n", []string{"Alpha.q", "Alpha.after"}},
		{"escaped char literal", "namespace Alpha\ndef q := '\\'' -- \"\ndef after := 0\nend Alpha\n", []string{"Alpha.q", "Alpha.after"}},
		{"multi-line string", "namespace Alpha\ndef s := \"first\nend Alpha\nstill string\"\ndef after := 0\nend Alpha\n", []string{"Alpha.s", "Alpha.after"}},
		{"attributed section", "namespace Alpha\n@[expose] public section\ndef inSection := 0\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.inSection", "Alpha.after"}},
		{"class inductive", "namespace Alpha\nclass inductive Good : Prop\n  | intro\nclass abbrev Both := Good\ndef after := 0\nend Alpha\n", []string{"Alpha.Good", "Alpha.Both", "Alpha.after"}},
		{"mutual", "namespace Alpha\nmutual\n  def even : Nat → Bool\n    | 0 => true\n    | n + 1 => odd n\n  def odd : Nat → Bool\n    | 0 => false\n    | n + 1 => even n\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.even", "Alpha.odd", "Alpha.after"}},
		{"anonymous section", "namespace Alpha\nsection\nvariable (n : Nat)\ndef inSection := n\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.inSection", "Alpha.after"}},
		{"deriving", "namespace Alpha\nstructure Point where\n  x : Nat\nderiving Repr\ndef after := 0\nend Alpha\n", []string{"Alpha.Point", "Alpha.after"}},
		{"attribute line", "namespace Alpha\n@[simp]\ntheorem lemma_one : True := trivial\ndef after := 0\nend Alpha\n", []string{"Alpha.lemma_one", "Alpha.after"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := declarationNames(ScanDeclarations("Construct.lean", test.source)); !sameStrings(got, test.want) {
				t.Fatalf("ScanDeclarations() names = %q, want %q", got, test.want)
			}
		})
	}
}

// A modifier written on its own line belongs to the declaration below it.
func TestScanDeclarationsCarriesModifierLines(t *testing.T) {
	source := "namespace Alpha\nprivate\ndef hidden := 0\n@[simp] protected\ntheorem guarded : True := trivial\ndef open_ := 0\nend Alpha\n"
	got := ScanDeclarations("Modifiers.lean", source)
	if names := declarationNames(got); !sameStrings(names, []string{"Alpha.hidden", "Alpha.guarded", "Alpha.open_"}) {
		t.Fatalf("ScanDeclarations() names = %q", names)
	}
	if !got[0].Private || got[0].Protected {
		t.Errorf("hidden = %+v, want private only", got[0])
	}
	if got[1].Private || !got[1].Protected {
		t.Errorf("guarded = %+v, want protected only", got[1])
	}
	if got[2].Private || got[2].Protected {
		t.Errorf("open_ = %+v, want neither modifier", got[2])
	}
}

// A declaration's Text is its whole signature: the header and every
// continuation line up to the body, joined with single spaces. Comments and
// blank lines inside the signature are not part of it, and a match arm of a
// definition starts its body.
func TestScanDeclarationsJoinsSignatureLines(t *testing.T) {
	source := "theorem T (n : Nat)\n    (h : n = 0) -- why\n\n    : True := by\n  trivial\n" +
		"def f : Nat → Nat\n  | 0 => 1\n  | n + 1 => n\n" +
		"structure Point (α : Type)\n    (β : Type) where\n  x : α\n" +
		"inductive Tree\n  | leaf\n  | node (l r : Tree)\n" +
		"theorem open_ (n : Nat)\n    (h : n = 0)\n"
	got := ScanDeclarations("Signatures.lean", source)
	want := map[string]string{
		"T":     "theorem T (n : Nat) (h : n = 0) : True := by",
		"f":     "def f : Nat → Nat",
		"Point": "structure Point (α : Type) (β : Type) where",
		"Tree":  "inductive Tree | leaf | node (l r : Tree)",
		"open_": "theorem open_ (n : Nat) (h : n = 0)",
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() = %+v, want %d declarations", got, len(want))
	}
	for _, declaration := range got {
		if declaration.Text != want[declaration.Name] {
			t.Errorf("%s Text = %q, want %q", declaration.Name, declaration.Text, want[declaration.Name])
		}
	}
}
