package landing

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrAdmissionConflict reports that the ledger refused a drop because another runner admitted or changed the batch,
// or another admitted batch holds one of its members. Nothing was dropped: the caller reloads the batch and continues
// from the stored state.
var ErrAdmissionConflict = errors.New("admission conflict")

// declineReason is the reason an admitted batch records when it is declined because members were dropped.
const declineReason = "member_dropped"

// successorID is the id of the batch that the members of a declined batch go to. It depends only on the batch id, so
// a retry names the same successor.
func successorID(batch string) string { return batch + "-next" }

// reloaded reports whether err is an admission conflict, and then replaces the batch with its stored state, so the
// runner continues from what the ledger holds. A failed reload keeps the batch as it was and reports false.
func (r Runner) reloaded(ctx context.Context, batch *Batch, err error) bool {
	if !errors.Is(err, ErrAdmissionConflict) {
		return false
	}
	stored, found, loadErr := r.Store.Load(ctx, batch.ID)
	if loadErr != nil || !found {
		return false
	}
	*batch = stored
	return true
}

// dropMembers removes members (migration to reason) from the batch's active set. It is the only code that does. The
// branch comes from the ledger's admission of the batch, read from the store:
//
//   - not admitted: each member's removal and the intent to return it are saved together before the ledger hears of
//     it, then the ledger returns it. When the last active member goes, the batch ends Empty.
//   - admitted: one DeclineBatch returns the dropped members and ends the batch Declined, naming the successor that
//     the other members go to. The runner never saves the batch again.
//
// An admission conflict leaves the stored batch as it was and returns ErrAdmissionConflict.
func (r Runner) dropMembers(ctx context.Context, batch *Batch, drops map[string]string) error {
	if len(drops) == 0 {
		return nil
	}
	stored, found, err := r.Store.Load(ctx, batch.ID)
	if err != nil {
		return fmt.Errorf("load batch %q before drop: %w", batch.ID, err)
	}
	if found {
		batch.Admission = stored.Admission
	}
	ids := make([]string, 0, len(drops))
	for id := range drops {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if batch.Admission == Admitted {
		return r.declineBatch(ctx, batch, declineReason, ids, drops)
	}
	for _, id := range ids {
		if err := r.returnMember(ctx, batch, id, drops[id]); err != nil {
			return err
		}
	}
	if len(batch.ActiveMembers) == 0 {
		return r.transition(ctx, batch, Empty, "no members")
	}
	return nil
}

// declineBatch ends an admitted batch with reason: it returns the dropped members (ids), each at its frozen generation
// and ready event, and sends the others to the successor, in one ledger call that also stores the declined record.
// With no ids, every member goes to the successor.
func (r Runner) declineBatch(ctx context.Context, batch *Batch, reason string, ids []string, drops map[string]string) error {
	if r.Admission == nil {
		return fmt.Errorf("decline batch %q: batch ledger is required", batch.ID)
	}
	record := cloneBatch(*batch)
	returns := make([]Return, 0, len(ids))
	for _, id := range ids {
		member, found := findMember(record.ActiveMembers, id)
		if !found {
			continue
		}
		returns = append(returns, Return{
			Migration:  member.Migration,
			Generation: member.Generation,
			ReadyEvent: member.ReadyEvent,
			Reason:     drops[id],
			Key:        returnKey(record.ID, member.Migration, member.Generation),
		})
		removeMember(&record, id, drops[id])
	}
	successor := successorID(record.ID)
	record.State, record.Reason, record.Successor = Declined, reason, successor
	if err := r.Admission.DeclineBatch(ctx, &record, reason, returns, successor); err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) && refusal.BatchConflict() {
			return fmt.Errorf("decline batch %q: %w: %w", batch.ID, ErrAdmissionConflict, err)
		}
		return fmt.Errorf("decline batch %q: %w", batch.ID, err)
	}
	*batch = record
	return nil
}

// returnMember removes an active member and records the intent to return it in one save, so the current key changes
// before the ledger hears of it. A stale_generation or not_open refusal completes the drop with that reason; a
// batch_admitted refusal restores the member and returns ErrAdmissionConflict.
func (r Runner) returnMember(ctx context.Context, batch *Batch, migration, reason string) error {
	member, found := findMember(batch.ActiveMembers, migration)
	if !found {
		return nil
	}
	key := "return/" + migration
	before := cloneBatch(*batch)
	removeMember(batch, migration, reason)
	batch.startOperation(Operation{Key: key, Kind: "return", Batch: batch.ID, Repository: batch.Repository, Member: migration, Generation: member.Generation, Reason: reason})
	if err := r.Store.Save(ctx, batch); err != nil {
		return fmt.Errorf("record return intent for %q: %w", migration, err)
	}
	if err := r.Ledger.ReturnMember(ctx, batch.Repository, member.Migration, member.Generation, reason); err != nil {
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			return fmt.Errorf("return %q: %w", migration, err)
		}
		switch refusal.Reason {
		case "stale_generation", "not_open":
			setDroppedReason(batch, migration, droppedReason(refusal.Reason, refusal.Violations))
		case "batch_admitted":
			before.RecordVersion = batch.RecordVersion
			*batch = before
			if saveErr := r.Store.Save(ctx, batch); saveErr != nil {
				return fmt.Errorf("restore member %q: %w", migration, saveErr)
			}
			return fmt.Errorf("return %q: %w: %w", migration, ErrAdmissionConflict, err)
		default:
			return fmt.Errorf("return %q: %w", migration, err)
		}
	}
	batch.completeOperation(key)
	if err := r.Store.Save(ctx, batch); err != nil {
		return fmt.Errorf("record returned member %q: %w", migration, err)
	}
	return nil
}

// removeMember takes a member out of the active set and records why.
func removeMember(batch *Batch, migration, reason string) {
	batch.ActiveMembers = withoutMember(batch.ActiveMembers, migration)
	if !containsDropped(batch.Dropped, migration) {
		batch.Dropped = append(batch.Dropped, DroppedMember{Migration: migration, Reason: reason})
	}
}

func setDroppedReason(batch *Batch, migration, reason string) {
	for index := range batch.Dropped {
		if batch.Dropped[index].Migration == migration {
			batch.Dropped[index].Reason = reason
		}
	}
}
