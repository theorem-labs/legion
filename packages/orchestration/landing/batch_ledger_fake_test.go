package landing

import (
	"context"
	"slices"
)

// batchLedgerFake behaves as the ledger's batch calls do for the runner: an admission holds the batch, a landing
// lands every active member at the merged commit in one call (recorded on the member ledger fake, so the tests that
// count landings see them), and records each post-landing effect as pending; a decline records its returns.
type batchLedgerFake struct {
	ledger *ledgerFake
	// store, when set, receives each call's record, as the ledger stores it in the call's own transaction.
	store     BatchStore
	landedAt  map[string]string
	admitted  []string
	landings  []string
	declines  []declined
	withdraws []string
	done      []string
	refuse    error
	landErr   error
}

type declined struct {
	batch, reason, successor string
	returns                  []Return
}

func (f *batchLedgerFake) Admit(_ context.Context, batch *Batch, _ []AdmitMember) error {
	if f.refuse != nil {
		return f.refuse
	}
	f.admitted = append(f.admitted, batch.ID)
	batch.Admission = Admitted
	return f.write(batch)
}

// write stores the call's record, as the ledger does in the call's transaction; without a store it only raises the
// version.
func (f *batchLedgerFake) write(batch *Batch) error {
	if f.store == nil {
		batch.RecordVersion++
		return nil
	}
	admission := batch.Admission
	if err := f.store.Save(context.Background(), batch); err != nil {
		return err
	}
	batch.Admission = admission
	return nil
}

func (f *batchLedgerFake) LandBatch(_ context.Context, batch *Batch, mergedCommit string) error {
	if f.landErr != nil {
		return f.landErr
	}
	if f.landedAt[batch.ID] == mergedCommit {
		// A retry of a landing whose response was lost replays.
		batch.Admission = AdmissionLanded
		return nil
	}
	f.landings = append(f.landings, batch.ID)
	if f.landedAt == nil {
		f.landedAt = map[string]string{}
	}
	f.landedAt[batch.ID] = mergedCommit
	if f.ledger != nil {
		for _, member := range batch.ActiveMembers {
			if err := f.ledger.Land(context.Background(), LandRequest{Repo: batch.Repository, Migration: member.Migration,
				Generation: member.Generation, Commit: mergedCommit}); err != nil {
				return err
			}
		}
	}
	batch.Admission = AdmissionLanded
	batch.PendingEffects = nil
	for _, effect := range batch.PostLandEffects {
		batch.PendingEffects = append(batch.PendingEffects, effect.Key)
	}
	slices.Sort(batch.PendingEffects)
	return f.write(batch)
}

func (f *batchLedgerFake) DeclineBatch(_ context.Context, batch *Batch, reason string, returns []Return, successor string) error {
	f.declines = append(f.declines, declined{batch: batch.ID, reason: reason, successor: successor, returns: returns})
	batch.Admission = AdmissionDeclined
	return f.write(batch)
}

func (f *batchLedgerFake) BeginWithdraw(_ context.Context, _, batch string) error {
	f.withdraws = append(f.withdraws, batch)
	return nil
}

func (f *batchLedgerFake) EndWithdraw(context.Context, string, string) error { return nil }

func (f *batchLedgerFake) EffectDone(_ context.Context, _, _, key string, _ []byte) error {
	f.done = append(f.done, key)
	return nil
}

func (f *batchLedgerFake) ReadyMembers(context.Context, string) ([]Member, error) { return nil, nil }

func (f *githubFake) Withdraw(context.Context, string, int) error { return nil }

func (f *githubFake) Read(context.Context, string, int) (PullRequest, error) { return f.wait, nil }

func (g *mergingGitHub) Withdraw(context.Context, string, int) error { return nil }

func (g *mergingGitHub) Read(ctx context.Context, repository string, number int) (PullRequest, error) {
	return g.Wait(ctx, repository, number, g.head)
}
