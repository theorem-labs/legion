package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReadyRequest stores the exact statement records an owner offers for a migration's landing.
//
// Ready checks the records' shape, by the rules check and land apply, and stores them as given: Land applies the
// review and pinned rules when the migration lands.
type ReadyRequest struct {
	Repo       string `json:"repo"`
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	// Key identifies one submission. A retry with the same key returns the record that submission stored and
	// publishes nothing, even after a round consumed it; a new submission takes a new key.
	Key string `json:"key"`
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
	Generation int64  `json:"generation"`
	Key        string `json:"key"`
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

// clearReadyEvent removes the stored ready record only when it is the one whose migration.ready event is readyEvent,
// and reports whether it did. A record the owner published since then stays.
func clearReadyEvent(ctx context.Context, tx pgx.Tx, repo, id string, readyEvent int64) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE migrations SET ready_at = NULL, ready_event = NULL, ready_records = NULL
		WHERE repo = $1 AND id = $2 AND ready_event = $3`, repo, id, readyEvent)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Ready records the exact landing inputs an owner supplies for an open, current, un-escalated migration. A ready
// record names the head that was built. A later ready replaces the previous record and has the id of its own
// migration.ready event. An absent list or map is recorded empty, so a record has one shape whether its request came
// from flags or from JSON.
//
// The key names the submission. A repeated key returns the record as its migration.ready event stored it and
// publishes nothing, whatever the rest of the request says and even after a round consumed that record, so a retry
// after a lost answer cannot publish a returned candidate again. A new submission takes a new key.
func (s *Store) Ready(ctx context.Context, req ReadyRequest) (ReadyRecord, error) {
	if req.Key == "" {
		return ReadyRecord{}, invalid("a ready submission needs an idempotency key")
	}
	if err := checkRecord("head", req.Head, true); err != nil {
		return ReadyRecord{}, err
	}
	if err := req.validate(); err != nil {
		return ReadyRecord{}, err
	}
	var ready ReadyRecord
	err := s.tx(ctx, func(tx pgx.Tx) error {
		ready = ReadyRecord{ID: req.ID, Generation: req.Generation, StatementRecords: req.withEmptyLists()}
		if _, err := lockRepo(ctx, tx, req.Repo, true); err != nil {
			return err
		}
		migration, err := loadMigration(ctx, tx, req.Repo, req.ID, true)
		if err != nil {
			return err
		}
		var published int64
		switch err := tx.QueryRow(ctx, `SELECT ready_event FROM ready_keys WHERE repo = $1 AND migration = $2 AND key = $3`,
			req.Repo, req.ID, req.Key).Scan(&published); {
		case err == nil:
			ready, err = publishedReady(ctx, tx, published)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := requireOpenAt(migration, req.Generation); err != nil {
			return err
		}
		if migration.EscalatedAt != nil {
			return refuse(ReasonEscalated, "migration %s was escalated to the steward: %s", req.ID, migration.EscalationReason)
		}
		event, err := eventRecord(ctx, tx, req.Repo, "migration.ready", req.ID, migration.Owner, readyEventData{
			Generation: req.Generation, Key: req.Key, StatementRecords: ready.StatementRecords,
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
		if _, err := tx.Exec(ctx, `INSERT INTO ready_keys (repo, migration, key, ready_event) VALUES ($1, $2, $3, $4)`,
			req.Repo, req.ID, req.Key, event.ID); err != nil {
			return err
		}
		ready.ReadyEvent, ready.ReadyAt = event.ID, event.At
		return nil
	})
	return ready, err
}

// publishedReady is the ready record that the migration.ready event id published, read from that event.
func publishedReady(ctx context.Context, q querier, id int64) (ReadyRecord, error) {
	record := ReadyRecord{ReadyEvent: id}
	var data readyEventData
	if err := q.QueryRow(ctx, `SELECT subject, actor, at, data FROM events WHERE id = $1`, id).
		Scan(&record.ID, &record.Owner, &record.ReadyAt, &data); err != nil {
		return ReadyRecord{}, err
	}
	record.Generation, record.StatementRecords = data.Generation, data.withEmptyLists()
	return record, nil
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
