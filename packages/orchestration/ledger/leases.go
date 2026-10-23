package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnsureRepo creates the repository record, or updates its landing-round limit if it exists.
func (s *Store) EnsureRepo(ctx context.Context, name string, maxLandingRounds int) (Repository, error) {
	if name == "" {
		return Repository{}, invalid("empty repository name")
	}
	if maxLandingRounds < 0 {
		return Repository{}, invalid("max landing rounds %d is negative", maxLandingRounds)
	}
	var r Repository
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var prev *int
		_ = tx.QueryRow(ctx, `SELECT max_landing_rounds FROM repositories WHERE name = $1 FOR UPDATE`, name).Scan(&prev)
		if err := tx.QueryRow(ctx, `INSERT INTO repositories (name, max_landing_rounds) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET max_landing_rounds = EXCLUDED.max_landing_rounds
			RETURNING name, max_landing_rounds, created_at`, name, maxLandingRounds).
			Scan(&r.Name, &r.MaxLandingRounds, &r.CreatedAt); err != nil {
			return err
		}
		if prev != nil && *prev == maxLandingRounds {
			return nil
		}
		return event(ctx, tx, name, "repository.configured", name, "", map[string]any{"max_landing_rounds": maxLandingRounds})
	})
	return r, err
}

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
		return tx.QueryRow(ctx, `SELECT coalesce(max(e.id), 0)
			FROM events e JOIN work_items item ON item.repo = e.repo AND item.id = e.subject
			WHERE e.repo = $1 AND item.owner = $2 AND e.kind IN ('item.enqueued', 'item.completed')`,
			repo, owner).Scan(&revision)
	})
	return revision, err
}

// checkLeaseArgs accepts a ttl of zero, a supervised lease with no end, or a fixed end of at least a
// microsecond.
func checkLeaseArgs(id, holder string, ttl time.Duration) error {
	switch {
	case id == "":
		return invalid("empty item id")
	case holder == "":
		return invalid("empty holder")
	case ttl < 0 || (ttl > 0 && ttl.Microseconds() == 0):
		return invalid("lease length %v must be zero (supervised) or at least one microsecond", ttl)
	}
	return nil
}

type itemRow struct {
	state, result string
	holder        *string
	generation    int64
	expiresAt     *time.Time
	live          bool
}

// lockItem reads an item under a row lock, then decides lease liveness from a fresh database timestamp.
func lockItem(ctx context.Context, tx pgx.Tx, repo, id string) (itemRow, error) {
	var r itemRow
	var result *string
	err := tx.QueryRow(ctx, `SELECT state, holder, generation, lease_expires_at, result
		FROM work_items WHERE repo = $1 AND id = $2 FOR UPDATE`, repo, id).
		Scan(&r.state, &r.holder, &r.generation, &r.expiresAt, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, &NotFoundError{What: "item", ID: id}
	}
	if err != nil {
		return r, err
	}
	if result != nil {
		r.result = *result
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return r, err
	}
	r.live = r.holder != nil && (r.expiresAt == nil || r.expiresAt.After(now))
	return r, nil
}

// Claim grants a lease on an item, creating the item if it is new. The generation rises by one on every grant.
// It is refused while another lease is live, including one held by the same holder (renew that instead).
// A ttl of zero grants a supervised lease: it has no end, and only Release, Complete or ReleaseHolder
// frees it.
func (s *Store) Claim(ctx context.Context, repo, id, holder string, ttl time.Duration) (Lease, error) {
	if err := checkLeaseArgs(id, holder, ttl); err != nil {
		return Lease{}, err
	}
	l := Lease{ID: id, Holder: holder}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_items (repo, id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, repo, id); err != nil {
			return err
		}
		it, err := lockItem(ctx, tx, repo, id)
		if err != nil {
			return err
		}
		if it.state == "done" {
			return refuse(ReasonDone, "item %s is done", id)
		}
		if it.live {
			return refuse(ReasonLeaseHeld, "item %s is held by %s (generation %d) %s",
				id, *it.holder, it.generation, leaseEnd(it.expiresAt))
		}
		var expires *time.Time
		if err := tx.QueryRow(ctx, `UPDATE work_items SET holder = $3, generation = generation + 1,
				lease_granted_at = clock_timestamp(),
				lease_expires_at = CASE WHEN $4::bigint = 0 THEN NULL ELSE clock_timestamp() + $4::bigint * interval '1 microsecond' END,
				updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING generation, lease_expires_at`,
			repo, id, holder, ttl.Microseconds()).Scan(&l.Generation, &expires); err != nil {
			return err
		}
		data := map[string]any{"generation": l.Generation, "supervised": expires == nil}
		if expires != nil {
			l.ExpiresAt = *expires
			data["expires_at"] = l.ExpiresAt
		}
		if it.holder != nil {
			data["expired_holder"] = *it.holder
		}
		return event(ctx, tx, repo, "item.claimed", id, holder, data)
	})
	return l, err
}

// leaseEnd describes when a lease ends: at its fixed end, or, for a supervised lease, when its
// holder is found stopped.
func leaseEnd(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "until its supervisor finds it stopped"
	}
	return "until " + expiresAt.UTC().Format(time.RFC3339)
}

// checkHolder refuses a call whose generation or holder is not the item's current one.
func checkHolder(it itemRow, id, holder string, gen int64) error {
	if it.state == "done" {
		return refuse(ReasonDone, "item %s is done", id)
	}
	if gen != it.generation {
		return refuse(ReasonStaleGeneration, "item %s is at generation %d, not %d", id, it.generation, gen)
	}
	if it.holder == nil {
		return refuse(ReasonLeaseExpired, "item %s has no live lease at generation %d", id, gen)
	}
	if *it.holder != holder {
		return refuse(ReasonNotHolder, "item %s is held by %s, not %s", id, *it.holder, holder)
	}
	return nil
}

// Renew extends a live lease. It is refused for a stale generation, another holder, a lease that has
// ended, or a supervised lease, which has no end to extend.
func (s *Store) Renew(ctx context.Context, repo, id, holder string, gen int64, ttl time.Duration) (Lease, error) {
	if err := checkLeaseArgs(id, holder, ttl); err != nil {
		return Lease{}, err
	}
	if ttl == 0 {
		return Lease{}, invalid("a renewal needs a lease length of at least one microsecond")
	}
	l := Lease{ID: id, Holder: holder, Generation: gen}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		it, err := lockItem(ctx, tx, repo, id)
		if err != nil {
			return err
		}
		if err := checkHolder(it, id, holder, gen); err != nil {
			return err
		}
		if it.expiresAt == nil {
			return refuse(ReasonSupervisedLease, "the lease on item %s is supervised: it has no end to renew", id)
		}
		if !it.live {
			return refuse(ReasonLeaseExpired, "the lease on item %s ended at %s", id, it.expiresAt.UTC().Format(time.RFC3339))
		}
		if err := tx.QueryRow(ctx, `UPDATE work_items SET lease_expires_at = lease_expires_at + $3 * interval '1 microsecond',
				updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING lease_expires_at`, repo, id, ttl.Microseconds()).Scan(&l.ExpiresAt); err != nil {
			return err
		}
		return event(ctx, tx, repo, "item.renewed", id, holder, map[string]any{"generation": gen, "expires_at": l.ExpiresAt})
	})
	return l, err
}

// Release gives up a lease early. Releasing an item that is already free at the same generation does nothing.
func (s *Store) Release(ctx context.Context, repo, id, holder string, gen int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		it, err := lockItem(ctx, tx, repo, id)
		if err != nil {
			return err
		}
		if it.holder == nil && it.state == "open" && gen == it.generation {
			return nil
		}
		if err := checkHolder(it, id, holder, gen); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE work_items SET holder = NULL, lease_expires_at = NULL, updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2`, repo, id); err != nil {
			return err
		}
		return event(ctx, tx, repo, "item.released", id, holder, map[string]any{"generation": gen})
	})
}

// Complete records the holder's result and marks the item done. A result after the lease ended is refused.
func (s *Store) Complete(ctx context.Context, repo, id, holder string, gen int64, result string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		it, err := lockItem(ctx, tx, repo, id)
		if err != nil {
			return err
		}
		if err := checkHolder(it, id, holder, gen); err != nil {
			return err
		}
		if !it.live {
			return refuse(ReasonLeaseExpired, "the lease on item %s %s", id, leaseEnd(it.expiresAt))
		}
		if _, err := tx.Exec(ctx, `UPDATE work_items SET state = 'done', result = $3, holder = NULL, lease_expires_at = NULL,
				updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2`, repo, id, result); err != nil {
			return err
		}
		return event(ctx, tx, repo, "item.completed", id, holder, map[string]any{"generation": gen, "result": result})
	})
}

// Expire frees every item whose lease has ended and returns them as they were (holder and generation).
func (s *Store) Expire(ctx context.Context, repo string) ([]Item, error) {
	var out []Item
	err := s.tx(ctx, func(tx pgx.Tx) error {
		out = nil
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, state, holder, generation, lease_expires_at
			FROM work_items
			WHERE repo = $1 AND holder IS NOT NULL
			ORDER BY id
			FOR UPDATE`, repo)
		if err != nil {
			return err
		}
		locked := []Item{}
		for rows.Next() {
			var it Item
			if err := rows.Scan(&it.ID, &it.State, &it.Holder, &it.Generation, &it.ExpiresAt); err != nil {
				rows.Close()
				return err
			}
			locked = append(locked, it)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		for _, it := range locked {
			if it.ExpiresAt == nil || it.ExpiresAt.After(now) {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE work_items SET holder = NULL, lease_expires_at = NULL, updated_at = clock_timestamp()
				WHERE repo = $1 AND id = $2`, repo, it.ID); err != nil {
				return err
			}
			out = append(out, it)
		}
		for _, it := range out {
			if err := event(ctx, tx, repo, "item.expired", it.ID, it.Holder,
				map[string]any{"generation": it.Generation, "expired_at": it.ExpiresAt}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ReleaseHolder frees, in one transaction, every lease holder has in the repository, at its current
// generation: the supervisor that watched holder found it stopped. Each freed item gets one
// item.holder_stopped event and is returned as it was. A later result or renewal at that
// generation is refused, and the item is claimed again at the next generation.
func (s *Store) ReleaseHolder(ctx context.Context, repo, holder, reason string) ([]Item, error) {
	if strings.TrimSpace(holder) == "" {
		return nil, invalid("empty holder")
	}
	var out []Item
	err := s.tx(ctx, func(tx pgx.Tx) error {
		out = []Item{}
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, owner, state, holder, generation, lease_expires_at
			FROM work_items WHERE repo = $1 AND holder = $2 ORDER BY id FOR UPDATE`, repo, holder)
		if err != nil {
			return err
		}
		freed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
			var it Item
			err := r.Scan(&it.ID, &it.Owner, &it.State, &it.Holder, &it.Generation, &it.ExpiresAt)
			return it, err
		})
		if err != nil {
			return err
		}
		for _, it := range freed {
			if _, err := tx.Exec(ctx, `UPDATE work_items SET holder = NULL, lease_expires_at = NULL, updated_at = clock_timestamp()
				WHERE repo = $1 AND id = $2`, repo, it.ID); err != nil {
				return err
			}
			if err := event(ctx, tx, repo, "item.holder_stopped", it.ID, holder,
				map[string]any{"generation": it.Generation, "reason": reason}); err != nil {
				return err
			}
		}
		out = freed
		return nil
	})
	return out, err
}

// Item reads one work item.
func (s *Store) Item(ctx context.Context, repo, id string) (Item, error) {
	var it Item
	var holder, result *string
	err := s.pool.QueryRow(ctx, `SELECT id, owner, state, holder, generation, lease_expires_at, result
		FROM work_items WHERE repo = $1 AND id = $2`, repo, id).
		Scan(&it.ID, &it.Owner, &it.State, &holder, &it.Generation, &it.ExpiresAt, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return it, &NotFoundError{What: "item", ID: id}
	}
	if holder != nil {
		it.Holder = *holder
	}
	if result != nil {
		it.Result = *result
	}
	return it, err
}

// heldItems lists items with a holder: live leases (supervised ones, which have no end, among
// them), or (stale) fixed-end leases that ended but were not yet expired.
func heldItems(ctx context.Context, q querier, repo string, stale bool, now time.Time) ([]Item, error) {
	condition := "(lease_expires_at IS NULL OR lease_expires_at > $2)"
	if stale {
		condition = "lease_expires_at <= $2"
	}
	rows, err := q.Query(ctx, `SELECT id, state, holder, generation, lease_expires_at FROM work_items
		WHERE repo = $1 AND holder IS NOT NULL AND `+condition+` ORDER BY lease_expires_at NULLS LAST, id`, repo, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		err := r.Scan(&it.ID, &it.State, &it.Holder, &it.Generation, &it.ExpiresAt)
		return it, err
	})
}
