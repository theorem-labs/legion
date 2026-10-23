package landing

import "testing"

func TestReviewLandRequestCarriesCandidateInputs(t *testing.T) {
	request := LandRequest{
		Repo:             "repo",
		Migration:        "member",
		Generation:       4,
		Commit:           "merged",
		ChangedPaths:     []string{"file"},
		Current:          map[string]string{"Read": "hash"},
		ChangedContracts: map[string]string{"Contract": "changed"},
		ApprovedPinned:   []string{"Contract"},
	}
	if request.Commit != "merged" || request.Current["Read"] != "hash" || request.ChangedContracts["Contract"] != "changed" {
		t.Fatalf("LandRequest lost candidate inputs: %#v", request)
	}
}
