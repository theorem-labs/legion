package blame

import "testing"

func TestEvidenceForScoresInsideEvidence(t *testing.T) {
	evidence := newEvidence()
	evidence.Inside[DeclarationID{Name: "Changed"}] = Declaration{Name: "Changed"}
	strength, label := EvidenceFor(Name{Value: "Changed", Kind: Changed}, evidence)
	if strength != 2 || label != "edited" {
		t.Fatalf("inside evidence = (%d, %q), want (2, edited)", strength, label)
	}
}

func TestReviewAttributedDeclarationAndPrivateConstructorPatterns(t *testing.T) {
	evidence := IndexDeclarations([]Declaration{{Name: "Marked", Text: "@[tag] theorem Marked : True := by trivial"}}, nil)
	strength, label := EvidenceFor(Name{Value: "Marked", Kind: Unknown}, evidence)
	if strength != 3 || label != "removed" {
		t.Fatalf("attributed declaration evidence = (%d, %q), want (3, removed)", strength, label)
	}
	for _, message := range []string{
		"Constructor for `_private.Mod.0.Hidden` is marked as private",
		"from structure `_private.Mod.0.Hidden` is private",
		"`_private.Alpha.0.Record` is private",
	} {
		names := Names(message)
		if len(names) == 0 || names[0].Kind != Unknown {
			t.Fatalf("private diagnostic %q names = %#v, want unknown", message, names)
		}
	}
}

func TestReviewParseAcceptsTimedStatusAndHeaderImport(t *testing.T) {
	report := Parse("✖ [1/2] Building Alpha.A (1.4s)\nerror: Alpha/A.lean:1:1: import Alpha.B failed\n", []string{"Alpha"})
	if got, want := report.RedModules, []string{"Alpha.A", "Alpha.B"}; !sameStrings(got, want) {
		t.Fatalf("red modules = %#v, want %#v", got, want)
	}
}

func TestReviewPickUsesOriginalWeakThreshold(t *testing.T) {
	selection := Pick([]Row{
		{Member: "one", Strength: 2, Label: "edited"},
		{Member: "two", Strength: 2, Label: "edited"},
		{Member: "three", Strength: 1, Label: "added"},
		{Member: "four", Strength: 1, Label: "added"},
	})
	if got, want := selection.Members, []Row{{Member: "one", Strength: 2, Label: "edited"}, {Member: "two", Strength: 2, Label: "edited"}, {Member: "four", Strength: 1, Label: "added"}}; !sameRows(got, want) {
		t.Fatalf("weak selection = %#v, want %#v", got, want)
	}
}
