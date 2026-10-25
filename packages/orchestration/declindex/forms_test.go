package declindex

import (
	"strings"
	"testing"
)

func TestFormsModuleNamesOneConstantPerForm(t *testing.T) {
	fm := FormsModule{
		Imports: []string{"Fixture"},
		Opens:   []string{"Fx"},
		Concepts: []ConceptForms{
			{Concept: "Fx.SpAligned", Forms: []string{"fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64", "fun (x : Nat) => x = x"}},
			{Concept: "Fx.Frame"},
		},
	}
	src, refs := fm.Source()
	for _, want := range []string{"import Fixture\n", "open Fx\n", "namespace DeclIndexForms\n",
		"noncomputable def «form_0_0» :=\n  fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64\n",
		"noncomputable def «form_0_1» :=\n  fun (x : Nat) => x = x\n", "end DeclIndexForms\n"} {
		if !strings.Contains(src, want) {
			t.Errorf("source lacks %q:\n%s", want, src)
		}
	}
	wantRefs := []ConstRef{{"Fx.SpAligned", "DeclIndexForms.form_0_0"}, {"Fx.SpAligned", "DeclIndexForms.form_0_1"}, {"Fx.Frame", "Fx.Frame"}}
	if len(refs) != len(wantRefs) {
		t.Fatalf("refs %v, want %v", refs, wantRefs)
	}
	for i := range refs {
		if refs[i] != wantRefs[i] {
			t.Errorf("ref %d = %v, want %v", i, refs[i], wantRefs[i])
		}
	}
}

func TestFormsModuleRefusesAFormThatEndsTheDefinition(t *testing.T) {
	fm := FormsModule{Concepts: []ConceptForms{{Concept: "C", Forms: []string{"fun x => x\nend DeclIndexForms"}}}}
	if err := fm.Check(); err == nil {
		t.Fatal("a form that ends the namespace was accepted")
	}
}
