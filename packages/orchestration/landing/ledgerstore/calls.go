package ledgerstore

import (
	"context"
	"errors"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// CheckRequest is the ledger's request for the program's check request: the same fields, by the same JSON names.
func CheckRequest(request landing.CheckRequest) ledger.CheckRequest {
	return ledger.CheckRequest{LandRequest: landRequest(request.LandRequest), DeferReads: request.DeferReads}
}

func landRequest(request landing.LandRequest) ledger.LandRequest {
	reviews := make([]ledger.Review, 0, len(request.Reviews))
	for _, review := range request.Reviews {
		reviews = append(reviews, ledger.Review{Contract: review.Contract, Reviewer: review.Reviewer, Head: review.Head})
	}
	if request.Reviews == nil {
		reviews = nil
	}
	return ledger.LandRequest{
		Repo:         request.Repo,
		Migration:    request.Migration,
		Generation:   request.Generation,
		Commit:       request.Commit,
		ChangedPaths: request.ChangedPaths,
		Current:      request.Current,
		StatementRecords: ledger.StatementRecords{
			ChangedContracts:   request.ChangedContracts,
			Head:               request.Head,
			CompatibilityProof: request.CompatibilityProof,
			ReviewedHead:       request.ReviewedHead,
			Reviews:            reviews,
			ApprovedPinned:     request.ApprovedPinned,
			Upstream:           request.Upstream,
		},
	}
}

// Check runs the ledger's check, deferred or full, and gives its decision: pass, incomplete (a deferred check that
// found nothing), or refused with each violation.
func (l *Ledger) Check(ctx context.Context, request landing.CheckRequest) (landing.CheckResult, error) {
	result, err := l.ledger.Check(ctx, CheckRequest(request))
	if err != nil {
		return landing.CheckResult{}, refusal(err)
	}
	decision := landing.CheckResult{Outcome: result.Outcome, Violations: make([]landing.Violation, 0, len(result.Violations))}
	for _, violation := range result.Violations {
		decision.Violations = append(decision.Violations, landing.Violation{
			Kind: violation.Kind, Name: violation.Name, Path: violation.Path, Message: violation.Message,
		})
	}
	return decision, nil
}

// ReturnMember records the member's failed landing round. Its key replays a retry, and its ready event makes the
// round clear only the ready record that the batch read.
func (l *Ledger) ReturnMember(ctx context.Context, round landing.RoundRequest) error {
	_, err := l.ledger.RecordRound(ctx, ledger.RoundRequest{
		Repo: round.Repo, ID: round.ID, Generation: round.Generation, Reason: round.Reason, Batch: round.Batch,
		Key: round.Key, ReadyEvent: round.ReadyEvent, Detail: round.Detail,
	})
	if err != nil {
		return refusal(err)
	}
	return nil
}

// Contracts lists the repository's registered contracts with their statement hashes, in name order.
func (l *Ledger) Contracts(ctx context.Context, repo string) ([]landing.Contract, error) {
	stored, err := l.ledger.Contracts(ctx, repo)
	if err != nil {
		return nil, err
	}
	contracts := make([]landing.Contract, 0, len(stored))
	for _, contract := range stored {
		contracts = append(contracts, landing.Contract{Name: contract.Name, StatementHash: contract.StatementHash})
	}
	return contracts, nil
}

// Escalate enqueues item for owner. A repeat for an item already open for that owner does nothing.
func (l *Ledger) Escalate(ctx context.Context, repo, item, owner string) error {
	if err := l.ledger.Enqueue(ctx, repo, item, owner); err != nil {
		var refused *ledger.Refusal
		if errors.As(err, &refused) {
			return refusal(err)
		}
		return err
	}
	return nil
}
