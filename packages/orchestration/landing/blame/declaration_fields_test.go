package blame

import (
	"reflect"
	"testing"
)

func TestScanDeclarationsReportsStructureFields(t *testing.T) {
	src := "namespace Lib\nstructure Box extends Base where\n  val : Nat\n  ok : val > 0 := by decide\n\ndef f := 1\nend Lib\n"
	ds := ScanDeclarations("Lib/Core.lean", src, nil)
	if len(ds) != 2 {
		t.Fatalf("declarations = %+v", ds)
	}
	box, f := ds[0], ds[1]
	if box.Name != "Lib.Box" || box.Kind != "structure" || box.Line != 2 || !box.Extends ||
		!reflect.DeepEqual(box.Fields, []string{"val", "ok"}) {
		t.Fatalf("box = %+v", box)
	}
	if f.Name != "Lib.f" || f.Kind != "def" || f.Line != 6 || f.Fields != nil || f.Extends {
		t.Fatalf("f = %+v", f)
	}
}

// A field's continuation lines, deeper indentation, a constructor line and a
// class without extends: only the body's own fields at its first indentation.
func TestScanDeclarationsReadsOnlyTopLevelFields(t *testing.T) {
	src := "class Shape (α : Type) where\n  mk ::\n  area : α →\n    Nat\n  name : String\n  deriving Repr\ntheorem t : True := trivial\n"
	ds := ScanDeclarations("Shape.lean", src, nil)
	if len(ds) != 2 {
		t.Fatalf("declarations = %+v", ds)
	}
	shape := ds[0]
	if shape.Kind != "class" || shape.Line != 1 || shape.Extends || !reflect.DeepEqual(shape.Fields, []string{"area", "name"}) {
		t.Fatalf("shape = %+v", shape)
	}
	if ds[1].Kind != "theorem" || ds[1].Line != 7 {
		t.Fatalf("theorem = %+v", ds[1])
	}
}

// Every Lean field form gives its names, in order: binders, several names on
// one line, a modifier, an attribute. A default that overrides a parent's
// field, the constructor line and `deriving` declare no field.
func TestScanDeclarationsReadsEveryFieldForm(t *testing.T) {
	for name, test := range map[string]struct {
		line string
		want []string
	}{
		"binders":         {"  f (n : Nat) {m : Nat} [inst : Inhabited α] : Fin (n+1)", []string{"f"}},
		"several names":   {"  x y : Nat", []string{"x", "y"}},
		"protected":       {"  protected p : Nat", []string{"p"}},
		"private":         {"  private q : Nat", []string{"q"}},
		"attribute":       {"  @[simp] h : 0 = 0", []string{"h"}},
		"default":         {"  d : Nat := 3", []string{"d"}},
		"primed":          {"  v' : Nat", []string{"v'"}},
		"parent default":  {"  base := 0", nil},
		"constructor":     {"  mk' ::", nil},
		"deriving":        {"  deriving Repr", nil},
		"proof by tactic": {"  ok : True := by\n    trivial", []string{"ok"}},
	} {
		t.Run(name, func(t *testing.T) {
			src := "structure S extends Base where\n  z : Nat\n" + test.line + "\n  w : Nat\n"
			ds := ScanDeclarations("S.lean", src, nil)
			want := append(append([]string{"z"}, test.want...), "w")
			if len(ds) != 1 || !reflect.DeepEqual(ds[0].Fields, want) {
				t.Fatalf("declarations = %+v, want fields %q", ds, want)
			}
		})
	}
}
