package landing

import (
	"context"
	"fmt"
	"strings"
)

// PullRequestState is GitHub's final or in-progress view of a pull request.
type PullRequestState string

const (
	PullRequestOpen    PullRequestState = "open"
	PullRequestMerged  PullRequestState = "merged"
	PullRequestRemoved PullRequestState = "removed"
	PullRequestClosed  PullRequestState = "closed"
)

// PullRequest identifies the candidate pull request and its observed outcome.
type PullRequest struct {
	Number       int
	Head         string
	MergedCommit string
	State        PullRequestState
	Reason       string
}

// GitHub is the small merge-queue surface the publisher needs.
type GitHub interface {
	Push(context.Context, string, string, string) error
	FindPullRequest(context.Context, string, string) (PullRequest, bool, error)
	Open(context.Context, string, string, string, string) (PullRequest, error)
	Enqueue(context.Context, string, int) error
	Wait(context.Context, string, int, string) (PullRequest, error)
}

// Scorer records the configured post-merge score and commits its output through
// the publishing policy.
type Scorer interface {
	Score(context.Context, string, string) (string, error)
	Commit(context.Context, string, string, string, string) error
}

// LandingEvent is emitted once per member after its merge is durably recorded.
// Flags are the member's flags from the receipt's check verdicts.
type LandingEvent struct {
	Batch  string
	Member string
	Commit string
	Flags  []Flag
}

// EventPublisher publishes post-landing member events.
type EventPublisher interface {
	Publish(context.Context, LandingEvent) error
}

// Publish puts a green candidate through the merge queue and records its members
// only after GitHub confirms that exact head merged. Until its pull request is
// recorded, a batch whose derived values are not at the current key (a new
// checking policy) is advanced again before anything is pushed. Once the pull
// request is recorded, the batch has been published to the merge queue, which
// may merge it at any moment, so it waits for the queue's outcome: failing it
// would leave the queued pull request to merge without a ledger record. A merged
// batch completes under the receipt, and so the policy, that its merge
// validated.
func (r Runner) Publish(ctx context.Context, batch Batch) (Batch, error) {
	if r.Store == nil || r.Ledger == nil || r.GitHub == nil {
		return Batch{}, fmt.Errorf("store, ledger, and GitHub are required to publish")
	}
	if batch.State != Green && batch.State != Publishing {
		return Batch{}, fmt.Errorf("batch %q is %q, not publishable", batch.ID, batch.State)
	}
	if batch.Receipt == nil {
		return Batch{}, fmt.Errorf("batch %q has no receipt", batch.ID)
	}
	if batch.BaseBranch == "" {
		return Batch{}, fmt.Errorf("batch %q has no base branch", batch.ID)
	}
	if batch.MergeValidated {
		return r.completeMerged(ctx, &batch)
	}
	if batch.PullRequest.Number == 0 && !r.fresh(batch) {
		if err := r.valid(); err != nil {
			return Batch{}, err
		}
		advanced, err := r.advance(ctx, &batch)
		if err != nil || advanced.State != Green {
			return advanced, err
		}
		batch = advanced
	}
	if batch.Branch == "" {
		batch.Branch = "land/" + batch.ID
	}
	if batch.State != Publishing {
		if err := r.transition(ctx, &batch, Publishing, ""); err != nil {
			return Batch{}, err
		}
	}
	if err := r.GitHub.Push(ctx, batch.Repository, batch.Branch, batch.Candidate.Commit); err != nil {
		return r.fail(ctx, &batch, "github: push")
	}
	pull, found, err := r.GitHub.FindPullRequest(ctx, batch.Repository, batch.Branch)
	if err != nil {
		return r.fail(ctx, &batch, "github: find")
	}
	if !found {
		body, err := batch.Receipt.JSON()
		if err != nil {
			return r.fail(ctx, &batch, "receipt")
		}
		pull, err = r.GitHub.Open(ctx, batch.Repository, batch.BaseBranch, batch.Branch, string(body))
		if err != nil {
			return r.fail(ctx, &batch, "github: open")
		}
	}
	batch.PullRequest = pull
	if err := r.Store.Save(ctx, batch); err != nil {
		return Batch{}, fmt.Errorf("record pull request: %w", err)
	}
	if err := r.GitHub.Enqueue(ctx, batch.Repository, pull.Number); err != nil {
		return r.fail(ctx, &batch, "github: enqueue")
	}
	pull, err = r.GitHub.Wait(ctx, batch.Repository, pull.Number, batch.Candidate.Commit)
	if err != nil {
		return r.fail(ctx, &batch, "github: wait")
	}
	return r.recordMerge(ctx, &batch, pull)
}

// recordMerge records GitHub's final view of the batch's pull request. Only a
// merge of the exact candidate commit validates the batch and completes it.
func (r Runner) recordMerge(ctx context.Context, batch *Batch, pull PullRequest) (Batch, error) {
	batch.PullRequest = pull
	batch.MergeValidated = pull.State == PullRequestMerged && pull.Head == batch.Candidate.Commit && pull.MergedCommit != ""
	if err := r.Store.Save(ctx, *batch); err != nil {
		return Batch{}, fmt.Errorf("record merged pull request: %w", err)
	}
	if !batch.MergeValidated {
		reason := strings.TrimSpace(pull.Reason)
		if reason == "" {
			reason = string(pull.State)
		}
		if pull.State == PullRequestMerged && pull.MergedCommit == "" {
			reason = "merged commit unavailable"
		}
		if pull.State == PullRequestMerged && pull.Head != batch.Candidate.Commit {
			reason = "merged unexpected head"
		}
		return r.fail(ctx, batch, "github: "+reason)
	}
	return r.completeMerged(ctx, batch)
}

// completeMerged records a merged batch's members, score and events. It never
// rechecks: the merge is irreversible, and the receipt holds the key, and so the
// policy, that validated it.
func (r Runner) completeMerged(ctx context.Context, batch *Batch) (Batch, error) {
	mergedCommit := batch.PullRequest.MergedCommit
	for _, member := range batch.ActiveMembers {
		if contains(batchMemberNames(batch.LandedMembers), member.Migration) {
			continue
		}
		if member.LedgerKey != batch.Receipt.Key || member.HashesKey != batch.Receipt.Key {
			return r.retryable(ctx, batch, "ledger decision stale", fmt.Errorf("member %q was checked at key %q, the receipt is at %q", member.Migration, member.LedgerKey, batch.Receipt.Key))
		}
		// Land sends the full check's request, with the commit that landed.
		request := landRequest(batch.Repository, member, mergedCommit, member.CurrentHashes, member.DerivedContracts)
		key := "land/" + member.Migration
		operation, found := batch.operation(key)
		if !found {
			operation = Operation{Key: key, Kind: "land", Batch: batch.ID, Repository: batch.Repository, Member: member.Migration, Generation: member.Generation, Commit: mergedCommit}
			batch.startOperation(operation)
			if err := r.Store.Save(ctx, *batch); err != nil {
				return Batch{}, fmt.Errorf("record land intent for %q: %w", member.Migration, err)
			}
		}
		if operation.State != OperationComplete {
			state, landedCommit, err := r.Ledger.MigrationState(ctx, batch.Repository, member.Migration)
			if err != nil {
				return r.retryable(ctx, batch, "ledger migration state", err)
			}
			if state == "landed" {
				if landedCommit != mergedCommit {
					return r.retryable(ctx, batch, "ledger landed commit mismatch", fmt.Errorf("migration %q landed at %q, want %q", member.Migration, landedCommit, mergedCommit))
				}
				batch.completeOperation(key)
				if err := r.Store.Save(ctx, *batch); err != nil {
					return Batch{}, fmt.Errorf("record observed land %q: %w", member.Migration, err)
				}
			} else {
				if err := r.Ledger.Land(ctx, request); err != nil {
					return r.retryable(ctx, batch, "ledger: land", err)
				}
				batch.completeOperation(key)
				if err := r.Store.Save(ctx, *batch); err != nil {
					return Batch{}, fmt.Errorf("record landed operation %q: %w", member.Migration, err)
				}
			}
		}
		batch.LandedMembers = append(batch.LandedMembers, member)
		if err := r.Store.Save(ctx, *batch); err != nil {
			return Batch{}, fmt.Errorf("record landed member %q: %w", member.Migration, err)
		}
	}
	if r.Score != nil && batch.Score.Verdict != "pass" {
		key := "score"
		operation, found := batch.operation(key)
		if !found {
			operation = Operation{Key: key, Kind: "score", Batch: batch.ID, Repository: batch.Repository, Commit: mergedCommit}
			batch.startOperation(operation)
			if err := r.Store.Save(ctx, *batch); err != nil {
				return Batch{}, fmt.Errorf("record score intent: %w", err)
			}
		}
		done, err := operationComplete(ctx, r.Score, operation)
		if err != nil {
			batch.Score = CheckVerdict{Name: "score", Verdict: "unable", Reason: err.Error()}
			return r.retryable(ctx, batch, "score operation status", err)
		}
		if !done {
			output, err := r.Score.Score(ctx, mergedCommit, batch.ID)
			if err != nil {
				batch.Score = CheckVerdict{Name: "score", Verdict: "unable", Reason: err.Error()}
				return r.retryable(ctx, batch, "score", err)
			}
			if err := r.Score.Commit(ctx, batch.Repository, mergedCommit, r.ScorePath, output); err != nil {
				batch.Score = CheckVerdict{Name: "score", Verdict: "unable", Reason: err.Error()}
				return r.retryable(ctx, batch, "score commit", err)
			}
		}
		batch.completeOperation(key)
		batch.Score = CheckVerdict{Name: "score", Verdict: "pass"}
		if err := r.Store.Save(ctx, *batch); err != nil {
			return Batch{}, fmt.Errorf("record score: %w", err)
		}
	}
	if r.Events != nil {
		for _, member := range batch.ActiveMembers {
			if contains(batch.NotifiedMembers, member.Migration) {
				continue
			}
			key := "event/" + member.Migration
			operation, found := batch.operation(key)
			if !found {
				operation = Operation{Key: key, Kind: "event", Batch: batch.ID, Repository: batch.Repository, Member: member.Migration, Generation: member.Generation, Commit: mergedCommit}
				batch.startOperation(operation)
				if err := r.Store.Save(ctx, *batch); err != nil {
					return Batch{}, fmt.Errorf("record event intent for %q: %w", member.Migration, err)
				}
			}
			done, err := operationComplete(ctx, r.Events, operation)
			if err != nil {
				return r.retryable(ctx, batch, "event operation status", err)
			}
			if !done {
				if err := r.Events.Publish(ctx, LandingEvent{Batch: batch.ID, Member: member.Migration, Commit: mergedCommit, Flags: memberFlags(batch.Receipt, member.Migration)}); err != nil {
					return r.retryable(ctx, batch, "event", err)
				}
			}
			batch.completeOperation(key)
			batch.NotifiedMembers = append(batch.NotifiedMembers, member.Migration)
			if err := r.Store.Save(ctx, *batch); err != nil {
				return Batch{}, fmt.Errorf("record member event %q: %w", member.Migration, err)
			}
		}
	}
	if err := r.transition(ctx, batch, Landed, ""); err != nil {
		return Batch{}, err
	}
	return *batch, nil
}

// memberFlags is a member's flags in the receipt's check verdicts, never nil.
func memberFlags(receipt *Receipt, member string) []Flag {
	flags := []Flag{}
	for _, verdict := range receipt.Checks {
		for _, flag := range verdict.Flags {
			if flag.Member == member {
				flags = append(flags, flag)
			}
		}
	}
	return flags
}

func (r Runner) retryable(ctx context.Context, batch *Batch, reason string, cause error) (Batch, error) {
	batch.State = Publishing
	batch.Reason = reason
	if err := r.Store.Save(ctx, *batch); err != nil {
		return Batch{}, fmt.Errorf("record retryable %s: %w", reason, err)
	}
	return *batch, fmt.Errorf("%s: %w", reason, cause)
}

func operationComplete(ctx context.Context, dependency any, operation Operation) (bool, error) {
	if operation.State == OperationComplete {
		return true, nil
	}
	status, supported := dependency.(OperationStatus)
	if !supported {
		return false, nil
	}
	return status.OperationComplete(ctx, operation)
}

func batchMemberNames(members []Member) []string {
	names := make([]string, len(members))
	for index, member := range members {
		names[index] = member.Migration
	}
	return names
}
