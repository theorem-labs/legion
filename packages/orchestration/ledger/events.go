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
// announcement is delivered only when tx commits. The transaction-scoped lock makes assigned event
// IDs commit in order within a repository, so a cursor cannot advance past an event that is still
// uncommitted.
func event(ctx context.Context, tx pgx.Tx, repo, kind, subject, actor string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, repo); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO events (repo, kind, subject, actor, data) VALUES ($1, $2, $3, $4, $5)`,
		repo, kind, subject, actor, b); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, EventsChannel, repo)
	return err
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
