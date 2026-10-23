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
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "list", "--repo", repository, "--head", branch, "--state", "all", "--limit", "1", "--json", "number,headRefOid,state,mergeStateStatus,isInMergeQueue")
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
	return pulls[0].pullRequest(), true, nil
}

// Open creates one pull request and reads its durable identity back from GitHub.
func (c GitHubCLI) Open(ctx context.Context, repository, base, branch, body string) (PullRequest, error) {
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "create", "--repo", repository, "--base", base, "--head", branch, "--body", body)
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
	InMergeQueue     bool   `json:"isInMergeQueue"`
}

func (p pullRequestWire) pullRequest() PullRequest {
	result := PullRequest{Number: p.Number, Head: p.Head}
	switch strings.ToUpper(p.State) {
	case "MERGED":
		result.State = PullRequestMerged
	case "CLOSED":
		result.State = PullRequestClosed
	case "OPEN":
		if strings.EqualFold(p.MergeStateStatus, "DIRTY") && !p.InMergeQueue {
			result.State = PullRequestRemoved
			result.Reason = "checks failed"
		} else {
			result.State = PullRequestOpen
		}
	default:
		result.State = PullRequestRemoved
		result.Reason = strings.ToLower(p.State)
	}
	return result
}

func (c GitHubCLI) view(ctx context.Context, repository, selector string) (PullRequest, error) {
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "pr", "view", selector, "--repo", repository, "--json", "number,headRefOid,state,mergeStateStatus,isInMergeQueue")
	if err != nil {
		return PullRequest{}, fmt.Errorf("read pull request: %w", err)
	}
	var pull pullRequestWire
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request: %w", err)
	}
	return pull.pullRequest(), nil
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
