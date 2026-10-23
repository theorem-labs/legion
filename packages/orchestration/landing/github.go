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

// Push pushes only the candidate commit to its queue branch.
func (c GitHubCLI) Push(ctx context.Context, _ string, branch, commit string) error {
	remote := c.Remote
	if remote == "" {
		remote = "origin"
	}
	if _, err := c.run(ctx, c.RepositoryPath, "git", "-C", c.RepositoryPath, "push", remote, commit+":refs/heads/"+branch); err != nil {
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
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

func (c GitHubCLI) enrich(ctx context.Context, repository string, wire pullRequestWire) (PullRequest, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" {
		return PullRequest{}, fmt.Errorf("repository %q is not owner/name", repository)
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){mergeQueueEntry{state} mergeCommit{oid}}}}`
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
	switch strings.ToUpper(wire.State) {
	case "MERGED":
		if result.MergedCommit == "" {
			return PullRequest{}, fmt.Errorf("merged pull request %d has no merge commit", wire.Number)
		}
		result.State = PullRequestMerged
	case "CLOSED":
		result.State = PullRequestClosed
	case "OPEN":
		if queueState == "FAILED" || (queueState == "" && strings.EqualFold(wire.MergeStateStatus, "DIRTY")) {
			result.State = PullRequestRemoved
			result.Reason = "checks failed"
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
