package landing

import (
	"context"
	"testing"
	"time"
)

// Push forces the queue branch to the candidate, because a rebuilt candidate replaces the one the branch held.
func TestGitHubCLIPushesCandidateBranchOnly(t *testing.T) {
	executor := &commandExecutorFake{}
	client := GitHubCLI{RepositoryPath: "/repo", Remote: "origin", Executor: executor}

	if err := client.Push(context.Background(), "owner/repo", "land/batch", "candidate"); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if got, want := executor.calls[0].name, "git"; got != want {
		t.Fatalf("program = %q, want %q", got, want)
	}
	if got, want := executor.calls[0].args, []string{"-C", "/repo", "push", "origin", "+candidate:refs/heads/land/batch"}; !sameStrings(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestGitHubCLIFindsQueueRemoval(t *testing.T) {
	executor := &commandExecutorFake{outputs: []string{
		`[{"number":7,"headRefOid":"candidate","state":"OPEN","mergeStateStatus":"UNSTABLE"}]`,
		`{"data":{"repository":{"pullRequest":{"mergeQueueEntry":null,"mergeCommit":null,"timelineItems":{"nodes":[{"reason":"checks failed"}]}}}}}`,
	}}
	client := GitHubCLI{Executor: executor}

	pull, found, err := client.FindPullRequest(context.Background(), "owner/repo", "land/batch")
	if err != nil {
		t.Fatalf("FindPullRequest() error = %v", err)
	}
	if !found {
		t.Fatal("FindPullRequest() found = false, want true")
	}
	if got, want := pull.State, PullRequestRemoved; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}

func TestGitHubCLIWaitsForMergedHead(t *testing.T) {
	executor := &commandExecutorFake{outputs: []string{
		`{"number":7,"headRefOid":"candidate","state":"MERGED","mergeStateStatus":"CLEAN"}`,
		`{"data":{"repository":{"pullRequest":{"mergeQueueEntry":null,"mergeCommit":{"oid":"merged"}}}}}`,
	}}
	client := GitHubCLI{Executor: executor, PollInterval: time.Nanosecond}

	pull, err := client.Wait(context.Background(), "owner/repo", 7, "candidate")
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got, want := pull.State, PullRequestMerged; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}

type commandCall struct {
	directory string
	name      string
	args      []string
}

type commandExecutorFake struct {
	calls   []commandCall
	outputs []string
}

func (f *commandExecutorFake) Run(_ context.Context, directory, name string, args ...string) (string, error) {
	f.calls = append(f.calls, commandCall{directory: directory, name: name, args: append([]string(nil), args...)})
	if len(f.outputs) == 0 {
		return "", nil
	}
	output := f.outputs[0]
	f.outputs = f.outputs[1:]
	return output, nil
}
