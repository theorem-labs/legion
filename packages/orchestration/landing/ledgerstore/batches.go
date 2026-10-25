// Package ledgerstore puts the landing program on the ledger's Postgres database: the batch store, which keeps each
// batch as the ledger's versioned record, and the ledger calls that the runner makes.
package ledgerstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// ErrStale says that another writer moved the batch past the version this call read, so the caller reloads and
// decides again from the stored state.
var ErrStale = errors.New("the batch is at another record version")

// Batches stores the landing program's batches as the ledger's batch records: the whole landing.Batch is one
// record, the ledger's record_version orders every write, and the ledger's own columns (the admission, the merged
// commit, the successor and the pending effects) stay with it.
type Batches struct {
	ledger *ledger.Store
	repo   string
}

// New is the batch store of repository repo in the ledger.
func New(store *ledger.Store, repo string) *Batches {
	return &Batches{ledger: store, repo: repo}
}

// Create stores batch at version 1 and reports that it created it. When the ledger already has the id, it creates
// nothing and reports false; either way batch carries the stored record version, so the caller's next save names it.
func (s *Batches) Create(ctx context.Context, batch *landing.Batch) (bool, error) {
	record, err := record(*batch)
	if err != nil {
		return false, err
	}
	stored, created, err := s.ledger.CreateBatch(ctx, s.repo, batch.ID, record)
	if err != nil {
		return false, wrap(err)
	}
	batch.SchemaVersion = landing.BatchSchemaVersion
	batch.RecordVersion = stored.RecordVersion
	return created, nil
}

// Save writes batch over the version it carries and raises that version. A batch at another stored version is
// ErrStale, and nothing is written.
func (s *Batches) Save(ctx context.Context, batch *landing.Batch) error {
	record, err := record(*batch)
	if err != nil {
		return err
	}
	saved, err := s.ledger.SaveBatch(ctx, s.repo, batch.ID, record, batch.RecordVersion)
	if err != nil {
		return wrap(err)
	}
	batch.SchemaVersion = landing.BatchSchemaVersion
	batch.RecordVersion = saved.RecordVersion
	return nil
}

// Load reads batch id, with the record version it is stored at. An id the ledger does not have is absent, not an
// error.
func (s *Batches) Load(ctx context.Context, id string) (landing.Batch, bool, error) {
	stored, err := s.ledger.LoadBatch(ctx, s.repo, id)
	var missing *ledger.NotFoundError
	if errors.As(err, &missing) {
		return landing.Batch{}, false, nil
	}
	if err != nil {
		return landing.Batch{}, false, wrap(err)
	}
	batch, err := decode(stored)
	if err != nil {
		return landing.Batch{}, false, err
	}
	return batch, true, nil
}

// ListActive lists, in creation order, every batch that a restarted program still has work on: each batch in an
// active phase, each declined batch whose successor does not exist yet, and each terminal batch with an effect still
// to do.
func (s *Batches) ListActive(ctx context.Context) ([]landing.Batch, error) {
	stored, err := s.ledger.ListActiveBatches(ctx, s.repo)
	if err != nil {
		return nil, wrap(err)
	}
	batches := make([]landing.Batch, 0, len(stored))
	for _, row := range stored {
		batch, err := decode(row)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// record is the JSON the ledger stores for batch: the whole batch at the current schema version, without the record
// version, which the ledger's own column holds.
func record(batch landing.Batch) (json.RawMessage, error) {
	batch.SchemaVersion = landing.BatchSchemaVersion
	batch.RecordVersion = 0
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, fmt.Errorf("encode batch %q: %w", batch.ID, err)
	}
	return body, nil
}

// decode reads a stored batch and fills the version from the ledger's column.
func decode(stored ledger.Batch) (landing.Batch, error) {
	var batch landing.Batch
	if err := json.Unmarshal(stored.Record, &batch); err != nil {
		return landing.Batch{}, fmt.Errorf("decode batch %q: %w", stored.ID, err)
	}
	batch.RecordVersion = stored.RecordVersion
	batch.Admission = stored.Admission
	batch.PendingEffects = stored.PendingEffects
	return batch, nil
}

// wrap turns the ledger's stale_batch refusal into ErrStale and leaves every other error as it is.
func wrap(err error) error {
	var refusal *ledger.Refusal
	if errors.As(err, &refusal) && refusal.Reason == ledger.ReasonStaleBatch {
		return fmt.Errorf("%w: %s", ErrStale, refusal.Message)
	}
	return err
}
