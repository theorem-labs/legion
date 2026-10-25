package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Batch phases that end a batch. A batch's phase is its record's state, and every phase not listed here is active.
const (
	PhaseLanded           = "landed"
	PhaseLandedUnrecorded = "landed_unrecorded"
	PhaseFailed           = "failed"
	PhaseEmpty            = "empty"
	PhaseDeclined         = "declined"
)

var terminalPhases = []string{PhaseLanded, PhaseLandedUnrecorded, PhaseFailed, PhaseEmpty, PhaseDeclined}

// Batch is the landing program's durable batch: its record, which the ledger stores as the program sends it, and the
// ledger's own columns.
type Batch struct {
	Repo string `json:"repo"`
	ID   string `json:"id"`
	// Phase is the record's state.
	Phase  string          `json:"phase"`
	Record json.RawMessage `json:"record"`
	// RecordVersion orders every write of the batch: a write names the version it read and raises it by one.
	RecordVersion int64 `json:"record_version"`
	// Admission is admitted while the batch's members are fenced, then landed or declined, and empty before that.
	Admission string `json:"admission,omitempty"`
	// AdmissionDigest identifies the request that admitted the batch, so that a retry of it replays (AdmitBatch).
	AdmissionDigest string `json:"admission_digest,omitempty"`
	// MergedCommit is GitHub's merged commit that LandBatch or ResolveBatch landed the members at.
	MergedCommit string `json:"merged_commit,omitempty"`
	// DeclineDigest identifies the request that declined the batch, so that a retry of it replays (DeclineBatch).
	DeclineDigest string `json:"decline_digest,omitempty"`
	// Successor is the batch that the members of a declined batch go to next, when it names one.
	Successor string `json:"successor,omitempty"`
	// PendingEffects are the sorted keys of the batch's effects that are still to do.
	PendingEffects []string  `json:"pending_effects"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// batchColumns selects a batch from a query over the batches table, in the order scanBatch reads it.
const batchColumns = `repo, id, phase, record, record_version, coalesce(admission, ''), coalesce(admission_digest, ''),
	coalesce(merged_commit, ''), coalesce(decline_digest, ''), coalesce(successor, ''),
	array(SELECT e.key FROM batch_effects e WHERE e.repo = batches.repo AND e.batch = batches.id AND e.state = 'intent'
		ORDER BY e.key),
	created_at, updated_at`

func scanBatch(row pgx.Row) (Batch, error) {
	var b Batch
	err := row.Scan(&b.Repo, &b.ID, &b.Phase, &b.Record, &b.RecordVersion, &b.Admission, &b.AdmissionDigest, &b.MergedCommit,
		&b.DeclineDigest, &b.Successor, &b.PendingEffects, &b.CreatedAt, &b.UpdatedAt)
	if b.PendingEffects == nil {
		b.PendingEffects = []string{}
	}
	return b, err
}

// loadBatch reads one batch; forUpdate takes its row lock. A missing batch is NotFoundError.
func loadBatch(ctx context.Context, q querier, repo, id string, forUpdate bool) (Batch, error) {
	sql := `SELECT ` + batchColumns + ` FROM batches WHERE repo = $1 AND id = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	b, err := scanBatch(q.QueryRow(ctx, sql, repo, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return b, &NotFoundError{What: "batch", ID: id}
	}
	return b, err
}

// escalateUnrecorded enqueues, when batch is landed_unrecorded, the work item landing:<id> for its record's
// escalate_to owner, with Enqueue's idempotent rule. Every write of a batch row calls it in the write's own
// transaction, so no crash can separate a landed_unrecorded batch from its escalation.
func escalateUnrecorded(ctx context.Context, tx pgx.Tx, batch Batch) error {
	if batch.Phase != PhaseLandedUnrecorded {
		return nil
	}
	var owner string
	if err := tx.QueryRow(ctx, `SELECT coalesce(record->>'escalate_to', '') FROM batches WHERE repo = $1 AND id = $2`,
		batch.Repo, batch.ID).Scan(&owner); err != nil {
		return err
	}
	return enqueueItem(ctx, tx, batch.Repo, "landing:"+batch.ID, owner)
}

// CreateBatch stores a new batch with record at version 1 and appends batch.created. When the id already names a
// batch of the repository, it returns that batch, uncreated, and changes nothing, so a retry after a lost response
// creates nothing twice.
func (s *Store) CreateBatch(ctx context.Context, repo, id string, record json.RawMessage) (Batch, bool, error) {
	var batch Batch
	var created bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// tx retries this function after a serialization failure, so no attempt may see a previous one's result.
		created = false
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		inserted, err := scanBatch(tx.QueryRow(ctx, `INSERT INTO batches (repo, id, record, record_version, created_at, updated_at)
			SELECT $1, $2, $3::jsonb, 1, t.at, t.at FROM (SELECT clock_timestamp() AS at) t
			ON CONFLICT DO NOTHING RETURNING `+batchColumns, repo, id, string(record)))
		if errors.Is(err, pgx.ErrNoRows) {
			// The id is taken. ON CONFLICT waited for any concurrent create of it to commit, and under READ COMMITTED
			// the next statement's snapshot sees that batch.
			batch, err = loadBatch(ctx, tx, repo, id, false)
			return err
		}
		if err != nil {
			return err
		}
		batch, created = inserted, true
		if err := event(ctx, tx, repo, "batch.created", id, "", map[string]any{
			"phase": batch.Phase, "record_version": batch.RecordVersion,
		}); err != nil {
			return err
		}
		return escalateUnrecorded(ctx, tx, batch)
	})
	if err != nil {
		return Batch{}, false, err
	}
	return batch, created, nil
}

// SaveBatch writes record over batch id, which the caller read at expectedVersion, and appends batch.saved. The
// ledger trusts its caller, so the batch's phase becomes whatever state the record holds. In one transaction:
//   - the replay: when the stored version is expectedVersion+1 and the stored record equals record, this is a retry
//     of the save that wrote it, whose response was lost, so it returns the stored batch and changes nothing;
//   - the version: another stored version refuses with stale_batch and changes nothing;
//   - the write: the record, the next version, and for a landed_unrecorded record its escalation item.
func (s *Store) SaveBatch(ctx context.Context, repo, id string, record json.RawMessage, expectedVersion int64) (Batch, error) {
	var saved Batch
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		stored, err := loadBatch(ctx, tx, repo, id, true)
		if err != nil {
			return err
		}
		if stored.RecordVersion == expectedVersion+1 {
			var replay bool
			if err := tx.QueryRow(ctx, `SELECT record = $3::jsonb FROM batches WHERE repo = $1 AND id = $2`,
				repo, id, string(record)).Scan(&replay); err != nil {
				return err
			}
			if replay {
				saved = stored
				return nil
			}
		}
		if stored.RecordVersion != expectedVersion {
			return refuse(ReasonStaleBatch, "batch %s is at record version %d, not version %d", id, stored.RecordVersion, expectedVersion)
		}
		if saved, err = scanBatch(tx.QueryRow(ctx, `UPDATE batches
			SET record = $3::jsonb, record_version = record_version + 1, updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING `+batchColumns, repo, id, string(record))); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "batch.saved", id, "", map[string]any{
			"from_phase": stored.Phase, "phase": saved.Phase, "record_version": saved.RecordVersion,
		}); err != nil {
			return err
		}
		return escalateUnrecorded(ctx, tx, saved)
	})
	if err != nil {
		return Batch{}, err
	}
	return saved, nil
}

// LoadBatch reads batch id of the repository.
func (s *Store) LoadBatch(ctx context.Context, repo, id string) (Batch, error) {
	return loadBatch(ctx, s.pool, repo, id, false)
}

// ListActiveBatches lists, in creation order, every batch of the repository that a restarted landing program still has
// work on: each batch in an active phase; each declined batch whose successor names no batch yet, so the program
// creates it; and each terminal batch with an effect still to do, so the program finishes it. A successor stored as
// NULL or as the empty string is none, as Batch.Successor reads it.
func (s *Store) ListActiveBatches(ctx context.Context, repo string) ([]Batch, error) {
	batches := []Batch{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+batchColumns+` FROM batches
			WHERE repo = $1 AND (
				phase <> ALL($2)
				OR (phase = $3 AND coalesce(successor, '') <> '' AND NOT EXISTS (
					SELECT 1 FROM batches successors WHERE successors.repo = batches.repo AND successors.id = batches.successor))
				OR EXISTS (SELECT 1 FROM batch_effects e WHERE e.repo = batches.repo AND e.batch = batches.id AND e.state = 'intent'))
			ORDER BY created_at, id`, repo, terminalPhases, PhaseDeclined)
		if err != nil {
			return err
		}
		batches, err = pgx.AppendRows(batches[:0], rows, func(row pgx.CollectableRow) (Batch, error) { return scanBatch(row) })
		return err
	})
	return batches, err
}
