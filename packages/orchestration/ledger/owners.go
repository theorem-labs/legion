package ledger

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// reopeningKinds are the events after which an owner has work it did not have, or work nobody
// holds any more: a new item, a lease given up, a fixed-end lease that ran out, and a stopped
// holder's leases freed.
var reopeningKinds = []string{"item.enqueued", "item.released", "item.expired", "item.holder_stopped"}

// OpenOwners lists, in order and once each, the owners that open items in repo name.
func (s *Store) OpenOwners(ctx context.Context, repo string) ([]string, error) {
	owners := []string{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT owner FROM work_items
			WHERE repo = $1 AND state = 'open' AND owner <> '' ORDER BY owner`, repo)
		if err != nil {
			return err
		}
		read, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		owners = append(owners[:0], read...)
		return nil
	})
	return owners, err
}

// LatestEventID is the id of repo's newest event, or zero when it has none: the cursor a reader of
// OwnerWakes starts from to see only what happens next.
func (s *Store) LatestEventID(ctx context.Context, repo string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events WHERE repo = $1`, repo).Scan(&id)
	return id, err
}

// OwnerWakes reads, in one snapshot, the owners of the items that the events after cursor reopened
// (reopeningKinds), in order and once each, and the cursor that reading moves to: the newest event
// it saw of any kind. Event ids commit in order within a repository, so no event at or below that
// cursor can still be uncommitted.
func (s *Store) OwnerWakes(ctx context.Context, repo string, cursor int64) ([]string, int64, error) {
	owners := []string{}
	last := cursor
	err := s.pool.QueryRow(ctx, `WITH upto AS (
			SELECT coalesce(max(id), $2) AS last FROM events WHERE repo = $1 AND id > $2
		)
		SELECT upto.last, coalesce(array(
			SELECT DISTINCT item.owner FROM events e
			JOIN work_items item ON item.repo = e.repo AND item.id = e.subject
			WHERE e.repo = $1 AND e.id > $2 AND e.id <= upto.last AND e.kind = ANY($3) AND item.owner <> ''
			ORDER BY 1
		), '{}') FROM upto`, repo, cursor, reopeningKinds).Scan(&last, &owners)
	return owners, last, err
}

// Listener is one connection listening on EventsChannel. A notification names the repository whose
// events changed and carries nothing else: a reader reads the events themselves (OwnerWakes), so a
// notification that is lost delays work rather than losing it.
type Listener struct {
	conn *pgx.Conn
}

// Listen opens a connection of its own and listens on EventsChannel.
func Listen(ctx context.Context, dsn string) (*Listener, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{EventsChannel}.Sanitize()); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return &Listener{conn: conn}, nil
}

// Wait returns the repository of the next notification, or ctx's error once ctx ends first.
func (l *Listener) Wait(ctx context.Context) (string, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(n.Payload), nil
}

// Close ends the listening connection.
func (l *Listener) Close(ctx context.Context) error { return l.conn.Close(ctx) }
