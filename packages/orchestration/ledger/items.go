package ledger

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueuedItem is an open item together with the event that put it in its owner's queue.
type QueuedItem struct {
	Item
	Owner        string    `json:"owner"`
	EnqueuedAt   time.Time `json:"enqueued_at"`
	EnqueueEvent int64     `json:"enqueue_event"`
}

// OpenItems reads one owner's open queue from one read-only, repeatable-read snapshot in enqueue order.
func (s *Store) OpenItems(ctx context.Context, repo, owner string) ([]QueuedItem, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, invalid("empty owner")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name = $1), queue.id, queue.owner,
		queue.state, queue.holder, queue.generation, queue.lease_expires_at, queue.result, queue.enqueued_at,
		queue.enqueue_event
		FROM (VALUES (1)) AS request
		LEFT JOIN LATERAL (
			SELECT item.id, item.owner, item.state, item.holder, item.generation, item.lease_expires_at, item.result,
				event.at AS enqueued_at, event.id AS enqueue_event
			FROM work_items AS item
			JOIN events AS event ON event.repo = item.repo AND event.subject = item.id AND event.kind = 'item.enqueued'
			WHERE item.repo = $1 AND item.owner = $2 AND item.state = 'open'
			ORDER BY event.id
		) AS queue ON true
		ORDER BY queue.enqueue_event NULLS LAST`, repo, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []QueuedItem{}
	for rows.Next() {
		var found bool
		var id, itemOwner, state, holder, result *string
		var generation, enqueueEvent *int64
		var expiresAt, enqueuedAt *time.Time
		if err := rows.Scan(&found, &id, &itemOwner, &state, &holder, &generation, &expiresAt, &result, &enqueuedAt, &enqueueEvent); err != nil {
			return nil, err
		}
		if !found {
			return nil, &NotFoundError{What: "repository", ID: repo}
		}
		if id == nil {
			continue
		}
		queued := QueuedItem{
			Item: Item{
				ID: *id, Owner: *itemOwner, State: *state, Generation: *generation, ExpiresAt: expiresAt,
			},
			Owner: *itemOwner, EnqueuedAt: *enqueuedAt, EnqueueEvent: *enqueueEvent,
		}
		if holder != nil {
			queued.Holder = *holder
		}
		if result != nil {
			queued.Result = *result
		}
		items = append(items, queued)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}
