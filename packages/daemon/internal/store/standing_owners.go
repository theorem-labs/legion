package store

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ReserveStandingOwnerAttempt atomically admits one owner attempt under both the running-owner cap
// and the persistent dollar budget. The generation is the launch Machine will record next.
func (s *Store) ReserveStandingOwnerAttempt(ctx context.Context, c supervise.Claim, maxRunning int, budget, cost float64) error {
	if !claim.IsStandingOwner(c.Role, c.Issue, c.Tree) {
		return nil
	}
	if maxRunning < 1 || budget <= 0 || cost <= 0 || math.IsNaN(budget) || math.IsInf(budget, 0) ||
		math.IsNaN(cost) || math.IsInf(cost, 0) {
		return fmt.Errorf("standing-owner admission has invalid limits")
	}
	generation := int64(c.Generation + 1)
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, c.Project); err != nil {
			return err
		}
		var exists, capacityReserved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM standing_owner_attempts WHERE owner_token = $1 AND generation = $2)`,
			string(c.Token), generation).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if err := tx.QueryRow(ctx, `SELECT capacity_reserved FROM standing_owner_attempts WHERE owner_token = $1 AND generation = $2`,
				string(c.Token), generation).Scan(&capacityReserved); err != nil {
				return err
			}
			if capacityReserved {
				return nil
			}
		}
		var running int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT token FROM claims WHERE project = $1 AND role = $2 AND token <> $3
				AND state IN ('launching', 'shim_connected', 'registered', 'ready', 'working', 'idle', 'launch_uncertain')
			UNION
			SELECT a.owner_token FROM standing_owner_attempts a JOIN claims owner ON owner.token = a.owner_token
				WHERE owner.project = $1 AND owner.role = $2 AND owner.token <> $3
					AND owner.state IN ('queued', 'suspended', 'failed') AND a.capacity_reserved
					AND a.generation = owner.generation + 1
		) running`, c.Project, string(claim.RoleOwner), string(c.Token)).Scan(&running); err != nil {
			return err
		}
		if running >= maxRunning {
			return fmt.Errorf("standing owner %s refused: max_running %d is full", c.Token, maxRunning)
		}
		var reserved float64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(coalesce(a.actual_usd, a.reserved_usd)), 0)
			FROM standing_owner_attempts a JOIN claims owner ON owner.token = a.owner_token
			WHERE owner.project = $1`, c.Project).Scan(&reserved); err != nil {
			return err
		}
		if !exists && reserved+cost > budget {
			return fmt.Errorf("standing owner %s refused: budget_usd %.2f would be exceeded by %.2f", c.Token, budget, reserved+cost)
		}
		if exists {
			_, err := tx.Exec(ctx, `UPDATE standing_owner_attempts SET capacity_reserved = true
				WHERE owner_token = $1 AND generation = $2 AND capacity_reserved = false`, string(c.Token), generation)
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO standing_owner_attempts (owner_token, generation, reserved_usd) VALUES ($1, $2, $3)`, string(c.Token), generation, cost)
		return err
	})
}

// ReleaseStandingOwnerPendingCapacity releases only an unresolved pre-launch capacity reservation.
// Its monetary reservation stays in the attempt history and remains part of budget accounting.
func (s *Store) ReleaseStandingOwnerPendingCapacity(ctx context.Context, c supervise.Claim) error {
	if !claim.IsStandingOwner(c.Role, c.Issue, c.Tree) {
		return nil
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE standing_owner_attempts SET capacity_reserved = false
			WHERE owner_token = $1 AND generation = $2 AND capacity_reserved`,
			string(c.Token), int64(c.Generation+1))
		return err
	})
}
