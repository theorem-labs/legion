package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// ledgerQueue reconnects lazily: ledger availability gates owner admission only, never daemon
// boot or the six workflow roles.
type ledgerQueue struct {
	dsn, repo string

	mu       sync.Mutex
	store    *ledger.Store
	nextTry  time.Time
	failures int
}

func (q *ledgerQueue) HasWork(ctx context.Context, owner string) (bool, error) {
	store, err := q.open(ctx)
	if err != nil {
		return false, err
	}
	hasWork, err := store.HasOpenWork(ctx, q.repo, owner)
	if err != nil {
		q.failed(store)
		return false, err
	}
	return hasWork, nil
}

func (q *ledgerQueue) WorkRevision(ctx context.Context, owner string) (int64, error) {
	store, err := q.open(ctx)
	if err != nil {
		return 0, err
	}
	revision, err := store.OwnerWorkRevision(ctx, q.repo, owner)
	if err != nil {
		q.failed(store)
		return 0, err
	}
	return revision, nil
}

func (q *ledgerQueue) open(ctx context.Context) (*ledger.Store, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.store != nil {
		return q.store, nil
	}
	if until := q.nextTry; !until.IsZero() && time.Now().Before(until) {
		return nil, fmt.Errorf("standing-owner ledger reconnect waits until %s", until.UTC().Format(time.RFC3339))
	}
	store, err := ledger.Open(ctx, q.dsn)
	if err == nil {
		q.store, q.failures, q.nextTry = store, 0, time.Time{}
		return store, nil
	}
	q.failures++
	delay := time.Second << min(q.failures-1, 5)
	q.nextTry = time.Now().Add(delay)
	return nil, fmt.Errorf("open standing-owner ledger: %w", err)
}

func (q *ledgerQueue) failed(store *ledger.Store) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.store != store {
		return
	}
	store.Close()
	q.store = nil
	q.failures++
	q.nextTry = time.Now().Add(time.Second << min(q.failures-1, 5))
}

func openStandingOwnerQueue(_ context.Context, cfg *config.StandingOwners) (*ledgerQueue, error) {
	if cfg == nil {
		return nil, nil
	}
	dsn, err := config.ReadPrivateSecretPointer("standing_owners.ledger_dsn_file", cfg.LedgerDSNFile)
	if err != nil {
		return nil, err
	}
	return &ledgerQueue{dsn: dsn, repo: cfg.Repository}, nil
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

func ownerLaunchCheck(st *store.Store, cfg *config.StandingOwners, owners map[claim.Token]config.StandingOwner,
	queue supervise.WorkQueue, next func(context.Context, supervise.Claim) error) func(context.Context, supervise.Claim) error {
	if cfg == nil {
		return next
	}
	return func(ctx context.Context, c supervise.Claim) error {
		owner, configured := owners[c.Token]
		if configured {
			name, ok := c.Token.OwnerName(c.Project)
			if !ok {
				return fmt.Errorf("standing owner %s has an invalid token", c.Token)
			}
			if queue == nil {
				return supervise.ErrStandingOwnerWorkUnavailable
			}
			hasWork, err := queue.HasWork(ctx, name)
			if err != nil {
				return fmt.Errorf("%w: %v", supervise.ErrStandingOwnerWorkUnavailable, err)
			}
			if !hasWork {
				if err := st.ReleaseStandingOwnerPendingCapacity(ctx, c); err != nil {
					return fmt.Errorf("release standing-owner pending capacity: %w", err)
				}
				return supervise.ErrStandingOwnerNoWork
			}
		}
		if next != nil {
			if err := next(ctx, c); err != nil {
				return err
			}
		}
		if !configured {
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
		if !claim.IsStandingOwner(c.Role, c.Issue, c.Tree) ||
			(c.State != supervise.StateQueued && c.State != supervise.StateFailed) {
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
		if !hasWork {
			if err := s.supervisor.store.ReleaseStandingOwnerPendingCapacity(ctx, c); err != nil && ctx.Err() == nil {
				s.log.Error("release standing-owner pending capacity", "owner", owner, "error", err)
			}
			continue
		}
		if c.State == supervise.StateQueued {
			s.supervisor.post(c.Token, supervise.RequestSpawn{Claim: c.Token})
			continue
		}
		revision, err := s.workQueue.WorkRevision(ctx, owner)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("read standing-owner work revision", "owner", owner, "error", err)
			}
			continue
		}
		if err := machine.RetryStandingOwnerForWorkRevision(ctx, revision); err != nil {
			s.supervisor.decisionFailed(c.Token, "work revision retry", err, "supervise: retry standing owner after work changed")
		}
	}
}

func closeStandingOwnerQueue(queue *ledgerQueue, log *slog.Logger) {
	if queue == nil {
		return
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.store != nil {
		queue.store.Close()
		queue.store = nil
		log.Debug("closed standing-owner ledger")
	}
}
