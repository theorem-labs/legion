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

// MergeGroupContext is the commit status that the merge-queue ruleset requires on each merge group's head commit.
const MergeGroupContext = "landing/merge-group"

// Commit status states that the program posts.
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// MergeGroup is one entry of a merge queue: the pull request it merges and the head commit, with its tree, that the
// queue tests and merges. The queue's group size is 1, so a group holds exactly one pull request.
type MergeGroup struct {
	PullRequest int
	Commit      string
	Tree        string
}

// MergeQueue is the merge-queue surface that the merge-group status needs: the groups queued on a base branch, and
// the commit status that the ruleset requires on each group's head.
type MergeQueue interface {
	MergeGroups(ctx context.Context, repository, branch string) ([]MergeGroup, error)
	SetStatus(ctx context.Context, repository, commit, context, state, description string) error
}

// MergeGroups lists the merge queue's entries on branch that have a head commit.
func (c GitHubCLI) MergeGroups(ctx context.Context, repository, branch string) ([]MergeGroup, error) {
	owner, name, err := splitRepository(repository)
	if err != nil {
		return nil, err
	}
	query := `query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){mergeQueue(branch:$branch){entries(first:100){nodes{headCommit{oid tree{oid}} pullRequest{number}}}}}}`
	output, err := c.run(ctx, c.RepositoryPath, c.gh(), "api", "graphql", "-f", "query="+query, "-F", "owner="+owner, "-F", "name="+name, "-f", "branch="+branch)
	if err != nil {
		return nil, fmt.Errorf("read merge queue of %s: %w", branch, err)
	}
	var queue struct {
		Data struct {
			Repository struct {
				MergeQueue *struct {
					Entries struct {
						Nodes []struct {
							HeadCommit *struct {
								OID  string `json:"oid"`
								Tree struct {
									OID string `json:"oid"`
								} `json:"tree"`
							} `json:"headCommit"`
							PullRequest *struct {
								Number int `json:"number"`
							} `json:"pullRequest"`
						} `json:"nodes"`
					} `json:"entries"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &queue); err != nil {
		return nil, fmt.Errorf("decode merge queue of %s: %w", branch, err)
	}
	if queue.Data.Repository.MergeQueue == nil {
		return nil, nil
	}
	var groups []MergeGroup
	for _, entry := range queue.Data.Repository.MergeQueue.Entries.Nodes {
		if entry.HeadCommit == nil || entry.HeadCommit.OID == "" {
			continue
		}
		group := MergeGroup{Commit: entry.HeadCommit.OID, Tree: entry.HeadCommit.Tree.OID}
		if entry.PullRequest != nil {
			group.PullRequest = entry.PullRequest.Number
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// SetStatus posts a commit status on commit.
func (c GitHubCLI) SetStatus(ctx context.Context, repository, commit, context, state, description string) error {
	if _, err := c.run(ctx, c.RepositoryPath, c.gh(), "api", "--method", "POST", "repos/"+repository+"/statuses/"+commit,
		"-f", "state="+state, "-f", "context="+context, "-f", "description="+description); err != nil {
		return fmt.Errorf("set status %s on %s: %w", context, commit, err)
	}
	return nil
}

// MergeGroupStatus decides the merge-group status of group: success only when its pull request belongs to a batch
// that the ledger shows admitted and the group's head tree is that batch's admitted candidate tree, and failure for
// every other group, so a pull request that is queued by hand never merges without an admission.
func (r Runner) MergeGroupStatus(ctx context.Context, repository string, group MergeGroup) (state, description string, err error) {
	batches, err := r.Store.ListActive(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list batches for merge group %s: %w", group.Commit, err)
	}
	for _, batch := range batches {
		if group.PullRequest == 0 || batch.Repository != repository || batch.PullRequest.Number != group.PullRequest {
			continue
		}
		switch {
		case batch.Admission != Admitted:
			return StatusFailure, fmt.Sprintf("batch %s is not admitted", batch.ID), nil
		case group.Tree == "" || group.Tree != batch.Candidate.Tree:
			return StatusFailure, fmt.Sprintf("tree %s is not the admitted candidate tree %s", shortID(group.Tree), shortID(batch.Candidate.Tree)), nil
		default:
			return StatusSuccess, fmt.Sprintf("the admitted candidate tree of batch %s", batch.ID), nil
		}
	}
	return StatusFailure, fmt.Sprintf("no landing batch holds pull request #%d", group.PullRequest), nil
}

// AttestMergeGroups sets the merge-group status on every merge group queued on branch. posted holds the state each
// group's head commit last received, so a status is posted again only when it changes.
func (r Runner) AttestMergeGroups(ctx context.Context, queue MergeQueue, repository, branch string, posted map[string]string) error {
	groups, err := queue.MergeGroups(ctx, repository, branch)
	if err != nil {
		return err
	}
	for _, group := range groups {
		state, description, err := r.MergeGroupStatus(ctx, repository, group)
		if err != nil {
			return err
		}
		if posted != nil && posted[group.Commit] == state {
			continue
		}
		if err := queue.SetStatus(ctx, repository, group.Commit, MergeGroupContext, state, description); err != nil {
			return err
		}
		if posted != nil {
			posted[group.Commit] = state
		}
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
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
