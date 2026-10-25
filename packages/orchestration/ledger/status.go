package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Status is what a steward or an agent needs at a glance: open migrations, live leases and stale ones.
type Status struct {
	Repository Repository `json:"repository"`
	// Leases are live leases. Stale are leases that ended but have not yet been expired; anyone may claim them.
	Leases     []Item `json:"leases"`
	Stale      []Item `json:"stale"`
	Migrations []Migration
	Escalated  []Migration
}

// Status reads the repository's current state.
func (s *Store) Status(ctx context.Context, repo string) (Status, error) {
	var st Status
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `SELECT name, max_landing_rounds, created_at FROM repositories WHERE name = $1`, repo).
		Scan(&st.Repository.Name, &st.Repository.MaxLandingRounds, &st.Repository.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, &NotFoundError{What: "repository", ID: repo}
	}
	if err != nil {
		return st, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return st, err
	}
	if st.Leases, err = heldItems(ctx, tx, repo, false, now); err != nil {
		return st, err
	}
	if st.Stale, err = heldItems(ctx, tx, repo, true, now); err != nil {
		return st, err
	}
	if st.Migrations, err = migrations(ctx, tx, repo, StateOpen); err != nil {
		return st, err
	}
	st.Escalated = []Migration{}
	for _, m := range st.Migrations {
		if m.EscalatedAt != nil {
			st.Escalated = append(st.Escalated, m)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return st, err
	}
	return st, nil
}
