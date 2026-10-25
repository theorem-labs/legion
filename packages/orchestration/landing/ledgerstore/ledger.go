package ledgerstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// Ledger makes the landing program's batch calls on the ledger's Go API: the admission that fences a batch's members
// until GitHub's merge, the landing that ends it, the decline that ends it without one, and the withdraw effect.
// Each call stores the batch's record in the same transaction, so a crash can never separate a batch's state from
// the ledger's.
type Ledger struct {
	ledger *ledger.Store
	repo   string
}

// NewLedger is the batch ledger of repository repo.
func NewLedger(store *ledger.Store, repo string) *Ledger {
	return &Ledger{ledger: store, repo: repo}
}

// Admit admits the batch's members with the record the runner holds, and leaves the batch at the version and
// admission the ledger wrote.
func (l *Ledger) Admit(ctx context.Context, batch *landing.Batch, members []landing.AdmitMember) error {
	body, err := record(*batch)
	if err != nil {
		return err
	}
	request := ledger.AdmitRequest{
		Repo:            l.repo,
		ID:              batch.ID,
		Base:            batch.Base,
		CandidateCommit: batch.Candidate.Commit,
		CandidateTree:   batch.Candidate.Tree,
		Members:         make([]ledger.AdmitMember, 0, len(members)),
		ExpectedVersion: batch.RecordVersion,
		Record:          body,
	}
	for _, member := range members {
		request.Members = append(request.Members, ledger.AdmitMember{
			Migration:     member.Migration,
			ReadyEvent:    member.ReadyEvent,
			CurrentHashes: member.CurrentHashes,
			ChangedPaths:  member.ChangedPaths,
		})
	}
	admitted, err := l.ledger.AdmitBatch(ctx, request)
	if err != nil {
		return refusal(err)
	}
	// A replayed admission (a retry after a lost response) returns the record the ledger stored, which the runner
	// adopts: its next save carries the admitted effects and never a list rebuilt from a changed configuration.
	stored, err := decode(admitted)
	if err != nil {
		return err
	}
	*batch = stored
	return nil
}

// LandBatch lands every admitted member at mergedCommit and stores the landed record.
func (l *Ledger) LandBatch(ctx context.Context, batch *landing.Batch, mergedCommit string) error {
	body, err := record(*batch)
	if err != nil {
		return err
	}
	landed, err := l.ledger.LandBatch(ctx, ledger.LandBatchRequest{
		Repo:            l.repo,
		ID:              batch.ID,
		MergedCommit:    mergedCommit,
		ExpectedVersion: batch.RecordVersion,
		Record:          body,
	})
	if err != nil {
		return refusal(err)
	}
	apply(batch, landed)
	return nil
}

// DeclineBatch ends the admission without a landing: it records each return at the ready record that this batch
// froze, names the successor and stores the declined record.
func (l *Ledger) DeclineBatch(ctx context.Context, batch *landing.Batch, reason string, returns []landing.Return, successor string) error {
	body, err := record(*batch)
	if err != nil {
		return err
	}
	request := ledger.DeclineRequest{
		Repo:            l.repo,
		ID:              batch.ID,
		Reason:          reason,
		Returns:         make([]ledger.RoundRequest, 0, len(returns)),
		Successor:       successor,
		ExpectedVersion: batch.RecordVersion,
		Record:          body,
	}
	for _, returned := range returns {
		request.Returns = append(request.Returns, ledger.RoundRequest{
			Repo:       l.repo,
			ID:         returned.Migration,
			Generation: returned.Generation,
			Reason:     returned.Reason,
			Batch:      batch.ID,
			Key:        returned.Key,
			ReadyEvent: returned.ReadyEvent,
		})
	}
	declined, err := l.ledger.DeclineBatch(ctx, request)
	if err != nil {
		return refusal(err)
	}
	apply(batch, declined)
	return nil
}

// BeginWithdraw records the batch's withdraw effect. A repeat records nothing more.
func (l *Ledger) BeginWithdraw(ctx context.Context, repo, batch string) error {
	if _, err := l.ledger.BeginWithdraw(ctx, repo, batch); err != nil {
		return refusal(err)
	}
	return nil
}

// EndWithdraw records the batch's withdraw effect done. A repeat finds it done and succeeds.
func (l *Ledger) EndWithdraw(ctx context.Context, repo, batch string) error {
	_, err := l.ledger.BatchEffectDone(ctx, repo, batch, "withdraw/"+batch, ledger.DoneRequest{
		Result: json.RawMessage(`{"outcome":"withdrawn"}`),
	})
	var refused *ledger.Refusal
	if errors.As(err, &refused) && refused.Reason == ledger.ReasonEffectDone {
		return nil
	}
	if err != nil {
		return refusal(err)
	}
	return nil
}

// ReadyMembers lists the repository's ready migrations, in the order their ready records were published, each as the
// member a batch freezes from it.
func (l *Ledger) ReadyMembers(ctx context.Context, repo string) ([]landing.Member, error) {
	records, err := l.ledger.ReadyMigrations(ctx, repo)
	if err != nil {
		return nil, err
	}
	members := make([]landing.Member, 0, len(records))
	for _, record := range records {
		members = append(members, landing.Member{
			Migration:        record.ID,
			Generation:       record.Generation,
			Head:             record.Head,
			ReadyEvent:       record.ReadyEvent,
			ChangedContracts: record.ChangedContracts,
			ApprovedPinned:   record.ApprovedPinned,
			Upstream:         record.Upstream,
		})
	}
	return members, nil
}

// apply puts the ledger's version and admission on the runner's batch, so its next save names them.
func apply(batch *landing.Batch, stored ledger.Batch) {
	batch.SchemaVersion = landing.BatchSchemaVersion
	batch.RecordVersion = stored.RecordVersion
	batch.Admission = stored.Admission
	batch.PendingEffects = stored.PendingEffects
}

// refusal turns a ledger refusal into the landing program's, which the runner routes by its outcome, and leaves
// every other error as it is.
func refusal(err error) error {
	var refused *ledger.Refusal
	if !errors.As(err, &refused) {
		return err
	}
	kinds := make([]string, 0, len(refused.Violations))
	for _, violation := range refused.Violations {
		kinds = append(kinds, violation.Kind)
	}
	return &landing.LedgerRefusal{
		Reason:     refused.Reason,
		Message:    refused.Message,
		Member:     refused.Member,
		Violations: kinds,
	}
}
