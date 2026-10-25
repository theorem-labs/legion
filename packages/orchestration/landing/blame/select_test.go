package blame

import (
	"reflect"
	"testing"
)

func TestSelectForIsIdentifyDetailedForOneName(t *testing.T) {
	evidence := map[string]Evidence{
		"one": IndexDeclarations([]Declaration{{Name: "gone", Text: "def gone := 0"}}, nil),
		"two": IndexDeclarations(nil, []Declaration{{Name: "other", Text: "def other := 1"}}),
	}
	got := SelectFor(Name{Value: "gone", Kind: Unknown}, evidence, TreeLines{})
	want := Selection{Strength: 3, Members: []Row{{Member: "one", Strength: 3, Label: "removed"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectFor = %+v, want %+v", got, want)
	}
	report := Report{Blocks: []Block{{Module: "Alpha.Use", Message: "Unknown identifier `gone`"}}}
	if a := IdentifyDetailed(report, evidence, nil); len(a) != 1 || a[0].Member != "one" || a[0].Evidence != "removed" {
		t.Fatalf("IdentifyDetailed = %+v", a)
	}
}

// Six members privatise one name, so Pick bounds it and SelectFor names nobody.
func TestSelectForBoundsBroadEvidence(t *testing.T) {
	evidence := map[string]Evidence{}
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		evidence[id] = IndexDeclarations([]Declaration{{Name: "helper", Text: "def helper := 0"}},
			[]Declaration{{Name: "helper", Text: "private def helper := 0", Private: true}})
	}
	got := SelectFor(Name{Value: "helper", Kind: Unknown}, evidence, TreeLines{})
	if len(got.Members) != 0 || got.Note != "too many strong matches" {
		t.Fatalf("SelectFor = %+v, want no member and the strong-matches note", got)
	}
}

// A graph finding of kind field names the field by its qualified name, as the
// evidence records it; build blame names it bare. Both select the member.
func TestSelectForMatchesAQualifiedField(t *testing.T) {
	evidence := map[string]Evidence{"culprit": NewEvidence(), "bystander": NewEvidence()}
	evidence["culprit"].FieldsRemoved["Lib.Core.Box.val"] = struct{}{}
	evidence["bystander"].FieldsRemoved["Lib.Core.Other.size"] = struct{}{}
	for _, name := range []string{"Lib.Core.Box.val", "Box.val", "val"} {
		got := SelectFor(Name{Value: name, Kind: Field}, evidence, TreeLines{})
		if len(got.Members) != 1 || got.Members[0].Member != "culprit" {
			t.Errorf("SelectFor(%q) = %+v, want culprit", name, got)
		}
	}
}
