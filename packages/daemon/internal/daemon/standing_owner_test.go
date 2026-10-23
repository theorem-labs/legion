package daemon

import (
	"context"
	"encoding/json"
	"net/http"
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
func TestStandingOwnerRecoversLaunchRecordedBeforeProcessStart(t *testing.T) {
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

func TestStandingOwnerRestartAdoptsLiveOwnerWithoutSecondStart(t *testing.T) {
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

func TestStandingOwnerVMRestartStartsOnlyWithQueuedWork(t *testing.T) {
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

func TestStandingOwnerOrphanSweepReapsUnrecordedPane(t *testing.T) {
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
