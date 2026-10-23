package landing

import (
	"context"
	"strings"
	"testing"
)

func TestReviewGitHubCLIUsesGraphQLForQueueAndMergedCommit(t *testing.T) {
	executor := &commandExecutorFake{outputs: []string{
		`[{"number":7,"headRefOid":"candidate","state":"MERGED","mergeStateStatus":"CLEAN"}]`,
		`{"data":{"repository":{"pullRequest":{"mergeQueueEntry":null,"mergeCommit":{"oid":"merged"}}}}}`,
	}}
	client := GitHubCLI{Executor: executor}

	pull, found, err := client.FindPullRequest(context.Background(), "owner/repo", "land/batch")
	if err != nil {
		t.Fatalf("FindPullRequest() error = %v", err)
	}
	if !found || pull.MergedCommit != "merged" {
		t.Fatalf("pull = %#v found=%v, want merged commit", pull, found)
	}
	for _, argument := range executor.calls[0].args {
		if strings.Contains(argument, "isInMergeQueue") {
			t.Fatalf("unsupported field requested in %#v", executor.calls[0].args)
		}
	}
}
