package ledger

import (
	"context"
	"errors"
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

func checkLeaseArgs(id, holder string, ttl time.Duration) error {
	switch {
	case id == "":
		return invalid("empty item id")
	case holder == "":
		return invalid("empty holder")
	case ttl <= 0 || ttl.Microseconds() == 0:
		return invalid("lease length %v must be at least one microsecond", ttl)
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
	r.live = r.holder != nil && r.expiresAt != nil && r.expiresAt.After(now)
	return r, nil
}

// Claim grants a lease on an item, creating the item if it is new. The generation rises by one on every grant.
// It is refused while another lease is live, including one held by the same holder (renew that instead).
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
			return refuse(ReasonLeaseHeld, "item %s is held by %s (generation %d) until %s",
				id, *it.holder, it.generation, it.expiresAt.UTC().Format(time.RFC3339))
		}
		if err := tx.QueryRow(ctx, `UPDATE work_items SET holder = $3, generation = generation + 1,
				lease_granted_at = clock_timestamp(), lease_expires_at = clock_timestamp() + $4 * interval '1 microsecond',
				updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING generation, lease_expires_at`,
			repo, id, holder, ttl.Microseconds()).Scan(&l.Generation, &l.ExpiresAt); err != nil {
			return err
		}
		data := map[string]any{"generation": l.Generation, "expires_at": l.ExpiresAt}
		if it.holder != nil {
			data["expired_holder"] = *it.holder
		}
		return event(ctx, tx, repo, "item.claimed", id, holder, data)
	})
	return l, err
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

// Renew extends a live lease. It is refused for a stale generation, another holder, or a lease that has ended.
func (s *Store) Renew(ctx context.Context, repo, id, holder string, gen int64, ttl time.Duration) (Lease, error) {
	if err := checkLeaseArgs(id, holder, ttl); err != nil {
		return Lease{}, err
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
			return refuse(ReasonLeaseExpired, "the lease on item %s ended at %s", id, it.expiresAt.UTC().Format(time.RFC3339))
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

// Item reads one work item.
func (s *Store) Item(ctx context.Context, repo, id string) (Item, error) {
	var it Item
	var holder, result *string
	err := s.pool.QueryRow(ctx, `SELECT id, state, holder, generation, lease_expires_at, result
		FROM work_items WHERE repo = $1 AND id = $2`, repo, id).
		Scan(&it.ID, &it.State, &holder, &it.Generation, &it.ExpiresAt, &result)
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

// heldItems lists items with a holder: live leases, or (stale) leases that ended but were not yet expired.
func heldItems(ctx context.Context, q querier, repo string, stale bool, now time.Time) ([]Item, error) {
	op := ">"
	if stale {
		op = "<="
	}
	rows, err := q.Query(ctx, `SELECT id, state, holder, generation, lease_expires_at FROM work_items
		WHERE repo = $1 AND holder IS NOT NULL AND lease_expires_at `+op+` $2 ORDER BY lease_expires_at, id`, repo, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		err := r.Scan(&it.ID, &it.State, &it.Holder, &it.Generation, &it.ExpiresAt)
		return it, err
	})
}
