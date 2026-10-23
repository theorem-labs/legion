package landing

import (
	"context"
	"fmt"
)

// Reconcile resumes every non-terminal batch from its durable state. A batch
// resumes its frozen candidate and never reintroduces a returned member.
func (r Runner) Reconcile(ctx context.Context) ([]Batch, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	batches, err := r.Store.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active batches: %w", err)
	}
	resumed := make([]Batch, 0, len(batches))
	for _, batch := range batches {
		next, err := r.resume(ctx, batch)
		if err != nil {
			return resumed, err
		}
		resumed = append(resumed, next)
	}
	return resumed, nil
}

func (r Runner) resume(ctx context.Context, batch Batch) (Batch, error) {
	returned, err := r.recoverReturns(ctx, &batch)
	if err != nil {
		return batch, err
	}
	if returned {
		if err := r.rebuild(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "candidate")
		}
		return r.drive(ctx, &batch, true)
	}
	switch batch.State {
	case Frozen:
		if err := r.applyCandidateDrops(ctx, &batch); err != nil {
			return Batch{}, err
		}
		if err := r.collectChanges(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "changes")
		}
		if err := r.checkLedger(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "ledger")
		}
		if err := r.rebuild(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "candidate")
		}
		return r.drive(ctx, &batch, true)
	case Checked, Building, Blaming:
		return r.drive(ctx, &batch, batch.CheckedTree != batch.Candidate.Tree)
	case Green, Publishing:
		return r.Publish(ctx, batch)
	default:
		return batch, nil
	}
}

func (r Runner) recoverReturns(ctx context.Context, batch *Batch) (bool, error) {
	recovered := false
	for key, operation := range batch.Operations {
		if operation.Kind != "return" || operation.State != OperationPending {
			continue
		}
		member, found := findMember(batch.FrozenMembers, operation.Member)
		if !found {
			return false, fmt.Errorf("return operation %q has no frozen member", key)
		}
		if status, supported := r.Ledger.(OperationStatus); supported {
			done, err := status.OperationComplete(ctx, operation)
			if err != nil {
				return false, fmt.Errorf("check return %q: %w", operation.Member, err)
			}
			if done {
				batch.completeOperation(key)
				if err := r.Store.Save(ctx, *batch); err != nil {
					return false, fmt.Errorf("record observed return %q: %w", operation.Member, err)
				}
				recovered = true
				continue
			}
		}
		if err := r.Ledger.ReturnMember(ctx, batch.Repository, member.Migration, member.Generation, operation.Reason); err != nil {
			return false, fmt.Errorf("complete return %q: %w", operation.Member, err)
		}
		batch.completeOperation(key)
		if err := r.Store.Save(ctx, *batch); err != nil {
			return false, fmt.Errorf("record completed return %q: %w", operation.Member, err)
		}
		recovered = true
	}
	return recovered, nil
}
