package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func openOwnerLedger(t *testing.T, dsn string) *ledger.Store {
	t.Helper()
	queue, err := ledger.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	return queue
}

func enqueueOwnerWork(t *testing.T, queue *ledger.Store, repo string) {
	t.Helper()
	if err := queue.Enqueue(context.Background(), repo, "item", "owner-a"); err != nil {
		t.Fatal(err)
	}
}

func standingOwnerConfig(t *testing.T, cfg *config.Config, queue *ledger.Store, repo string) {
	t.Helper()
	if _, err := queue.EnsureRepo(context.Background(), repo, 0); err != nil {
		t.Fatalf("ensure ledger repository: %v", err)
	}
	dsnFile := filepath.Join(t.TempDir(), "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte(cfg.PostgresDSN+"\n"), 0o600); err != nil {
		t.Fatalf("write ledger dsn: %v", err)
	}
	prompt := filepath.Join(t.TempDir(), "owner.md")
	if err := os.WriteFile(prompt, []byte("Wait for work."), 0o600); err != nil {
		t.Fatalf("write owner prompt: %v", err)
	}
	cfg.StandingOwners = &config.StandingOwners{LedgerDSNFile: dsnFile, Repository: repo, MaxRunning: 1, BudgetUSD: 10,
		LedgerTimeout: 5 * time.Second, Reconcile: time.Minute,
		Owners: []config.StandingOwner{{Name: "owner-a", PromptFile: prompt, Harness: "omp", MaxCostUSD: 1}}}
}

func standingOwnerClaim(t *testing.T, cfg config.Config, state supervise.ClaimState, locator *runtime.Locator) supervise.Claim {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.OwnerToken(project, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	return supervise.Claim{Token: token, Project: project, Role: claim.RoleOwner, State: state, Locator: locator}
}

func putStandingOwner(t *testing.T, cfg config.Config, owner supervise.Claim) {
	t.Helper()
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdmitClaim(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
}

type beforeSpawnRuntime struct {
	runtime.Runtime
	entered chan struct{}
	once    sync.Once
}

func (r *beforeSpawnRuntime) Spawn(ctx context.Context, _ runtime.SpawnSpec) (runtime.Locator, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return runtime.Locator{}, ctx.Err()
}

type orphanReapingRuntime struct {
	runtime.Runtime
	orphan claim.Token
	reaped atomic.Bool
}

func (r *orphanReapingRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	if grace == orphanGrace {
		for _, entry := range known {
			if entry.Claim == r.orphan {
				return r.Runtime.ReconcileOrphans(ctx, known, grace)
			}
		}
		r.reaped.Store(true)
	}
	return r.Runtime.ReconcileOrphans(ctx, known, grace)
}

func TestStandingOwnerStartsWhenLedgerItemIsEnqueuedAndSleepsWhenDone(t *testing.T) {
	cfg := testConfig(t)
	queue, err := ledger.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	repo := "owner-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	if err := queue.Enqueue(context.Background(), repo, "item", "owner-a"); err != nil {
		t.Fatal(err)
	}

	rt := fake.NewRuntime()
	startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	testwait.Eventually(t, "standing owner launch", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
}

// A daemon stopped after it persisted launching but before Spawn returns leaves no unrecorded
// process. The next daemon repairs that launch and starts generation two exactly once.
func TestFakeSupervisorRepairsOwnerLaunchRecordedBeforeSpawn(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "record-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	first := &beforeSpawnRuntime{Runtime: fake.NewRuntime(), entered: make(chan struct{})}
	o := fakeRuntime(first, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	select {
	case <-first.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("queued owner did not reach Spawn")
	}
	d.stop()
	stored := storedClaim(t, cfg, owner.Token)
	if stored.State != supervise.StateLaunching || stored.Locator != nil {
		t.Fatalf("interrupted launch = %+v, want launching without a locator", stored)
	}
	rebindHeldPorts(t, &cfg)
	second := fake.NewRuntime()
	restarted := startDaemon(t, cfg, fakeRuntime(second, &built{}))
	testwait.Eventually(t, "recovered launch", func() bool {
		c := restarted.claim(owner.Token)
		return c.State == string(supervise.StateLaunching) && c.Locator != nil
	})
	if spawns := second.CallsOf("Spawn"); len(spawns) != 1 || spawns[0].Spec.Generation != 2 {
		t.Fatalf("recovery spawns = %+v, want one generation-two start", spawns)
	}
}

func TestFakeOwnerRestartDoesNotSpawnRecordedLiveOwner(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "adopt-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateLaunching, nil)
	seed := fake.NewRuntime()
	locator, err := seed.Spawn(context.Background(), runtime.SpawnSpec{Claim: owner.Token})
	if err != nil {
		t.Fatal(err)
	}
	owner.Locator = &locator
	putStandingOwner(t, cfg, owner)
	enqueueOwnerWork(t, queue, repo)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	startDaemon(t, cfg, o)
	time.Sleep(3 * o.orphanSweep)
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("restart spawned live owner %d times: %+v", len(spawns), spawns)
	}
}

func TestFakeOwnerGoneWithEmptyQueueSleeps(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "restart-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	firstRuntime := fake.NewRuntime()
	o := fakeRuntime(firstRuntime, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	testwait.Eventually(t, "first owner start", func() bool { return len(firstRuntime.CallsOf("Spawn")) == 1 })
	before := d.claim(owner.Token)
	if before.Locator == nil {
		t.Fatal("running owner has no locator")
	}
	lease, err := queue.Claim(context.Background(), repo, "item", "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(context.Background(), repo, "item", "worker", lease.Generation, "done"); err != nil {
		t.Fatal(err)
	}
	d.stop()
	rebindHeldPorts(t, &cfg)
	second := fake.NewRuntime()
	o = fakeRuntime(second, &built{})
	o.orphanSweep = 20 * time.Millisecond
	restarted := startDaemon(t, cfg, o)
	second.Emit(runtime.Observation{Locator: *before.Locator, Kind: runtime.Gone})
	testwait.Eventually(t, "empty queue sleep", func() bool {
		c := restarted.claim(owner.Token)
		return c.State == string(supervise.StateQueued) && c.Locator == nil
	})
	time.Sleep(3 * o.orphanSweep)
	if spawns := second.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("empty queue restarted owner %d times: %+v", len(spawns), spawns)
	}
	if err := queue.Enqueue(context.Background(), repo, "item-b", "owner-a"); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "queue wake", func() bool { return len(second.CallsOf("Spawn")) == 1 })
	if got := second.CallsOf("Spawn")[0].Spec.Generation; got != 2 {
		t.Fatalf("queue wake generation = %d, want 2", got)
	}
}

func TestFakeOrphanSweepSeesUnrecordedOwnerToken(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "orphan-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	rt := &orphanReapingRuntime{
		Runtime: fake.NewRuntime(),
		orphan:  claim.Token("legion-" + owner.Project + "-orphan"),
	}
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	startDaemon(t, cfg, o)
	testwait.Eventually(t, "orphan sweep", func() bool { return rt.reaped.Load() })
}

func TestStandingOwnersCapacityRefusesAnExtraOwner(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "capacity-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	ownerB := cfg.StandingOwners.Owners[0]
	ownerB.Name = "owner-b"
	cfg.StandingOwners.Owners = append(cfg.StandingOwners.Owners, ownerB)
	enqueueOwnerWork(t, queue, repo)
	if err := queue.Enqueue(context.Background(), repo, "item-b", "owner-b"); err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	testwait.Eventually(t, "one capacity-admitted owner", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 1 {
		t.Fatalf("owner launches = %d, want one under max_running 1", got)
	}
}

func TestStandingOwnerBudgetRefusalSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "budget-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	cfg.StandingOwners.BudgetUSD = 0.5
	enqueueOwnerWork(t, queue, repo)
	rt := fake.NewRuntime()
	first := startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	time.Sleep(50 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("budget-refused owner launched %d times", got)
	}
	first.stop()
	rebindHeldPorts(t, &cfg)
	startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	time.Sleep(50 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("owner launch after restart bypassed budget: %d", got)
	}
}

func readyStandingOwner(t *testing.T, d *daemon, built *built, rt *fake.Runtime, token claim.Token) *shim {
	t.Helper()
	var launch runtime.SpawnSpec
	for _, call := range rt.Calls() {
		if call.Method == "Spawn" || call.Method == "Resume" {
			launch = call.Spec
		}
	}
	if launch.BootToken == "" {
		t.Fatal("standing owner has no launch boot token")
	}
	connection := dialShim(t, built.address, launch.BootToken)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "owner-session", OmpSessionFile: "/sessions/owner.jsonl",
		AgentID: "owner-agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register standing owner = %d: %s", status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatal(err)
	}
	status, body = d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "owner-session", Secret: registered.Secret, Generation: launch.Generation,
	}, false)
	if status != http.StatusNoContent {
		t.Fatalf("ready standing owner = %d: %s", status, body)
	}
	testwait.Eventually(t, "standing owner ready", func() bool { return d.claim(token).State == string(supervise.StateReady) })
	return connection
}

func TestStandingOwnerRepeatedStopsWaitAfterRetryBudget(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 3
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "retries-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	rt := fake.NewRuntime()
	record := &built{}
	d := startDaemon(t, cfg, fakeRuntime(rt, record))
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.OwnerToken(project, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "initial owner launch", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
	connection := readyStandingOwner(t, d, record, rt, token)
	for death := 1; death <= cfg.LaunchFailureLimit; death++ {
		loc := d.claim(token).Locator
		if loc == nil {
			t.Fatalf("owner has no locator before stop %d", death)
		}
		rt.Emit(runtime.Observation{Locator: *loc, Kind: runtime.Gone, Detail: "stopped"})
		_ = connection.conn.Close()
		if death < cfg.LaunchFailureLimit {
			testwait.Eventually(t, "owner retry", func() bool {
				return len(rt.CallsOf("Spawn"))+len(rt.CallsOf("Resume")) == death+1
			})
			connection = readyStandingOwner(t, d, record, rt, token)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); got != cfg.LaunchFailureLimit {
		t.Fatalf("owner launches = %d, want %d before retry budget exhaustion", got, cfg.LaunchFailureLimit)
	}
}

func TestStandingOwnerLedgerOutageLeavesDaemonServing(t *testing.T) {
	cfg := testConfig(t)
	dsnFile := filepath.Join(t.TempDir(), "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte("postgres://postgres:test@127.0.0.1:1/postgres\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(t.TempDir(), "owner.md")
	if err := os.WriteFile(prompt, []byte("Wait for work."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.StandingOwners = &config.StandingOwners{
		LedgerDSNFile: dsnFile, Repository: "example/repository", MaxRunning: 1, BudgetUSD: 1, LedgerTimeout: 5 * time.Second, Reconcile: time.Minute,
		Owners: []config.StandingOwner{{Name: "owner-a", PromptFile: prompt, Harness: runtime.BackendOmp, MaxCostUSD: 1}},
	}
	rt := fake.NewRuntime()
	startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	time.Sleep(20 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("owners launched while their ledger is unreachable: %d", got)
	}
}

func TestStandingOwnersCountPendingSuspendedReservations(t *testing.T) {
	cfg := testConfig(t)
	a := standingOwnerClaim(t, cfg, supervise.StateSuspended, nil)
	b := a
	var err error
	b.Token, err = claim.OwnerToken(a.Project, "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	putStandingOwner(t, cfg, a)
	putStandingOwner(t, cfg, b)
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReserveStandingOwnerAttempt(context.Background(), a, 1, 10, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveStandingOwnerAttempt(context.Background(), b, 1, 10, 1); err == nil {
		t.Fatal("two suspended owners reserved max_running 1")
	}
}

func TestUnstartedOwnerReservationDoesNotStrandCapacity(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "abandoned-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	ownerB := cfg.StandingOwners.Owners[0]
	ownerB.Name = "owner-b"
	cfg.StandingOwners.Owners = append(cfg.StandingOwners.Owners, ownerB)
	a := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	putStandingOwner(t, cfg, a)
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveStandingOwnerAttempt(context.Background(), a, 1, 10, 1); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := queue.Enqueue(context.Background(), repo, "item-b", "owner-b"); err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	startDaemon(t, cfg, o)
	testwait.Eventually(t, "owner-b capacity after abandoned reservation", func() bool {
		return len(rt.CallsOf("Spawn")) == 1
	})
}

func TestSleepingOwnerDoesNotWakeOnRestartWithoutWork(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "sleeping-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	owner.Generation = 1
	putStandingOwner(t, cfg, owner)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	time.Sleep(3 * o.orphanSweep)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("sleeping owner launches %d times without work", got)
	}
	if got := d.claim(owner.Token); got.State != string(supervise.StateQueued) || got.Generation != 1 {
		t.Fatalf("sleeping owner = %+v, want queued generation one", got)
	}
}

func TestExhaustedOwnerRetriesOnlyAfterWorkRevisionChanges(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "revision-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	revision, err := queue.OwnerWorkRevision(context.Background(), repo, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	owner := standingOwnerClaim(t, cfg, supervise.StateFailed, nil)
	owner.Budgets.Deaths = cfg.LaunchFailureLimit
	owner.OwnerRetryRevision = revision
	putStandingOwner(t, cfg, owner)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	startDaemon(t, cfg, o)
	time.Sleep(3 * o.orphanSweep)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("same work retried exhausted owner %d times", got)
	}
	if err := queue.Enqueue(context.Background(), repo, "item-b", "owner-a"); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "changed work retry", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
}

func TestOperatorRetryClearsExhaustedStandingOwner(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "operator-retry-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	revision, err := queue.OwnerWorkRevision(context.Background(), repo, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	owner := standingOwnerClaim(t, cfg, supervise.StateFailed, nil)
	owner.Budgets.Deaths = cfg.LaunchFailureLimit
	owner.OwnerRetryRevision = revision
	putStandingOwner(t, cfg, owner)
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	time.Sleep(50 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("exhausted owner launched before an operator retry: %d", got)
	}
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(owner.Token)+"/retry", nil, true)
	if status != http.StatusOK {
		t.Fatalf("operator retry = %d: %s", status, body)
	}
	testwait.Eventually(t, "operator standing owner retry", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
}

func TestStandingOwnerPrebootReadsLedgerPointerAndPrompt(t *testing.T) {
	cfg := testConfig(t)
	dsnFile := filepath.Join(t.TempDir(), "ledger-dsn")
	prompt := filepath.Join(t.TempDir(), "owner.md")
	cfg.StandingOwners = &config.StandingOwners{
		LedgerDSNFile: dsnFile,
		Owners:        []config.StandingOwner{{Name: "owner-a", PromptFile: prompt}},
	}
	if err := readStandingOwnerFiles(cfg); err == nil {
		t.Fatal("preboot accepted missing standing-owner files")
	}
	if err := os.WriteFile(dsnFile, []byte("postgres://postgres:test@127.0.0.1:5432/postgres\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prompt, []byte("Wait for work."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readStandingOwnerFiles(cfg); err != nil {
		t.Fatalf("preboot read standing-owner files: %v", err)
	}
}

func TestUnfinishedOwnerLaunchSleepsWhenLedgerHasNoWork(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "unfinished-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	owner := standingOwnerClaim(t, cfg, supervise.StateLaunching, nil)
	owner.Generation = 1
	putStandingOwner(t, cfg, owner)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	time.Sleep(3 * o.orphanSweep)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("unfinished owner launched %d times without work", got)
	}
	if got := d.claim(owner.Token); got.State != string(supervise.StateQueued) || got.Generation != 1 {
		t.Fatalf("unfinished owner after recovery = %+v, want queued generation one", got)
	}
}

func TestExhaustedOwnerRetryRevisionSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "revision-restart-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	revision, err := queue.OwnerWorkRevision(context.Background(), repo, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	owner := standingOwnerClaim(t, cfg, supervise.StateFailed, nil)
	owner.Budgets.Deaths = cfg.LaunchFailureLimit
	owner.OwnerRetryRevision = revision
	putStandingOwner(t, cfg, owner)
	first := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))
	first.stop()
	if err := queue.Enqueue(context.Background(), repo, "item-b", "owner-a"); err != nil {
		t.Fatal(err)
	}
	rebindHeldPorts(t, &cfg)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	startDaemon(t, cfg, o)
	testwait.Eventually(t, "changed revision after restart", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
}

// A reservation whose capacity was released takes capacity again only through the cap check,
// keeps its one monetary charge, and two concurrent retries at its generation admit it once.
func TestReleasedReservationReacquiresCapacityBeforeReuse(t *testing.T) {
	cfg := testConfig(t)
	a := standingOwnerClaim(t, cfg, supervise.StateQueued, nil)
	b := a
	var err error
	b.Token, err = claim.OwnerToken(a.Project, "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	putStandingOwner(t, cfg, a)
	putStandingOwner(t, cfg, b)
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.ReserveStandingOwnerAttempt(ctx, a, 1, 10, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStandingOwnerPendingCapacity(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveStandingOwnerAttempt(ctx, b, 1, 10, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveStandingOwnerAttempt(ctx, a, 1, 10, 1); err == nil {
		t.Fatal("released reservation bypassed max_running")
	}
	if err := st.ReleaseStandingOwnerPendingCapacity(ctx, b); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- st.ReserveStandingOwnerAttempt(ctx, a, 1, 10, 1) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("a concurrent same-generation retry was refused: %v", err)
		}
	}
	var reserved float64
	var rows, active int
	if err := st.Pool().QueryRow(ctx, `SELECT coalesce(sum(reserved_usd), 0), count(*), count(*) FILTER (WHERE capacity_reserved)
		FROM standing_owner_attempts WHERE owner_token = $1`, string(a.Token)).Scan(&reserved, &rows, &active); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 || rows != 1 || active != 1 {
		t.Fatalf("owner A's reservations = $%.2f in %d rows, %d holding capacity; want $1.00 in one active row", reserved, rows, active)
	}
	if err := st.ReserveStandingOwnerAttempt(ctx, b, 1, 10, 1); err == nil {
		t.Fatal("owner B was admitted while owner A's readmitted reservation holds the only slot")
	}
}

func TestFailedOwnerSpawnsDoNotResetWithoutWorkChange(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 2
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "launch-failure-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	cfg.StandingOwners.BudgetUSD = 1000
	enqueueOwnerWork(t, queue, repo)
	rt := fake.NewRuntime()
	for range 100 {
		rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("harness unavailable")})
	}
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	testwait.Eventually(t, "initial owner spawn failures", func() bool { return len(rt.CallsOf("Spawn")) >= 2 })
	time.Sleep(150 * time.Millisecond)
	token := standingOwnerClaim(t, cfg, supervise.StateQueued, nil).Token
	if got := len(rt.CallsOf("Spawn")); got != 2 {
		t.Fatalf("unchanged work caused %d failed Spawn attempts, want 2 then wait; state=%s", got, d.claim(token).State)
	}
}

// A registration that never arrives is the second owner failure path: the launch's revision is
// bound before the deadline charges it, so an exhausted owner does not reset on unchanged work.
func TestOwnerRegistrationFailuresBindTheirWorkRevision(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 2
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "registration-failure-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	revision, err := queue.OwnerWorkRevision(context.Background(), repo, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	clock := &heldClock{}
	o.clock = clock
	d := startDaemon(t, cfg, o)
	token := standingOwnerClaim(t, cfg, supervise.StateQueued, nil).Token
	testwait.Eventually(t, "first owner launch", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
	if got := storedClaim(t, cfg, token).OwnerRetryRevision; got != revision {
		t.Fatalf("launching owner's bound work revision = %d, want %d", got, revision)
	}
	deadline := cfg.WorkerBootTimeout * time.Duration(cfg.WorkerBootRegistrationDeadlineIntervals)
	for range cfg.LaunchFailureLimit {
		clock.fire(cfg.WorkerBootTimeout)
		clock.fire(deadline)
	}
	testwait.Eventually(t, "registration failures exhaust the owner", func() bool {
		return d.claim(token).State == string(supervise.StateFailed)
	})
	launches := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))
	time.Sleep(100 * time.Millisecond)
	if got := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); got != launches {
		t.Fatalf("unchanged work relaunched the exhausted owner: %d launches, then %d", launches, got)
	}
	if got := storedClaim(t, cfg, token).OwnerRetryRevision; got != revision {
		t.Fatalf("exhausted owner's work revision = %d, want %d", got, revision)
	}
}

// A ledger that accepts TCP and never answers the PostgreSQL handshake holds owner admission
// closed, never the daemon's boot: /healthz answers within 500ms.
func TestSilentLedgerDoesNotBlockDaemonBoot(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	standingOwnerConfig(t, &cfg, queue, "silent-"+randomSuffix(t))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
			}()
		}
	}()
	dsn := "postgres://postgres:test@" + listener.Addr().String() + "/postgres?sslmode=disable"
	if err := os.WriteFile(cfg.StandingOwners.LedgerDSNFile, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	startDaemonWithin(t, cfg, fakeRuntime(rt, &built{}), quietLogger(), 500*time.Millisecond)
	if got := len(rt.CallsOf("Spawn")); got != 0 {
		t.Fatalf("owner launched %d times while its ledger never answered", got)
	}
}

// The same ledger endpoint, unreachable at first, answers owner admission again once it returns,
// without replacing the queue.
func TestLedgerQueueReconnectsAfterServiceReturns(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "reconnect-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	enqueueOwnerWork(t, queue, repo)
	parsed, err := url.Parse(cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	upstream := parsed.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var available atomic.Bool
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				if !available.Load() {
					return
				}
				backend, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer backend.Close()
				go io.Copy(backend, client)
				io.Copy(client, backend)
			}()
		}
	}()
	parsed.Host = listener.Addr().String()
	q := &ledgerQueue{dsn: parsed.String(), repo: repo, timeout: 5 * time.Second}
	t.Cleanup(func() { closeStandingOwnerQueue(q, quietLogger()) })
	if _, err := q.HasWork(context.Background(), "owner-a"); err == nil {
		t.Fatal("the unreachable ledger answered")
	}
	available.Store(true)
	testwait.Eventually(t, "ledger reconnect", func() bool {
		hasWork, err := q.HasWork(context.Background(), "owner-a")
		return err == nil && hasWork
	})
}

// recordedRefusals stands in for the ledger's RecordOwnerRefused (OwnerRefusalRecorder) and keeps
// every call it is given.
type recordedRefusals struct {
	mu    sync.Mutex
	calls []api.OwnerRefusal
}

func (r *recordedRefusals) RecordOwnerRefused(_ context.Context, repo, owner string, revision int64, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, api.OwnerRefusal{Repository: repo, Owner: owner, Revision: revision, Reason: reason})
	return nil
}

func (r *recordedRefusals) of(owner string) []api.OwnerRefusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	var calls []api.OwnerRefusal
	for _, call := range r.calls {
		if call.Owner == owner {
			calls = append(calls, call)
		}
	}
	return calls
}

// An owner on never_start is never started and never refused: its items stay open for a person.
// An owner the daemon cannot launch — a node kind with no template, or a key outside the owner-key
// grammar — is refused durably, once per work revision, through the ledger's RecordOwnerRefused,
// and the operator API lists the refusal. New work for that owner is a new revision, refused again.
func TestOwnersTheDaemonWillNotStartAreRefusedDurablyOncePerRevision(t *testing.T) {
	cfg := testConfig(t)
	queue := openOwnerLedger(t, cfg.PostgresDSN)
	repo := "refusals-" + randomSuffix(t)
	standingOwnerConfig(t, &cfg, queue, repo)
	cfg.StandingOwners.NeverStart = []string{"escalations"}
	cfg.StandingOwners.Reconcile = 50 * time.Millisecond
	for item, owner := range map[string]string{"escalation-1": "escalations", "file-1": "file:fs/namespace.c", "struct-1": "struct:mount"} {
		if err := queue.Enqueue(context.Background(), repo, item, owner); err != nil {
			t.Fatal(err)
		}
	}
	refusals := &recordedRefusals{}
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.ownerRefusals = refusals
	d := startDaemon(t, cfg, o)
	testwait.Eventually(t, "both unlaunchable owners refused", func() bool {
		return len(refusals.of("file:fs/namespace.c")) == 1 && len(refusals.of("struct:mount")) == 1
	})
	revision := func(owner string) int64 {
		t.Helper()
		got, err := queue.OwnerWorkRevision(context.Background(), repo, owner)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, owner := range []string{"file:fs/namespace.c", "struct:mount"} {
		if got := refusals.of(owner)[0]; got.Repository != repo || got.Revision != revision(owner) || got.Reason == "" {
			t.Errorf("refusal of %s = %+v, want repository %s at revision %d with a reason", owner, got, repo, revision(owner))
		}
	}
	status, body := d.request(http.MethodGet, "/legion/v1/operator/owners/refused", nil, true)
	if status != http.StatusOK {
		t.Fatalf("list the refused owners = %d: %s", status, body)
	}
	var listed api.OwnerRefusals
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Refused) != 2 || listed.Refused[0].Owner != "file:fs/namespace.c" || listed.Refused[1].Owner != "struct:mount" ||
		!listed.Refused[1].Recorded || listed.Refused[1].Revision != revision("struct:mount") {
		t.Fatalf("the operator's refused owners = %+v, want file:fs/namespace.c and struct:mount, recorded at their revisions", listed.Refused)
	}
	// Sweeps over the same work refuse nothing again; never_start's owner is neither started nor
	// refused, and the named owner with work starts beside them.
	if err := queue.Enqueue(context.Background(), repo, "owner-a-wake", "owner-a"); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "the named owner starts", func() bool { return len(rt.CallsOf("Spawn")) == 1 })
	time.Sleep(10 * cfg.StandingOwners.Reconcile)
	if got := len(refusals.of("struct:mount")); got != 1 {
		t.Fatalf("struct:mount refused %d times on unchanged work, want once", got)
	}
	if got := refusals.of("escalations"); len(got) != 0 {
		t.Fatalf("never_start owner was refused: %+v", got)
	}
	for _, call := range rt.CallsOf("Spawn") {
		if key, _ := call.Spec.Claim.OwnerKey(call.Spec.Project); key != "owner-a" {
			t.Fatalf("spawned %s, want only owner-a", call.Spec.Claim)
		}
	}
	if err := queue.Enqueue(context.Background(), repo, "struct-2", "struct:mount"); err != nil {
		t.Fatal(err)
	}
	testwait.Eventually(t, "new work refused again", func() bool { return len(refusals.of("struct:mount")) == 2 })
	if got := refusals.of("struct:mount")[1].Revision; got != revision("struct:mount") {
		t.Fatalf("second refusal at revision %d, want %d", got, revision("struct:mount"))
	}
}
