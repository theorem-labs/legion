package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// OwnerRefusalRecorder records, durably, that the daemon will not start an owner at a work
// revision: the ledger's RecordOwnerRefused (INFRA-135 task L6), idempotent per owner and revision,
// which appends `owner.refused` once.
type OwnerRefusalRecorder interface {
	RecordOwnerRefused(ctx context.Context, repo, owner string, revision int64, reason string) error
}

// ownerRefusals is every owner the admission loop refused while its work is unchanged, keyed by
// owner, for the operator API (RefusedOwners); the admission loop writes it and the API reads it.
type ownerRefusals struct {
	mu      sync.Mutex
	byOwner map[string]api.OwnerRefusal
}

// list is every refusal, in owner order.
func (r *ownerRefusals) list() []api.OwnerRefusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	refused := make([]api.OwnerRefusal, 0, len(r.byOwner))
	for _, refusal := range r.byOwner {
		refused = append(refused, refusal)
	}
	slices.SortFunc(refused, func(a, b api.OwnerRefusal) int { return strings.Compare(a.Owner, b.Owner) })
	return refused
}

func (r *ownerRefusals) get(owner string) (api.OwnerRefusal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	refusal, ok := r.byOwner[owner]
	return refusal, ok
}

func (r *ownerRefusals) put(refusal api.OwnerRefusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byOwner[refusal.Owner] = refusal
}

// forget drops the refusal of every owner not in open: its work is done, so the refusal is over.
func (r *ownerRefusals) forget(open map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for owner := range r.byOwner {
		if !open[owner] {
			delete(r.byOwner, owner)
		}
	}
}

// ledgerQueue is the daemon's view of the standing owners' ledger repository. It connects lazily
// and reconnects with backoff: ledger availability gates owner admission only, never daemon boot
// or the six workflow roles. Every call — the connection, its schema check and the read or write —
// runs under its own deadline, so a ledger that never answers holds only the owner that asked.
//
// It also keeps which owners' leases the ledger must free (an owner's leases are taken under its
// key, so a stopped process's leases stay held until the ledger is told): stopped is each owner
// whose process supervision let go (holderStopped), clean each owner the ledger holds no lease of
// from an earlier process.
type ledgerQueue struct {
	dsn, repo string
	timeout   time.Duration
	log       *slog.Logger

	mu       sync.Mutex
	store    *ledger.Store
	nextTry  time.Time
	failures int

	// call serializes the releases, so each stop is released once; state guards stopped, clean and
	// stops, and is never held across a ledger call, so holderStopped never waits on one.
	call    sync.Mutex
	state   sync.Mutex
	stopped map[string]stoppedHolder
	clean   map[string]bool
	stops   uint64
	// released wakes the admission loop to free a stopped holder's leases (admitStandingOwners).
	released chan struct{}
}

// stoppedHolder is why an owner's holder stopped, and which stop it was.
type stoppedHolder struct {
	reason string
	stop   uint64
}

func openStandingOwnerQueue(cfg *config.StandingOwners, log *slog.Logger) (*ledgerQueue, error) {
	if cfg == nil {
		return nil, nil
	}
	dsn, err := config.ReadPrivateSecretPointer("standing_owners.ledger_dsn_file", cfg.LedgerDSNFile)
	if err != nil {
		return nil, err
	}
	return &ledgerQueue{dsn: dsn, repo: cfg.Repository, timeout: cfg.LedgerTimeout, log: log,
		stopped: map[string]stoppedHolder{}, clean: map[string]bool{}, released: make(chan struct{}, 1)}, nil
}

// with runs f on the ledger under the call deadline, dropping the connection f's error may have
// broken.
func (q *ledgerQueue) with(ctx context.Context, f func(context.Context, *ledger.Store) error) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	st, err := q.open(ctx)
	if err != nil {
		return err
	}
	if err := f(ctx, st); err != nil {
		q.failed(st)
		return err
	}
	return nil
}

func (q *ledgerQueue) HasWork(ctx context.Context, owner string) (bool, error) {
	var has bool
	err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		has, err = st.HasOpenWork(ctx, q.repo, owner)
		return err
	})
	return has, err
}

func (q *ledgerQueue) WorkRevision(ctx context.Context, owner string) (int64, error) {
	var revision int64
	err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		revision, err = st.OwnerWorkRevision(ctx, q.repo, owner)
		return err
	})
	return revision, err
}

func (q *ledgerQueue) openOwners(ctx context.Context) ([]string, error) {
	var owners []string
	err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		owners, err = st.OpenOwners(ctx, q.repo)
		return err
	})
	return owners, err
}

func (q *ledgerQueue) ownerWakes(ctx context.Context, cursor int64) ([]string, int64, error) {
	var owners []string
	last := cursor
	err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		owners, last, err = st.OwnerWakes(ctx, q.repo, cursor)
		return err
	})
	return owners, last, err
}

func (q *ledgerQueue) latestEventID(ctx context.Context) (int64, error) {
	var id int64
	err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		id, err = st.LatestEventID(ctx, q.repo)
		return err
	})
	return id, err
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
	st, err := ledger.Open(ctx, q.dsn)
	if err == nil {
		q.store, q.failures, q.nextTry = st, 0, time.Time{}
		return st, nil
	}
	q.failures++
	q.nextTry = time.Now().Add(time.Second << min(q.failures-1, 5))
	return nil, fmt.Errorf("open standing-owner ledger: %w", err)
}

func (q *ledgerQueue) failed(st *ledger.Store) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.store != st {
		return
	}
	st.Close()
	q.store = nil
	q.failures++
	q.nextTry = time.Now().Add(time.Second << min(q.failures-1, 5))
}

// holderStopped records that supervision let an owner's process go (supervise.Deps.HolderStopped)
// and wakes the admission loop to free its leases. It takes no lock a ledger call holds, so the
// machine that calls it under its own lock never waits on the ledger.
func (q *ledgerQueue) holderStopped(owner, reason string) {
	q.state.Lock()
	q.stops++
	q.stopped[owner] = stoppedHolder{reason: reason, stop: q.stops}
	q.clean[owner] = false
	q.state.Unlock()
	select {
	case q.released <- struct{}{}:
	default:
	}
}

// releaseStopped frees the leases of every owner whose holder stopped; an owner the ledger did not
// answer for is logged and stays recorded, for the next event or sweep.
func (q *ledgerQueue) releaseStopped(ctx context.Context) {
	q.state.Lock()
	owners := make([]string, 0, len(q.stopped))
	for owner := range q.stopped {
		owners = append(owners, owner)
	}
	q.state.Unlock()
	for _, owner := range owners {
		if err := q.release(ctx, owner, true); err != nil && ctx.Err() == nil {
			q.log.Warn("standing owner: the ledger did not free the stopped holder's leases; retrying on the next event or sweep",
				"owner", owner, "error", err)
		}
	}
}

// release frees owner's leases in the ledger: those of a holder recorded stopped, and — unless
// onlyStopped — any an earlier process left, which a reaped orphan or a restart can leave held.
// Releases run one at a time, and each stop is released once.
func (q *ledgerQueue) release(ctx context.Context, owner string, onlyStopped bool) error {
	q.call.Lock()
	defer q.call.Unlock()
	q.state.Lock()
	stop, isStopped := q.stopped[owner]
	needed := isStopped || (!onlyStopped && !q.clean[owner])
	q.state.Unlock()
	if !needed {
		return nil
	}
	reason := stop.reason
	if !isStopped {
		reason = "freeing any lease an earlier process of the owner left, before its launch"
	}
	var freed []ledger.Item
	if err := q.with(ctx, func(ctx context.Context, st *ledger.Store) (err error) {
		freed, err = st.ReleaseHolder(ctx, q.repo, owner, reason)
		return err
	}); err != nil {
		return err
	}
	q.state.Lock()
	if current, still := q.stopped[owner]; still && current.stop == stop.stop {
		delete(q.stopped, owner)
	}
	q.clean[owner] = true
	q.state.Unlock()
	q.log.Info("standing owner: the ledger freed the holder's leases", "owner", owner, "freed", len(freed), "reason", reason)
	return nil
}

// launching records that a process of owner is about to start, so its leases are no longer an
// earlier process's to free.
func (q *ledgerQueue) launching(owner string) {
	q.state.Lock()
	q.clean[owner] = false
	q.state.Unlock()
}

// listen holds a connection listening on the ledger's event channel and signals notes for every
// notification of this queue's repository, reconnecting with backoff until ctx ends. A fresh
// connection signals once, for whatever arrived while it was down.
func (q *ledgerQueue) listen(ctx context.Context, notes chan<- struct{}) {
	signal := func() {
		select {
		case notes <- struct{}{}:
		default:
		}
	}
	for failures := 0; ctx.Err() == nil; {
		connecting, cancel := context.WithTimeout(ctx, q.timeout)
		listener, err := ledger.Listen(connecting, q.dsn)
		cancel()
		if err != nil {
			failures++
			if ctx.Err() == nil {
				q.log.Warn("standing owner: could not listen for ledger events; the sweep admits owners meanwhile", "error", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second << min(failures-1, 5)):
			}
			continue
		}
		failures = 0
		q.log.Info("standing owner: listening for ledger events", "repository", q.repo)
		signal()
		for {
			repo, err := listener.Wait(ctx)
			if err != nil {
				break
			}
			if repo == q.repo {
				signal()
			}
		}
		closing, cancel := context.WithTimeout(context.Background(), q.timeout)
		_ = listener.Close(closing)
		cancel()
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

// ownerLaunches is how the daemon launches the owner of a ledger key: a named owner by its own
// entry, a graph node by its kind's template.
func ownerLaunches(cfg *config.StandingOwners) func(string) (config.OwnerTemplate, bool) {
	if cfg == nil {
		return func(string) (config.OwnerTemplate, bool) { return config.OwnerTemplate{}, false }
	}
	named := make(map[string]config.OwnerTemplate, len(cfg.Owners))
	for _, owner := range cfg.Owners {
		named[owner.Name] = config.OwnerTemplate{PromptFile: owner.PromptFile, Harness: owner.Harness, MaxCostUSD: owner.MaxCostUSD}
	}
	return func(key string) (config.OwnerTemplate, bool) {
		if launch, ok := named[key]; ok {
			return launch, true
		}
		return cfg.TemplateFor(key)
	}
}

// ownerLaunchCheck is every standing owner's admission, before its launch is recorded: the
// leases an earlier process of it left are freed, its queue must have work, and the attempt must
// fit max_running and the dollar budget (store.ReserveStandingOwnerAttempt). An unreachable ledger
// leaves the owner asleep (supervise.ErrStandingOwnerWorkUnavailable).
func ownerLaunchCheck(st *store.Store, cfg *config.StandingOwners, launches func(string) (config.OwnerTemplate, bool),
	queue *ledgerQueue, next func(context.Context, supervise.Claim) error) func(context.Context, supervise.Claim) error {
	if cfg == nil {
		return next
	}
	return func(ctx context.Context, c supervise.Claim) error {
		key, isOwner := c.StandingOwnerKey()
		if !isOwner {
			if next != nil {
				return next(ctx, c)
			}
			return nil
		}
		launch, ok := launches(key)
		if !ok {
			return fmt.Errorf("standing owner %s has no named owner and no template for its kind", key)
		}
		if queue == nil {
			return supervise.ErrStandingOwnerWorkUnavailable
		}
		if err := queue.release(ctx, key, false); err != nil {
			return fmt.Errorf("%w: free the leases an earlier process of %s left: %v", supervise.ErrStandingOwnerWorkUnavailable, key, err)
		}
		hasWork, err := queue.HasWork(ctx, key)
		if err != nil {
			return fmt.Errorf("%w: %v", supervise.ErrStandingOwnerWorkUnavailable, err)
		}
		if !hasWork {
			if err := st.ReleaseStandingOwnerPendingCapacity(ctx, c); err != nil {
				return fmt.Errorf("release standing-owner pending capacity: %w", err)
			}
			return supervise.ErrStandingOwnerNoWork
		}
		if next != nil {
			if err := next(ctx, c); err != nil {
				return err
			}
		}
		if err := st.ReserveStandingOwnerAttempt(ctx, c, cfg.MaxRunning, cfg.BudgetUSD, launch.MaxCostUSD); err != nil {
			return err
		}
		queue.launching(key)
		return nil
	}
}

// admission is the state of the one goroutine that wakes standing owners.
type admission struct {
	s        *supervision
	queue    *ledgerQueue
	launches func(string) (config.OwnerTemplate, bool)
	// neverStart is standing_owners.never_start: owners admission neither starts nor refuses.
	neverStart map[string]bool
	// refusals is each owner refused at its current work revision, which the operator API lists;
	// recorder records each one durably in the ledger, nil for a daemon that records none.
	refusals *ownerRefusals
	recorder OwnerRefusalRecorder
	// cursor is the newest ledger event read; known is whether it was read yet.
	cursor int64
	known  bool
	// asked is each owner an event or pass asked to start that has not started yet; the sweep logs
	// an owner it starts that nothing asked for, because that is a notification lost.
	asked map[string]bool
}

// admitStandingOwners wakes standing owners on their own goroutine, beside the serving API, so
// boot, health and the software roles never wait on the ledger. It wakes exactly the owners the
// ledger's events name as their notifications arrive (LISTEN on ledger.EventsChannel), frees a
// stopped holder's leases and then admits whoever was waiting on capacity, and runs a slow
// reconciliation sweep (standing_owners.reconcile_seconds) that starts any owner with open work
// that nothing woke.
func (s *supervision) admitStandingOwners(ctx context.Context) {
	if s.ledger == nil {
		return
	}
	notes := make(chan struct{}, 1)
	var listening sync.WaitGroup
	listening.Add(1)
	go func() {
		defer listening.Done()
		s.ledger.listen(ctx, notes)
	}()
	defer listening.Wait()
	neverStart := map[string]bool{}
	for _, owner := range s.cfg.StandingOwners.NeverStart {
		neverStart[owner] = true
	}
	a := &admission{s: s, queue: s.ledger, launches: ownerLaunches(s.cfg.StandingOwners), neverStart: neverStart,
		refusals: s.refused, recorder: s.plan.ownerRefusals, asked: map[string]bool{}}
	sweep := time.NewTicker(s.cfg.StandingOwners.Reconcile)
	defer sweep.Stop()
	a.begin(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-notes:
			s.ledger.releaseStopped(ctx)
			a.events(ctx)
		case <-s.ledger.released:
			s.ledger.releaseStopped(ctx)
			a.pass(ctx, "release")
		case <-sweep.C:
			s.ledger.releaseStopped(ctx)
			if !a.known {
				a.begin(ctx)
				continue
			}
			a.pass(ctx, "sweep")
		}
	}
}

// begin reads the event cursor and then starts every owner with open work, so no event between
// the two is missed.
func (a *admission) begin(ctx context.Context) {
	cursor, err := a.queue.latestEventID(ctx)
	if err != nil {
		a.logLedger(ctx, "read the ledger's event cursor", err)
		return
	}
	a.cursor, a.known = cursor, true
	a.pass(ctx, "boot")
	a.s.log.Info("standing owner: admission open", "repository", a.queue.repo, "cursor", cursor)
}

// events wakes the owners of the items the events after the cursor reopened.
func (a *admission) events(ctx context.Context) {
	if !a.known {
		a.begin(ctx)
		return
	}
	owners, last, err := a.queue.ownerWakes(ctx, a.cursor)
	if err != nil {
		a.logLedger(ctx, "read the ledger's events", err)
		return
	}
	a.cursor = last
	for _, owner := range owners {
		a.wake(ctx, owner, "event")
	}
}

// pass starts every owner with open work, gives up the capacity a queued owner with none reserved,
// and ends the refusal of an owner whose work is done.
func (a *admission) pass(ctx context.Context, why string) {
	owners, err := a.queue.openOwners(ctx)
	if err != nil {
		a.logLedger(ctx, "read the owners with open work", err)
		return
	}
	open := make(map[string]bool, len(owners))
	for _, owner := range owners {
		open[owner] = true
		a.wake(ctx, owner, why)
	}
	a.refusals.forget(open)
	for _, m := range a.s.supervisor.supervised() {
		c := m.View().Claim
		key, isOwner := c.StandingOwnerKey()
		if !isOwner || open[key] {
			continue
		}
		delete(a.asked, key)
		if c.State == supervise.StateQueued {
			if err := a.s.supervisor.store.ReleaseStandingOwnerPendingCapacity(ctx, c); err != nil && ctx.Err() == nil {
				a.s.log.Error("release standing-owner pending capacity", "owner", key, "error", err)
			}
		}
	}
}

// wake starts the owner of key: it supervises a new claim for an owner the ledger names first, a
// queued owner is asked to spawn, and an owner that exhausted its retries is retried once its work
// changed. An owner on never_start is left alone, and one the daemon cannot launch is refused.
func (a *admission) wake(ctx context.Context, key, why string) {
	if ctx.Err() != nil || a.neverStart[key] {
		return
	}
	token, err := claim.OwnerToken(a.s.plan.project, key)
	if err != nil {
		a.refuse(ctx, key, fmt.Sprintf("the owner key is not one a standing owner can hold: %v", err))
		return
	}
	m, supervised := a.s.supervisor.Machine(token)
	if !supervised {
		if _, ok := a.launches(key); !ok {
			kind, _, isNode := strings.Cut(key, ":")
			reason := fmt.Sprintf("no standing_owners.owners entry is named %s", key)
			if isNode {
				reason = fmt.Sprintf("standing_owners.templates has no template for node kind %s", kind)
			}
			a.refuse(ctx, key, reason)
			return
		}
		m, _, err = a.s.supervisor.Create(ctx, supervise.Claim{Token: token, Project: a.s.plan.project, Role: claim.RoleOwner,
			OwnerKey: key, State: supervise.StateQueued}, "")
		if err != nil {
			if ctx.Err() == nil {
				a.s.log.Error("standing owner: record the owner's claim", "owner", key, "error", err)
			}
			return
		}
	}
	c := m.View().Claim
	switch c.State {
	case supervise.StateQueued:
		if why == "sweep" && !a.asked[key] {
			a.s.log.Warn("standing owner: the reconciliation sweep started an owner no ledger event woke; a notification was lost", "owner", key)
		}
		a.asked[key] = true
		a.s.supervisor.post(token, supervise.RequestSpawn{Claim: token})
	case supervise.StateFailed:
		revision, err := a.queue.WorkRevision(ctx, key)
		if err != nil {
			a.logLedger(ctx, "read the owner's work revision", err)
			return
		}
		if err := m.RetryStandingOwnerForWorkRevision(ctx, revision); err != nil {
			a.s.supervisor.decisionFailed(token, "work revision retry", err, "supervise: retry standing owner after work changed")
		}
	default:
		delete(a.asked, key)
	}
}

// refuse records that the daemon will not start owner while its work stays at the current
// revision: durably in the ledger (recorder), once per revision, and in the operator API's list.
// A refusal the ledger did not take is asked again at the next event or sweep.
func (a *admission) refuse(ctx context.Context, owner, reason string) {
	revision, err := a.queue.WorkRevision(ctx, owner)
	if err != nil {
		a.logLedger(ctx, "read a refused owner's work revision", err)
		return
	}
	prior, refusedBefore := a.refusals.get(owner)
	if refusedBefore && prior.Revision == revision && (prior.Recorded || a.recorder == nil) {
		return
	}
	refusal := api.OwnerRefusal{Repository: a.queue.repo, Owner: owner, Revision: revision, Reason: reason, RefusedAt: time.Now().UTC()}
	if refusedBefore && prior.Revision == revision {
		refusal.RefusedAt = prior.RefusedAt
	} else {
		a.s.log.Warn("standing owner: refused; it starts only once its configuration can launch it", "owner", owner,
			"revision", revision, "reason", reason)
	}
	if a.recorder != nil {
		if err := a.recorder.RecordOwnerRefused(ctx, a.queue.repo, owner, revision, reason); err != nil {
			a.logLedger(ctx, "record a refused owner", err)
		} else {
			refusal.Recorded = true
		}
	}
	a.refusals.put(refusal)
}

func (a *admission) logLedger(ctx context.Context, what string, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	a.s.log.Warn("standing owner: "+what+"; owner admission waits for the ledger", "error", err)
}
