package landing

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// emptySuccessorReason is the reason a successor ends with when none of its declined parent's members is still ready.
const emptySuccessorReason = "no ready members"

// Reconcile resumes every batch that the store lists, from its durable state, in this order:
//
//   - a declined batch whose successor does not exist yet: its successor is created from the parent's members that
//     are still ready, and nothing more is done for it;
//   - an admitted batch in Publishing: it resumes before anything else is built, so a withdraw that a crash
//     interrupted finishes first;
//   - every other batch.
//
// A terminal batch is never advanced or saved again. A batch never reintroduces a returned member: the active member
// set is authoritative, and every derived value whose key differs from the current key is recomputed before its next
// side effect.
func (r Runner) Reconcile(ctx context.Context) ([]Batch, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	batches, err := r.Store.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active batches: %w", err)
	}
	sort.SliceStable(batches, func(i, j int) bool { return resumeRank(batches[i]) < resumeRank(batches[j]) })
	resumed := make([]Batch, 0, len(batches))
	for _, batch := range batches {
		next, err := r.resume(ctx, batch)
		if err != nil {
			if retryable(next, err) {
				resumed = append(resumed, next)
				continue
			}
			return resumed, err
		}
		resumed = append(resumed, next)
	}
	return resumed, nil
}

// retryable reports whether a batch's error leaves it for the next round instead of stopping the runner: a
// publishing step that failed short of a StopError, or an effect of a landed batch that is still to do.
func retryable(batch Batch, err error) bool {
	var stop *StopError
	if errors.As(err, &stop) {
		return false
	}
	return batch.State == Publishing || errors.Is(err, ErrEffectsPending)
}

// resumeRank orders a restart's work: a declined batch's missing successor, then an admitted batch in Publishing,
// then the rest.
func resumeRank(batch Batch) int {
	switch {
	case batch.State == Declined:
		return 0
	case batch.State == Publishing && batch.Admission == Admitted:
		return 1
	default:
		return 2
	}
}

func (r Runner) resume(ctx context.Context, batch Batch) (Batch, error) {
	if batch.State.terminal() {
		// A terminal batch is never advanced or saved again: the store lists it only for an effect still to do or,
		// when declined, for its missing successor.
		if len(batch.PendingEffects) != 0 {
			if err := r.finishEffects(ctx, &batch); err != nil {
				return batch, fmt.Errorf("finish effects of %q: %w", batch.ID, err)
			}
		}
		if batch.State == Declined {
			return r.createSuccessor(ctx, batch)
		}
		return batch, nil
	}
	if err := r.recoverReturns(ctx, &batch); err != nil {
		return batch, err
	}
	switch batch.State {
	case Green, Publishing:
		return r.Publish(ctx, batch)
	default:
		return r.advance(ctx, &batch)
	}
}

// createSuccessor creates the successor of a declined batch, when it does not exist yet, and returns it. The
// successor has the parent's repository, base and base branch, and its members are the parent's members that the
// ledger lists as ready now, in ready order, each frozen at its current ready record. So a member that the decline
// returned joins it only after its owner readies it again. When no member is still ready, the successor is created
// straight in the terminal Empty state, so the parent stops being listed and the next ready migration starts a new
// batch. The parent is never saved again, and the successor is neither built nor checked here.
func (r Runner) createSuccessor(ctx context.Context, parent Batch) (Batch, error) {
	id := parent.Successor
	if id == "" {
		id = successorID(parent.ID)
	}
	if existing, found, err := r.Store.Load(ctx, id); err != nil {
		return parent, fmt.Errorf("load successor %q of %q: %w", id, parent.ID, err)
	} else if found {
		return existing, nil
	}
	if r.Admission == nil {
		return parent, fmt.Errorf("create successor of %q: batch ledger is required", parent.ID)
	}
	ready, err := r.Admission.ReadyMembers(ctx, parent.Repository)
	if err != nil {
		return parent, fmt.Errorf("list ready members for successor of %q: %w", parent.ID, err)
	}
	members := make([]Member, 0, len(parent.FrozenMembers))
	for _, member := range ready {
		if _, found := findMember(parent.FrozenMembers, member.Migration); found {
			members = append(members, member)
		}
	}
	successor := Batch{
		ID:                id,
		Repository:        parent.Repository,
		Base:              parent.Base,
		BaseBranch:        parent.BaseBranch,
		Operations:        make(map[string]Operation),
		CheckPolicyDigest: r.policyDigest(),
	}
	if len(members) == 0 {
		successor.State, successor.Reason = Empty, emptySuccessorReason
	} else {
		input, err := r.freezeInput(ctx, Input{ID: id, Repository: parent.Repository, Base: parent.Base, BaseBranch: parent.BaseBranch, Members: members})
		if err != nil {
			return parent, fmt.Errorf("freeze successor %q: %w", id, err)
		}
		if err := r.readChanges(ctx, input.Repository, input.Base, input.Members); err != nil {
			return parent, err
		}
		successor.Base = input.Base
		successor.FrozenMembers = cloneMembers(input.Members)
		successor.ActiveMembers = cloneMembers(input.Members)
		successor.State = Frozen
		candidate, err := r.candidate(ctx, successor.Repository, successor.Base, successor.ActiveMembers)
		if err != nil {
			return parent, fmt.Errorf("freeze candidate of successor %q: %w", id, err)
		}
		r.setCandidate(&successor, candidate)
		successor.InitialCandidate = candidate
	}
	created, err := r.Store.Create(ctx, &successor)
	if err != nil {
		return parent, fmt.Errorf("create successor %q of %q: %w", id, parent.ID, err)
	}
	if !created {
		existing, found, err := r.Store.Load(ctx, id)
		if err != nil || !found {
			return parent, fmt.Errorf("load raced successor %q of %q: found=%v: %w", id, parent.ID, found, err)
		}
		return existing, nil
	}
	return successor, nil
}

// recoverReturns finishes each return whose intent was recorded but whose
// ledger call was not. The member already left the active set in the same
// save as the intent. The round's key replays a round the ledger already
// recorded, so the return is sent again whether or not the first one arrived.
func (r Runner) recoverReturns(ctx context.Context, batch *Batch) error {
	for key, operation := range batch.Operations {
		if operation.Kind != "return" || operation.State != OperationPending {
			continue
		}
		member, found := findMember(batch.FrozenMembers, operation.Member)
		if !found {
			return fmt.Errorf("return operation %q has no frozen member", key)
		}
		if err := r.Ledger.ReturnMember(ctx, roundRequest(operation, member)); err != nil {
			return fmt.Errorf("complete return %q: %w", operation.Member, err)
		}
		batch.completeOperation(key)
		if err := r.Store.Save(ctx, batch); err != nil {
			return fmt.Errorf("record completed return %q: %w", operation.Member, err)
		}
	}
	return nil
}
