package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// reopeningKinds are the events after which an owner has work it did not have, or work nobody
// holds any more: a new item, a lease given up, a fixed-end lease that ran out, and a stopped
// holder's leases freed.
var reopeningKinds = []string{"item.enqueued", "item.released", "item.expired", "item.holder_stopped"}

// Enqueue creates an open work item for owner. Repeating an open enqueue for the same owner is
// idempotent; an existing item belongs to one owner for its whole history.
func (s *Store) Enqueue(ctx context.Context, repo, id, owner string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" {
		return invalid("an enqueued item needs an id and an owner")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		var existingOwner, state string
		err := tx.QueryRow(ctx, `INSERT INTO work_items (repo, id, owner) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING RETURNING owner, state`, repo, id, owner).Scan(&existingOwner, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT owner, state FROM work_items WHERE repo = $1 AND id = $2 FOR UPDATE`,
				repo, id).Scan(&existingOwner, &state); err != nil {
				return err
			}
			if existingOwner == owner && state == "open" {
				return nil
			}
			return refuse(ReasonExists, "item %s already belongs to owner %s in state %s", id, existingOwner, state)
		}
		if err != nil {
			return err
		}
		return event(ctx, tx, repo, "item.enqueued", id, owner, map[string]any{"owner": owner})
	})
}

// HasOpenWork reports whether owner names at least one open item in repository.
func (s *Store) HasOpenWork(ctx context.Context, repo, owner string) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, invalid("empty owner")
	}
	var open bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM work_items WHERE repo = $1 AND owner = $2 AND state = 'open'
		)`, repo, owner).Scan(&open)
	})
	return open, err
}

// OwnerWorkRevision is the newest durable queue-change event of owner. Leasing an item does not
// change its work, while enqueueing and completing one do; callers use the revision to distinguish
// new work from the same work that exhausted a retry budget.
func (s *Store) OwnerWorkRevision(ctx context.Context, repo, owner string) (int64, error) {
	if strings.TrimSpace(owner) == "" {
		return 0, invalid("empty owner")
	}
	var revision int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		return tx.QueryRow(ctx, ownerWorkRevisionQuery("$2"), repo, owner).Scan(&revision)
	})
	return revision, err
}

// ownerWorkRevisionQuery selects OwnerWorkRevision, zero for an owner with no work, of the owner that the SQL
// expression owner names in repository $1: a parameter, or a column of an enclosing query.
func ownerWorkRevisionQuery(owner string) string {
	return `SELECT coalesce(max(e.id), 0)
		FROM events e JOIN work_items item ON item.repo = e.repo AND item.id = e.subject
		WHERE e.repo = $1 AND item.owner = ` + owner + ` AND e.kind IN ('item.enqueued', 'item.completed')`
}

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

// RecordOwnerRefused records that the supervisor will not start owner for the work at revision, and appends one
// owner.refused event. A repeat with the same reason returns the stored refusal and appends nothing; one with another
// reason replaces the stored reason and appends one event with it, so OwnerRefusals shows the latest reason. A revision
// below 1 is invalid input. A refusal at a revision that is not the owner's current work revision is recorded, but
// OwnerRefusals never lists it.
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
		// ON CONFLICT waits for a concurrent insert of this row to commit. A stored refusal with the same reason is
		// left as it is, and under read committed the next statement's snapshot sees it.
		err := tx.QueryRow(ctx, `INSERT INTO owner_refusals (repo, owner, revision, reason) VALUES ($1, $2, $3, $4)
			ON CONFLICT (repo, owner, revision) DO UPDATE SET reason = EXCLUDED.reason, at = clock_timestamp()
			WHERE owner_refusals.reason <> EXCLUDED.reason
			RETURNING at`, repo, owner, revision, reason).Scan(&refusal.At)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT at FROM owner_refusals WHERE repo = $1 AND owner = $2 AND revision = $3`,
				repo, owner, revision).Scan(&refusal.At)
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
		rows, err := tx.Query(ctx, `SELECT r.owner, r.revision, r.reason, r.at
			FROM (SELECT DISTINCT owner FROM work_items WHERE repo = $1 AND state = 'open' AND owner <> '') o
			CROSS JOIN LATERAL (`+ownerWorkRevisionQuery("o.owner")+`) AS work(revision)
			JOIN owner_refusals r ON r.repo = $1 AND r.owner = o.owner AND r.revision = work.revision
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
