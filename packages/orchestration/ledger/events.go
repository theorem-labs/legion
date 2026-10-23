package ledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventsChannel is the PostgreSQL notification channel every event is announced on, with its
// repository as the payload, when its transaction commits.
const EventsChannel = "ledger_events"

// event appends one row to the event log inside tx and announces it on EventsChannel; the
// announcement is delivered only when tx commits.
func event(ctx context.Context, tx pgx.Tx, repo, kind, subject, actor string, data any) error {
	_, err := eventRecord(ctx, tx, repo, kind, subject, actor, data)
	return err
}

// eventRecord appends an event and returns its immutable id and database timestamp. The transaction-scoped lock
// makes assigned event IDs commit in order within a repository, so a cursor cannot advance past an event that is
// still uncommitted.
func eventRecord(ctx context.Context, tx pgx.Tx, repo, kind, subject, actor string, data any) (Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, repo); err != nil {
		return Event{}, err
	}
	var event Event
	if err := tx.QueryRow(ctx, `INSERT INTO events (repo, kind, subject, actor, data)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, at`, repo, kind, subject, actor, b).Scan(&event.ID, &event.At); err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, EventsChannel, repo); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Event is one entry of the append-only log.
type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Subject string          `json:"subject"`
	Actor   string          `json:"actor,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// Events lists a repository's events after the given id, oldest first; subject "" means every subject.
func (s *Store) Events(ctx context.Context, repo string, after int64, subject string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, at, kind, subject, actor, data FROM events
		WHERE repo = $1 AND id > $2 AND ($3 = '' OR subject = $3) ORDER BY id`, repo, after, subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.At, &e.Kind, &e.Subject, &e.Actor, &e.Data)
		return e, err
	})
}
