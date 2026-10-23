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
			if next.State == Publishing {
				resumed = append(resumed, next)
				continue
			}
			return resumed, err
		}
		resumed = append(resumed, next)
	}
	return resumed, nil
}

func (r Runner) resume(ctx context.Context, batch Batch) (Batch, error) {
	policyChanged := batch.CheckPolicyDigest != r.ConfigDigest || !sameStringSet(batch.CheckPolicy, r.checkPolicyNames())
	if policyChanged {
		batch.CheckPolicyDigest = r.ConfigDigest
		batch.CheckPolicy = r.checkPolicyNames()
		batch.CheckedTree = ""
		if err := r.Store.Save(ctx, batch); err != nil {
			return Batch{}, fmt.Errorf("record changed check policy: %w", err)
		}
	}
	returned, err := r.recoverReturns(ctx, &batch)
	if err != nil {
		return batch, err
	}
	if batch.State != Frozen && (returned || batch.CandidateDirty) {
		if err := r.rebuild(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "candidate")
		}
		if err := r.collectChanges(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "changes")
		}
		if err := r.revalidateCandidate(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "ledger")
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
		if err := r.revalidateCandidate(ctx, &batch); err != nil {
			return r.fail(ctx, &batch, "ledger")
		}
		return r.drive(ctx, &batch, true)
	case Checked, Building, Blaming:
		currentMembers := memberIDs(batch.ActiveMembers)
		if policyChanged || batch.ChangesTree != batch.Candidate.Tree || !sameStringSet(batch.ChangeMembers, currentMembers) || batch.LedgerTree != batch.Candidate.Tree || !sameStringSet(batch.LedgerMembers, currentMembers) {
			if err := r.collectChanges(ctx, &batch); err != nil {
				return r.fail(ctx, &batch, "changes")
			}
			if err := r.revalidateCandidate(ctx, &batch); err != nil {
				return r.fail(ctx, &batch, "ledger")
			}
			return r.drive(ctx, &batch, true)
		}
		return r.drive(ctx, &batch, policyChanged || batch.CheckedTree != batch.Candidate.Tree || !sameStringSet(batch.CheckMembers, currentMembers))
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

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
