package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReadyRequest stores the exact statement records an owner offers for a migration's landing.
//
// Ready refuses an upstream record unless the migration's owner is the repository's upstream owner. It checks the other
// records' shape, by the rules check and land apply, and stores them as given: Land applies the review and pinned rules
// when the migration lands.
type ReadyRequest struct {
	Repo       string `json:"repo"`
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	StatementRecords
}

// ReadyRecord is a ready migration's stored landing record.
type ReadyRecord struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Generation int64     `json:"generation"`
	ReadyEvent int64     `json:"ready_event"`
	ReadyAt    time.Time `json:"ready_at"`
	StatementRecords
}

type readyEventData struct {
	Generation int64 `json:"generation"`
	StatementRecords
}

// clearReady removes the stored ready record and reports whether one was present.
// Callers invoke it from their mutation transaction before they append the event that reports the clearing.
func clearReady(ctx context.Context, tx pgx.Tx, repo, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE migrations SET ready_at = NULL, ready_event = NULL, ready_records = NULL
		WHERE repo = $1 AND id = $2 AND ready_event IS NOT NULL`, repo, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Ready records the exact landing inputs an owner supplies for an open, current, un-escalated migration. A ready
// record names the head that was built. A later ready replaces the previous record and has the id of its own
// migration.ready event. An absent list or map is recorded empty, so a record has one shape whether its request came
// from flags or from JSON.
func (s *Store) Ready(ctx context.Context, req ReadyRequest) (ReadyRecord, error) {
	if err := checkRecord("head", req.Head, true); err != nil {
		return ReadyRecord{}, err
	}
	if err := req.validate(); err != nil {
		return ReadyRecord{}, err
	}
	ready := ReadyRecord{ID: req.ID, Generation: req.Generation, StatementRecords: req.withEmptyLists()}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, req.Repo, true)
		if err != nil {
			return err
		}
		migration, err := lockOpenAt(ctx, tx, req.Repo, req.ID, req.Generation)
		if err != nil {
			return err
		}
		if migration.EscalatedAt != nil {
			return refuse(ReasonEscalated, "migration %s was escalated to the steward: %s", req.ID, migration.EscalationReason)
		}
		if upstream, blocked := upstreamViolation(repository, migration, req.Upstream); blocked {
			return refusalForViolation(upstream)
		}
		event, err := eventRecord(ctx, tx, req.Repo, "migration.ready", req.ID, migration.Owner, readyEventData{
			Generation: req.Generation, StatementRecords: ready.StatementRecords,
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE migrations SET ready_at = $3, ready_event = $4, ready_records = $5
			WHERE repo = $1 AND id = $2
			RETURNING owner`, req.Repo, req.ID, event.At, event.ID, ready.StatementRecords).
			Scan(&ready.Owner); err != nil {
			return err
		}
		ready.ReadyEvent, ready.ReadyAt = event.ID, event.At
		return nil
	})
	return ready, err
}

// ReadyMigrations lists every currently open migration that has a ready record, in the order that the ready events
// were appended.
func (s *Store) ReadyMigrations(ctx context.Context, repo string) ([]ReadyRecord, error) {
	ready := []ReadyRecord{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, owner, generation, ready_event, ready_at, ready_records
			FROM migrations
			WHERE repo = $1 AND state = 'open' AND ready_event IS NOT NULL
			ORDER BY ready_event`, repo)
		if err != nil {
			return err
		}
		ready, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReadyRecord, error) {
			var record ReadyRecord
			err := row.Scan(&record.ID, &record.Owner, &record.Generation, &record.ReadyEvent, &record.ReadyAt,
				&record.StatementRecords)
			return record, err
		})
		return err
	})
	return ready, err
}
