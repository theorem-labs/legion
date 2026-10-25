package blame

import "testing"

func TestReviewIdentifyDetailedCarriesActualMemberEvidence(t *testing.T) {
	report := Report{Blocks: []Block{{Module: "Alpha.Mod", Message: "Unknown identifier `gone`"}}}
	evidence := map[string]Evidence{
		"member": IndexDeclarations([]Declaration{{Name: "gone", Text: "def gone := 0"}}, nil),
	}

	attributions := IdentifyDetailed(report, evidence, nil)
	if got, want := attributions, []Attribution{{Module: "Alpha.Mod", Name: "gone", Kind: Unknown, Member: "member", Strength: 3, Evidence: "removed"}}; !sameAttributions(got, want) {
		t.Fatalf("attributions = %#v, want %#v", got, want)
	}
}

func TestReviewIdentifyDetailedSuppressesStaleRestatementAtRuntime(t *testing.T) {
	report := Report{Blocks: []Block{{Module: "Alpha.Mod", Message: "environment already contains 'Shared'"}}}
	evidence := map[string]Evidence{"old": IndexDeclarations(nil, []Declaration{{Name: "Shared", Text: "def Shared := 0"}})}
	lines := map[string]TreeLines{"Shared": {Base: []Declaration{{Name: "Shared", Text: "def Shared := 0"}}, Candidate: []Declaration{{Name: "Shared", Text: "def Shared := 0"}}}}

	if got := IdentifyDetailed(report, evidence, lines); len(got) != 0 {
		t.Fatalf("attributions = %#v, want stale restatement suppressed", got)
	}
}

func sameAttributions(got, want []Attribution) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
