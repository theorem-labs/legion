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
		return r.drive(ctx, &batch, false)
	case Green, Publishing:
		return r.Publish(ctx, batch)
	default:
		return batch, nil
	}
}
