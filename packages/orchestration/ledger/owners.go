package ledger

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

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

// OpenItems lists owner's open items in repo, in enqueue order (created_at, then id), each as Item reads it: its
// holder and generation, and its lease end only for a fixed-end lease. An owner with no open items has an empty list;
// an empty owner is invalid input.
func (s *Store) OpenItems(ctx context.Context, repo, owner string) ([]Item, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, invalid("empty owner")
	}
	items := []Item{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, owner, state, coalesce(holder, ''), generation, lease_expires_at
			FROM work_items WHERE repo = $1 AND owner = $2 AND state = 'open' ORDER BY created_at, id`, repo, owner)
		if err != nil {
			return err
		}
		read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
			var it Item
			err := row.Scan(&it.ID, &it.Owner, &it.State, &it.Holder, &it.Generation, &it.ExpiresAt)
			return it, err
		})
		if err != nil {
			return err
		}
		items = append(items[:0], read...)
		return nil
	})
	return items, err
}

// OwnerRefusal is why the supervisor declined to start owner for the work at Revision, the owner's
// OwnerWorkRevision when it declined.
type OwnerRefusal struct {
	Owner    string    `json:"owner"`
	Revision int64     `json:"revision"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

// RecordOwnerRefused records, once per owner and work revision, that the supervisor will not start
// owner for the work at revision, and appends one owner.refused event. A repeat with the same reason
// returns the stored refusal and appends nothing; one with another reason is refused
// (ReasonRefusalConflict) and changes nothing. A revision below 1, or newer than the owner's
// OwnerWorkRevision, refuses no work that exists, so it is invalid input. A refusal at an older
// revision, or at a revision where the owner has no open work, is recorded but OwnerRefusals never
// lists it.
func (s *Store) RecordOwnerRefused(ctx context.Context, repo, owner string, revision int64, reason string) (OwnerRefusal, error) {
	switch {
	case strings.TrimSpace(owner) == "":
		return OwnerRefusal{}, invalid("empty owner")
	case strings.TrimSpace(reason) == "":
		return OwnerRefusal{}, invalid("empty reason")
	case revision < 1:
		return OwnerRefusal{}, invalid("refusal revision %d is below 1", revision)
	}
	refusal := OwnerRefusal{Owner: owner, Revision: revision, Reason: reason}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, ownerWorkRevisionQuery("$2"), repo, owner).Scan(&current); err != nil {
			return err
		}
		if revision > current {
			return invalid("refusal revision %d of owner %s is newer than its work revision %d", revision, owner, current)
		}
		// The revision column holds the canonical decimal form, so text equality is integer equality.
		stored := strconv.FormatInt(revision, 10)
		err := tx.QueryRow(ctx, `INSERT INTO owner_refusals (repo, owner, revision, reason) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING RETURNING at`, repo, owner, stored, reason).Scan(&refusal.At)
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT waited for a concurrent insert of this row to commit, and under read committed the next
			// statement's snapshot sees the committed row.
			var storedReason string
			if err := tx.QueryRow(ctx, `SELECT reason, at FROM owner_refusals WHERE repo = $1 AND owner = $2 AND revision = $3`,
				repo, owner, stored).Scan(&storedReason, &refusal.At); err != nil {
				return err
			}
			if storedReason != reason {
				return refuse(ReasonRefusalConflict, "owner %s is already refused at work revision %d for another reason: %s",
					owner, revision, storedReason)
			}
			return nil
		}
		if err != nil {
			return err
		}
		return event(ctx, tx, repo, "owner.refused", owner, "", map[string]any{
			"owner": owner, "revision": revision, "reason": reason,
		})
	})
	if err != nil {
		return OwnerRefusal{}, err
	}
	return refusal, nil
}

// OwnerRefusals lists, by owner, the refusal of each owner with open work at its current
// OwnerWorkRevision: the owners OpenOwners lists, and no others. A refusal of work that has changed
// since is not listed, because the supervisor will try that owner again; nor is the refusal of an
// owner whose work is all done, because nothing is left to start it for.
func (s *Store) OwnerRefusals(ctx context.Context, repo string) ([]OwnerRefusal, error) {
	refusals := []OwnerRefusal{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.owner, r.revision::bigint, r.reason, r.at
			FROM (SELECT DISTINCT owner FROM work_items WHERE repo = $1 AND state = 'open' AND owner <> '') o
			CROSS JOIN LATERAL (`+ownerWorkRevisionQuery("o.owner")+`) AS work(revision)
			JOIN owner_refusals r ON r.repo = $1 AND r.owner = o.owner AND r.revision = work.revision::text
			ORDER BY r.owner`, repo)
		if err != nil {
			return err
		}
		read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OwnerRefusal, error) {
			var r OwnerRefusal
			err := row.Scan(&r.Owner, &r.Revision, &r.Reason, &r.At)
			return r, err
		})
		if err != nil {
			return err
		}
		refusals = append(refusals[:0], read...)
		return nil
	})
	return refusals, err
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
