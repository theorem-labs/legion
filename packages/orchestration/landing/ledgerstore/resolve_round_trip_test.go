package ledgerstore_test

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
)

// An operator's resolution of a batch that GitHub merged and the ledger did not land reaches the landing program:
// the ledger writes the merged commit into the stored record, and the program's decoder reads it back, so the
// restarted hooks and the refresh lookup see the commit the operator supplied. The program and the ledger must name
// that field alike (pull_request.merged_commit); with two spellings the decoder reads an empty commit.
func TestResolveBatchReachesTheLandingDecoder(t *testing.T) {
	ctx := context.Background()
	batches, _, store, admitted, _ := admittedBatch(t)

	// GitHub merged the pull request and the ledger refused the landing, so the program stored
	// landed_unrecorded with the pull request it knows and no merged commit.
	admitted.State = landing.LandedUnrecorded
	admitted.Reason = "merged, but the ledger refused the landing"
	admitted.EscalateTo = "landing-operator"
	admitted.PullRequest = landing.PullRequest{Number: 7, Head: "candidate-1", State: landing.PullRequestMerged}
	if err := batches.Save(ctx, &admitted); err != nil {
		t.Fatal(err)
	}
	if loaded, _, err := batches.Load(ctx, "batch-1"); err != nil || loaded.PullRequest.Number != 7 || loaded.PullRequest.MergedCommit != "" {
		t.Fatalf("the stored batch is %+v (%v), want pull request 7 with no merged commit", loaded.PullRequest, err)
	}

	if _, err := store.ResolveBatch(ctx, "r", "batch-1", "merged-9"); err != nil {
		t.Fatal(err)
	}

	resolved, found, err := batches.Load(ctx, "batch-1")
	if err != nil || !found {
		t.Fatalf("load the resolved batch: %v, %v", found, err)
	}
	if resolved.PullRequest.MergedCommit != "merged-9" {
		t.Fatalf("the decoded merged commit is %q, want the operator's merged-9: the ledger's field name and the program's disagree", resolved.PullRequest.MergedCommit)
	}
	if resolved.PullRequest.Number != 7 || resolved.PullRequest.Head != "candidate-1" || resolved.PullRequest.State != landing.PullRequestMerged {
		t.Fatalf("the resolution lost the rest of the pull request: %+v", resolved.PullRequest)
	}
	if resolved.State != landing.Landed || resolved.Reason != "resolved by operator" {
		t.Fatalf("the resolved batch is %q (%q), want landed by the operator", resolved.State, resolved.Reason)
	}
}
