package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/orchestration/ledger"
)

type ledgerQueue struct {
	store *ledger.Store
	repo  string
}

func (q ledgerQueue) HasWork(ctx context.Context, owner string) (bool, error) {
	return q.store.HasOpenWork(ctx, q.repo, owner)
}

func openStandingOwnerQueue(ctx context.Context, cfg *config.StandingOwners) (ledgerQueue, error) {
	if cfg == nil {
		return ledgerQueue{}, nil
	}
	dsn, err := config.ReadPrivateSecretPointer("standing_owners.ledger_dsn_file", cfg.LedgerDSNFile)
	if err != nil {
		return ledgerQueue{}, err
	}
	st, err := ledger.Open(ctx, dsn)
	if err != nil {
		return ledgerQueue{}, fmt.Errorf("open standing-owner ledger: %w", err)
	}
	return ledgerQueue{store: st, repo: cfg.Repository}, nil
}

func ownerConfigs(cfg *config.StandingOwners, project string) (map[claim.Token]config.StandingOwner, error) {
	owners := make(map[claim.Token]config.StandingOwner)
	if cfg == nil {
		return owners, nil
	}
	for _, owner := range cfg.Owners {
		token, err := claim.OwnerToken(project, owner.Name)
		if err != nil {
			return nil, err
		}
		owners[token] = owner
	}
	return owners, nil
}

func ensureStandingOwners(ctx context.Context, st *store.Store, project string, configured map[claim.Token]config.StandingOwner, claims []supervise.Claim) ([]supervise.Claim, error) {
	have := make(map[claim.Token]bool, len(claims))
	for _, c := range claims {
		have[c.Token] = true
	}
	for token := range configured {
		if have[token] {
			continue
		}
		c, err := st.AdmitClaim(ctx, supervise.Claim{Token: token, Project: project, Role: claim.RoleOwner, State: supervise.StateQueued})
		if err != nil {
			return nil, fmt.Errorf("record standing owner %s: %w", token, err)
		}
		claims = append(claims, c)
	}
	return claims, nil
}

func ownerLaunchCheck(st *store.Store, cfg *config.StandingOwners, owners map[claim.Token]config.StandingOwner, next func(context.Context, supervise.Claim) error) func(context.Context, supervise.Claim) error {
	if cfg == nil {
		return next
	}
	return func(ctx context.Context, c supervise.Claim) error {
		if next != nil {
			if err := next(ctx, c); err != nil {
				return err
			}
		}
		owner, ok := owners[c.Token]
		if !ok {
			return nil
		}
		return st.ReserveStandingOwnerAttempt(ctx, c, cfg.MaxRunning, cfg.BudgetUSD, owner.MaxCostUSD)
	}
}

func (s *supervision) wakeStandingOwners(ctx context.Context) {
	if s.workQueue == nil {
		return
	}
	for _, machine := range s.supervisor.supervised() {
		c := machine.View().Claim
		if !claim.IsStandingOwner(c.Role, c.Issue, c.Tree) || c.State != supervise.StateQueued {
			continue
		}
		owner, ok := c.Token.OwnerName(c.Project)
		if !ok {
			s.log.Error("standing owner has invalid token", "claim", c.Token)
			continue
		}
		hasWork, err := s.workQueue.HasWork(ctx, owner)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("read standing-owner work", "owner", owner, "error", err)
			}
			continue
		}
		if hasWork {
			s.supervisor.post(c.Token, supervise.RequestSpawn{Claim: c.Token})
		}
	}
}

func closeStandingOwnerQueue(queue *ledgerQueue, log *slog.Logger) {
	if queue != nil && queue.store != nil {
		queue.store.Close()
		log.Debug("closed standing-owner ledger")
	}
}
