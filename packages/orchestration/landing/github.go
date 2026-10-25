package landing

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CommandExecutor runs a command in a requested directory.
type CommandExecutor interface {
	Run(context.Context, string, string, ...string) (string, error)
}

// GitHubCLI implements GitHub with git and GitHub CLI commands.
type GitHubCLI struct {
	RepositoryPath string
	Remote         string
	Executable     string
	Executor       CommandExecutor
	PollInterval   time.Duration
}

// Push puts the candidate commit on its queue branch. The push is forced: after a member is dropped, the batch's
// rebuilt candidate replaces the one the branch held.
func (c GitHubCLI) Push(ctx context.Context, _ string, branch, commit string) error {
	remote := c.Remote
	if remote == "" {
		remote = "origin"
	}
	if _, err := c.run(ctx, c.RepositoryPath, "git", "-C", c.RepositoryPath, "push", remote, "+"+commit+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("push candidate branch: %w", err)
	}
	return nil
}

// FindPullRequest finds the pull request whose head is branch.
func (c GitHubCLI) FindPullRequest(ctx context.Context, repository, branch string) (PullRequest, bool, error) {
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "list", "--repo", repository, "--head", branch, "--state", "all", "--limit", "1", "--json", "number,headRefOid,state,mergeStateStatus")
	if err != nil {
		return PullRequest{}, false, fmt.Errorf("find pull request: %w", err)
	}
	var pulls []pullRequestWire
	if err := json.Unmarshal([]byte(output), &pulls); err != nil {
		return PullRequest{}, false, fmt.Errorf("decode pull request list: %w", err)
	}
	if len(pulls) == 0 {
		return PullRequest{}, false, nil
	}
	pull, err := c.enrich(ctx, repository, pulls[0])
	if err != nil {
		return PullRequest{}, false, err
	}
	return pull, true, nil
}

// Open creates one pull request and reads its durable identity back from GitHub.
func (c GitHubCLI) Open(ctx context.Context, repository, base, branch, body string) (PullRequest, error) {
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "create", "--repo", repository, "--base", base, "--head", branch, "--title", "Landing "+branch, "--body", body)
	if err != nil {
		return PullRequest{}, fmt.Errorf("open pull request: %w", err)
	}
	url := strings.TrimSpace(output)
	if url == "" {
		return PullRequest{}, fmt.Errorf("open pull request returned no URL")
	}
	return c.view(ctx, repository, url)
}

// Enqueue enables auto-merge, which queues the pull request on a protected base.
func (c GitHubCLI) Enqueue(ctx context.Context, repository string, number int) error {
	if _, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "merge", fmt.Sprint(number), "--repo", repository, "--auto", "--merge"); err != nil {
		return fmt.Errorf("enqueue pull request: %w", err)
	}
	return nil
}

// Withdraw takes the pull request out of the merge queue, when it is queued, and closes it. A pull request that is
// already closed or merged is left as it is.
func (c GitHubCLI) Withdraw(ctx context.Context, repository string, number int) error {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return err
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){id state mergeQueueEntry{id}}}}`
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "api", "graphql", "-f", "query="+query, "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return fmt.Errorf("read pull request %d before the withdraw: %w", number, err)
	}
	var current struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ID              string `json:"id"`
					State           string `json:"state"`
					MergeQueueEntry *struct {
						ID string `json:"id"`
					} `json:"mergeQueueEntry"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &current); err != nil {
		return fmt.Errorf("decode pull request %d before the withdraw: %w", number, err)
	}
	pull := current.Data.Repository.PullRequest
	switch strings.ToUpper(pull.State) {
	case "MERGED", "CLOSED":
		return nil
	}
	if pull.MergeQueueEntry != nil {
		mutation := `mutation($id:ID!){dequeuePullRequest(input:{id:$id}){clientMutationId}}`
		if _, err := c.run(ctx, c.RepositoryPath, c.gh(), "api", "graphql", "-f", "query="+mutation, "-f", "id="+pull.ID); err != nil {
			return fmt.Errorf("remove pull request %d from the merge queue: %w", number, err)
		}
	}
	if _, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "close", fmt.Sprint(number), "--repo", repository); err != nil {
		return fmt.Errorf("close pull request %d: %w", number, err)
	}
	return nil
}

func splitRepository(repository string) (string, string, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" {
		return "", "", fmt.Errorf("repository %q is not owner/name", repository)
	}
	return owner, name, nil
}

// Wait waits until GitHub merges, closes, or removes the expected head.
func (c GitHubCLI) Wait(ctx context.Context, repository string, number int, expectedHead string) (PullRequest, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		pull, err := c.view(ctx, repository, fmt.Sprint(number))
		if err != nil {
			return PullRequest{}, err
		}
		if pull.State != PullRequestOpen || pull.Head != expectedHead {
			return pull, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return PullRequest{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// Read reads the pull request once, without waiting for an outcome.
func (c GitHubCLI) Read(ctx context.Context, repository string, number int) (PullRequest, error) {
	return c.view(ctx, repository, fmt.Sprint(number))
}

type pullRequestWire struct {
	Number           int    `json:"number"`
	Head             string `json:"headRefOid"`
	State            string `json:"state"`
	MergeStateStatus string `json:"mergeStateStatus"`
}

type pullRequestDetails struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				MergeQueueEntry *struct {
					State string `json:"state"`
				} `json:"mergeQueueEntry"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
				TimelineItems struct {
					Nodes []struct {
						Reason string `json:"reason"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

func (c GitHubCLI) enrich(ctx context.Context, repository string, wire pullRequestWire) (PullRequest, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return PullRequest{}, err
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){mergeQueueEntry{state} mergeCommit{oid} timelineItems(last:1,itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{... on RemovedFromMergeQueueEvent{reason}}}}}}`
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "api", "graphql", "-f", "query="+query, "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", wire.Number))
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request queue state: %w", err)
	}
	var details pullRequestDetails
	if err := json.Unmarshal([]byte(output), &details); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request queue state: %w", err)
	}
	result := PullRequest{Number: wire.Number, Head: wire.Head}
	if details.Data.Repository.PullRequest.MergeCommit != nil {
		result.MergedCommit = details.Data.Repository.PullRequest.MergeCommit.OID
	}
	queueState := ""
	if details.Data.Repository.PullRequest.MergeQueueEntry != nil {
		queueState = details.Data.Repository.PullRequest.MergeQueueEntry.State
	}
	removalReason := ""
	for _, item := range details.Data.Repository.PullRequest.TimelineItems.Nodes {
		if item.Reason != "" {
			removalReason = item.Reason
		}
	}
	switch strings.ToUpper(wire.State) {
	case "MERGED":
		if result.MergedCommit == "" {
			return PullRequest{}, fmt.Errorf("merged pull request %d has no merge commit", wire.Number)
		}
		result.State = PullRequestMerged
	case "CLOSED":
		result.State = PullRequestClosed
	case "OPEN":
		if queueState == "" && removalReason != "" {
			result.State = PullRequestRemoved
			result.Reason = removalReason
		} else {
			result.State = PullRequestOpen
		}
	default:
		result.State = PullRequestRemoved
		result.Reason = strings.ToLower(wire.State)
	}
	return result, nil
}

func (c GitHubCLI) view(ctx context.Context, repository, selector string) (PullRequest, error) {
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "view", selector, "--repo", repository, "--json", "number,headRefOid,state,mergeStateStatus")
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request: %w", err)
	}
	var pull pullRequestWire
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request: %w", err)
	}
	return c.enrich(ctx, repository, pull)
}

func (c GitHubCLI) gh() string {
	if c.Executable != "" {
		return c.Executable
	}
	return "gh"
}

func (c GitHubCLI) run(ctx context.Context, directory, name string, args ...string) (string, error) {
	if c.Executor != nil {
		return c.Executor.Run(ctx, directory, name, args...)
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
