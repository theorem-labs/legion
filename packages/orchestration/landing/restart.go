package landing

import (
	"context"
	"fmt"
)

// Reconcile resumes every non-terminal batch from its durable state. A batch
// never reintroduces a returned member: the active member set is authoritative,
// and every derived value whose key differs from the current key is
// recomputed before its next side effect.
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
	if err := r.recoverReturns(ctx, &batch); err != nil {
		return batch, err
	}
	switch batch.State {
	case Green, Publishing:
		return r.Publish(ctx, batch)
	case Failed, Landed:
		return batch, nil
	default:
		return r.advance(ctx, &batch)
	}
}

// recoverReturns finishes each return whose intent was recorded but whose
// ledger call was not. The member already left the active set in the same
// save as the intent.
func (r Runner) recoverReturns(ctx context.Context, batch *Batch) error {
	for key, operation := range batch.Operations {
		if operation.Kind != "return" || operation.State != OperationPending {
			continue
		}
		member, found := findMember(batch.FrozenMembers, operation.Member)
		if !found {
			return fmt.Errorf("return operation %q has no frozen member", key)
		}
		done, err := operationComplete(ctx, r.Ledger, operation)
		if err != nil {
			return fmt.Errorf("check return %q: %w", operation.Member, err)
		}
		if !done {
			if err := r.Ledger.ReturnMember(ctx, batch.Repository, member.Migration, member.Generation, operation.Reason); err != nil {
				return fmt.Errorf("complete return %q: %w", operation.Member, err)
			}
		}
		batch.completeOperation(key)
		if err := r.Store.Save(ctx, batch); err != nil {
			return fmt.Errorf("record completed return %q: %w", operation.Member, err)
		}
	}
	return nil
}
