package declindex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSourceShapesGiveRenamedTheoremsTheSameShape(t *testing.T) {
	shapes := SourceShapes(readShapesGolden(t))
	original := sourceShapeAt(t, shapes, 3)
	copy := sourceShapeAt(t, shapes, 5)
	if original.Kind != "theorem" || copy.Kind != "theorem" {
		t.Fatalf("shapes = %+v, want theorem entries at lines 3 and 5", shapes)
	}
	if original.Shape != copy.Shape {
		t.Fatalf("the theorem copies have shapes %q and %q, want one shape", original.Shape, copy.Shape)
	}
}

func TestSourceShapesNormalizePrimedDeclarationNames(t *testing.T) {
	src := []byte("theorem frame' (h : Input) : Output := by sorry\ntheorem frameCopy (h : Input) : Output := by sorry\n")
	shapes := SourceShapes(src)
	if first, second := sourceShapeAt(t, shapes, 1), sourceShapeAt(t, shapes, 2); first.Shape != second.Shape {
		t.Fatalf("primed and unprimed copies have shapes %q and %q, want one shape", first.Shape, second.Shape)
	}
}

func TestSourceHeadsReadGoldenTheoremBindersAndConclusion(t *testing.T) {
	_, src, _ := readGolden(t)
	heads := SourceHeads(src)
	binder := sourceHeadAt(t, heads, 14, "binder")
	conclusion := sourceHeadAt(t, heads, 14, "conclusion")
	if got, want := headsAt(heads, 14), []SourceHead{
		{Line: 14, Role: "binder", Written: "Frame", Namespace: "Gold"},
		{Line: 14, Role: "conclusion", Written: "Aligned", Namespace: "Gold"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("heads = %+v, want %+v", got, want)
	}
	if binder.Written != "Frame" || binder.Namespace != "Gold" {
		t.Fatalf("binder = %+v, want Gold.Frame", binder)
	}
	if conclusion.Written != "Aligned" || conclusion.Namespace != "Gold" {
		t.Fatalf("conclusion = %+v, want Gold.Aligned", conclusion)
	}
	declared := map[string]bool{"Gold.Frame": true, "Gold.Aligned": true}
	if got := ResolveHead(binder, func(name string) bool { return declared[name] }); got != "Gold.Frame" {
		t.Fatalf("ResolveHead(%+v) = %q, want Gold.Frame", binder, got)
	}
	if got := ResolveHead(conclusion, func(name string) bool { return declared[name] }); got != "Gold.Aligned" {
		t.Fatalf("ResolveHead(%+v) = %q, want Gold.Aligned", conclusion, got)
	}
}

func TestSourceHeadsCarryOpenAndNamespaceScopes(t *testing.T) {
	src := []byte(`namespace Outer
open Imported.One Imported.Two
namespace Inner
theorem hasHeads (h : ∀ n, Gate n) : Result := by
  sorry
end Inner
end Outer
`)
	heads := SourceHeads(src)
	binder := sourceHeadAt(t, heads, 4, "binder")
	conclusion := sourceHeadAt(t, heads, 4, "conclusion")
	if binder.Written != "Gate" || binder.Namespace != "Outer.Inner" ||
		!reflect.DeepEqual(binder.Opens, []string{"Outer.Imported.One", "Imported.One", "Outer.Imported.Two", "Imported.Two"}) {
		t.Fatalf("binder = %+v, want Gate in Outer.Inner with both opens", binder)
	}
	declared := map[string]bool{"Imported.One.Gate": true, "Outer.Inner.Result": true}
	if got := ResolveHead(binder, func(name string) bool { return declared[name] }); got != "Imported.One.Gate" {
		t.Fatalf("ResolveHead(%+v) = %q, want Imported.One.Gate", binder, got)
	}
	if got := ResolveHead(conclusion, func(name string) bool { return declared[name] }); got != "Outer.Inner.Result" {
		t.Fatalf("ResolveHead(%+v) = %q, want Outer.Inner.Result", conclusion, got)
	}
}

func TestResolveHeadPrefersNestedNamespacesThenOpensThenWrittenName(t *testing.T) {
	head := SourceHead{Written: "T", Namespace: "A.B.C", Opens: []string{"Imported.One", "Imported.Two"}}
	for _, test := range []struct {
		name     string
		declared map[string]bool
		want     string
	}{
		{name: "innermost namespace", declared: map[string]bool{"A.B.C.T": true}, want: "A.B.C.T"},
		{name: "outer namespace", declared: map[string]bool{"A.B.T": true}, want: "A.B.T"},
		{name: "first open", declared: map[string]bool{"Imported.One.T": true}, want: "Imported.One.T"},
		{name: "second open", declared: map[string]bool{"Imported.Two.T": true}, want: "Imported.Two.T"},
		{name: "written name", declared: map[string]bool{"T": true}, want: "T"},
		{name: "undeclared name", declared: nil, want: "T"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveHead(head, func(name string) bool { return test.declared[name] }); got != test.want {
				t.Fatalf("ResolveHead = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSourceShapesAndHeadsHandleWrappedAndMutualDeclarations(t *testing.T) {
	src := []byte(`namespace Scope
@[simp] theorem attributed (h : Input) : Output := by
  sorry
open Imported in theorem opened (h : Shared) : Result := by
  sorry
mutual
  def first (h : Input) : Output := by
    sorry
  theorem second (h : Input) : Output := by
    sorry
end
theorem after (h : Input) : Output := by
  sorry
end Scope
`)
	shapes := SourceShapes(src)
	for _, line := range []int{2, 4, 7, 9, 12} {
		sourceShapeAt(t, shapes, line)
	}
	heads := SourceHeads(src)
	if got, want := headsAt(heads, 2), []SourceHead{
		{Line: 2, Role: "binder", Written: "Input", Namespace: "Scope"},
		{Line: 2, Role: "conclusion", Written: "Output", Namespace: "Scope"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attribute heads = %+v, want %+v", got, want)
	}
	if got, want := headsAt(heads, 4), []SourceHead{
		{Line: 4, Role: "binder", Written: "Shared", Opens: []string{"Scope.Imported", "Imported"}, Namespace: "Scope"},
		{Line: 4, Role: "conclusion", Written: "Result", Opens: []string{"Scope.Imported", "Imported"}, Namespace: "Scope"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped-open heads = %+v, want %+v", got, want)
	}
	for _, line := range []int{7, 9, 12} {
		if got, want := headsAt(heads, line), []SourceHead{
			{Line: line, Role: "binder", Written: "Input", Namespace: "Scope"},
			{Line: line, Role: "conclusion", Written: "Output", Namespace: "Scope"},
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("heads at line %d = %+v, want %+v", line, got, want)
		}
	}
}

func TestSourceShapesAndHeadsSpanMultilineDeclarations(t *testing.T) {
	src := []byte(`theorem flat (h : Input) : Output := by
  sorry
theorem wrapped (h : Input) :
  Output := by
  sorry
def first : Nat
  | 0 => 1
  | _ => 2
def second : Nat
  | 0 => 3
  | _ => 4
`)
	shapes := SourceShapes(src)
	if flat, wrapped := sourceShapeAt(t, shapes, 1), sourceShapeAt(t, shapes, 3); flat.Shape != wrapped.Shape {
		t.Fatalf("flat and wrapped theorem shapes = %q and %q, want one shape", flat.Shape, wrapped.Shape)
	}
	if first, second := sourceShapeAt(t, shapes, 6), sourceShapeAt(t, shapes, 9); first.Shape == second.Shape {
		t.Fatalf("definition shapes are both %q after different bodies", first.Shape)
	}
	if got, want := headsAt(SourceHeads(src), 3), []SourceHead{
		{Line: 3, Role: "binder", Written: "Input"},
		{Line: 3, Role: "conclusion", Written: "Output"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapped theorem heads = %+v, want %+v", got, want)
	}
}

func TestSourceHeadsCoverEachSupportedDeclarationKind(t *testing.T) {
	src := []byte(`theorem theoremHead (h : Input) : Output := by sorry
lemma lemmaHead (h : Input) : Output := by sorry
def defHead (h : Input) : Output := by sorry
abbrev abbrevHead (h : Input) : Output := by sorry
structure StructureHead (h : Input) : Output where
  field : Input
class IgnoredHead (h : Input) : Output where
  field : Input
`)
	heads := SourceHeads(src)
	for _, line := range []int{1, 2, 3, 4, 5} {
		if got, want := headsAt(heads, line), []SourceHead{
			{Line: line, Role: "binder", Written: "Input"},
			{Line: line, Role: "conclusion", Written: "Output"},
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("heads at line %d = %+v, want %+v", line, got, want)
		}
	}
	if got := headsAt(heads, 7); len(got) != 0 {
		t.Fatalf("class heads = %+v, want none", got)
	}
}

func TestResolveHeadStripsRootQualification(t *testing.T) {
	head := SourceHead{Written: "_root_.T", Namespace: "Nested", Opens: []string{"Opened"}}
	if got := ResolveHead(head, func(name string) bool { return name == "T" }); got != "T" {
		t.Fatalf("ResolveHead(%+v) = %q, want T", head, got)
	}
}

func TestSourceHeadsIgnorePiBindersInConclusions(t *testing.T) {
	heads := SourceHeads([]byte(`theorem result : (h : Link.Contract) → True := by sorry
`))
	for _, head := range heads {
		if head.Role == "binder" {
			t.Fatalf("conclusion Pi binder became a declaration binder: %+v", head)
		}
	}
}

func TestSourceHeadsResolveRelativeAndMutualScopedOpens(t *testing.T) {
	src := []byte(`namespace Parent
open Local
theorem regular (h : H) : Output := by sorry
open Scoped in
mutual
  theorem mutually (h : T) : U := by sorry
end
end Parent
`)
	heads := SourceHeads(src)
	regular := sourceHeadAt(t, heads, 3, "binder")
	if got, want := regular.Opens, []string{"Parent.Local", "Local"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("regular opens = %q, want %q", got, want)
	}
	mutual := sourceHeadAt(t, heads, 6, "binder")
	if got, want := mutual.Opens, []string{"Parent.Local", "Local", "Parent.Scoped", "Scoped"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mutual opens = %q, want %q", got, want)
	}
	declared := map[string]bool{"Parent.Local.H": true, "Parent.Scoped.T": true}
	if got := ResolveHead(regular, func(name string) bool { return declared[name] }); got != "Parent.Local.H" {
		t.Fatalf("ResolveHead(%+v) = %q, want Parent.Local.H", regular, got)
	}
	if got := ResolveHead(mutual, func(name string) bool { return declared[name] }); got != "Parent.Scoped.T" {
		t.Fatalf("ResolveHead(%+v) = %q, want Parent.Scoped.T", mutual, got)
	}
}

func headsAt(heads []SourceHead, line int) []SourceHead {
	var out []SourceHead
	for _, head := range heads {
		if head.Line == line {
			out = append(out, head)
		}
	}
	return out
}

func readShapesGolden(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "golden", "Shapes.lean"))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func sourceShapeAt(t *testing.T, shapes []SourceShape, line int) SourceShape {
	t.Helper()
	for _, shape := range shapes {
		if shape.Line == line {
			return shape
		}
	}
	t.Fatalf("SourceShapes has no declaration at line %d: %+v", line, shapes)
	return SourceShape{}
}

func sourceHeadAt(t *testing.T, heads []SourceHead, line int, role string) SourceHead {
	t.Helper()
	for _, head := range heads {
		if head.Line == line && head.Role == role {
			return head
		}
	}
	t.Fatalf("SourceHeads has no %s head at line %d: %+v", role, line, heads)
	return SourceHead{}
}

// definitionHash is the statement hash of a definition whose text is all of text.
func definitionHash(text string) string {
	return StatementHash(StatementText(text, 0, len(text), 0, "def"))
}

// White space inside a string or character literal is part of its value: a change to it changes the statement
// hash. White space between tokens, comments and doc strings are not part of the statement.
func TestStatementHashKeepsTheWhiteSpaceOfLiterals(t *testing.T) {
	base := definitionHash(`def message : String := "a b"`)
	for _, c := range []struct {
		name, text string
		same       bool
	}{
		{"two spaces in a string", `def message : String := "a  b"`, false},
		{"a line break in a string", "def message : String := \"a\nb\"", false},
		{"a tab in a string", "def message : String := \"a\tb\"", false},
		{"white space between tokens", "def  message :\n    String :=\n  \"a b\"", true},
		{"a comment", `def message : String := "a b" -- it's "one" value`, true},
		{"a doc string", "/-- The message: \"a  b\", isn't it? -/\ndef message : String := \"a b\"", true},
	} {
		if got := definitionHash(c.text) == base; got != c.same {
			t.Errorf("%s: hash equal to the base %v, want %v", c.name, got, c.same)
		}
	}
	space, tab := definitionHash(`def c : Char := ' '`), definitionHash("def c : Char := '\t'")
	if space == tab {
		t.Error("a space and a tab character literal have one statement hash")
	}
	for _, pair := range [][2]string{
		{`def message : String := r"a b"`, `def message : String := r"a  b"`},
		{`def message : String := r#"say "a b""#`, `def message : String := r#"say "a  b""#`},
	} {
		if definitionHash(pair[0]) == definitionHash(pair[1]) {
			t.Errorf("%s and %s have one statement hash", pair[0], pair[1])
		}
	}
	if primed := definitionHash("def f' (x' : Nat) : Nat := x' + 1"); primed != definitionHash("def f'  (x' : Nat)\n  : Nat := x'  +  1") {
		t.Error("the primes of names were read as character literals")
	}
	if image := definitionHash("def g := f '' s ∪ f '' t"); image != definitionHash("def g := f ''  s ∪\n  f '' t") {
		t.Error("the quotes of notation were read as character literals")
	}
}
