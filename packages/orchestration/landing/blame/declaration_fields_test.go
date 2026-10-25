package blame

import (
	"reflect"
	"testing"
)

func TestScanDeclarationsReportsStructureFields(t *testing.T) {
	src := "namespace Lib\nstructure Box extends Base where\n  val : Nat\n  ok : val > 0 := by decide\n\ndef f := 1\nend Lib\n"
	ds := ScanDeclarations("Lib/Core.lean", src)
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
	ds := ScanDeclarations("Shape.lean", src)
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
