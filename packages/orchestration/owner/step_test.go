package owner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

var errInjected = errors.New("injected failure")

type callGate struct {
	failAt int
	calls  []string
}

func (g *callGate) call(name string) error {
	g.calls = append(g.calls, name)
	if g.failAt == len(g.calls) {
		return errInjected
	}
	return nil
}

func (g *callGate) reset() {
	g.failAt = 0
	g.calls = nil
}

func (g *callGate) count(name string) int {
	count := 0
	for _, call := range g.calls {
		if call == name {
			count++
		}
	}
	return count
}

type faultLedger struct {
	store *ledger.Store
	gate  *callGate
}

func (l faultLedger) Item(ctx context.Context, repo, id string) (ledger.Item, error) {
	if err := l.gate.call("item"); err != nil {
		return ledger.Item{}, err
	}
	return l.store.Item(ctx, repo, id)
}

func (l faultLedger) Claim(ctx context.Context, repo, id, holder string, ttl time.Duration) (ledger.Lease, error) {
	if err := l.gate.call("claim"); err != nil {
		return ledger.Lease{}, err
	}
	return l.store.Claim(ctx, repo, id, holder, ttl)
}

func (l faultLedger) Enqueue(ctx context.Context, repo, id, owner string) error {
	if err := l.gate.call("enqueue"); err != nil {
		return err
	}
	return l.store.Enqueue(ctx, repo, id, owner)
}

func (l faultLedger) Complete(ctx context.Context, repo, id, holder string, generation int64, result string) error {
	if err := l.gate.call("complete"); err != nil {
		return err
	}
	return l.store.Complete(ctx, repo, id, holder, generation, result)
}

type publishedNotice struct {
	topic, message, key string
	payload             any
}

type faultPublisher struct {
	gate       *callGate
	alwaysFail bool
	published  []publishedNotice
}

func (p *faultPublisher) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	p.published = append(p.published, publishedNotice{topic: topic, message: message, payload: payload, key: key})
	if err := p.gate.call("publish"); err != nil {
		return err
	}
	if p.alwaysFail {
		return errInjected
	}
	return nil
}

type stepFixture struct {
	ctx       context.Context
	store     *ledger.Store
	ledger    faultLedger
	publisher *faultPublisher
	gate      *callGate
	repo      Repo
	runner    Runner
	step      Step
}

func newStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	ctx := context.Background()
	store, err := ledger.Open(ctx, testpg.Database(t, "owner_step"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}

	answer := "need:function:ns_match->struct:ns_common@r1"
	owner := "function:ns_match"
	if err := store.Enqueue(ctx, "r", answer, owner); err != nil {
		t.Fatal(err)
	}
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/function-ns-match")
	gate := &callGate{}
	publisher := &faultPublisher{gate: gate}
	fixture := &stepFixture{
		ctx:       ctx,
		store:     store,
		ledger:    faultLedger{store: store, gate: gate},
		publisher: publisher,
		gate:      gate,
		repo:      repo,
		step: Step{
			Answers: answer,
			Path:    "skeleton/nodes/Kb401aaa780da5ad4/needs/E9c01e86f8749cad3.yaml",
			Data:    []byte("need: ns_common\n"),
			Subject: "owner: record need",
			FollowUps: []FollowUp{
				{Item: "offer:function:ns_match->struct:ns_common@r1", Owner: "struct:ns_common"},
				{Item: "review:function:ns_match@r1", Owner: owner},
			},
			Notices: []Notice{{
				Topic:   "notifications.example.node.struct:ns_common.needs",
				Message: "need changed",
				Payload: Payload{"item": answer},
				Key:     "c-need-ns-match-r1",
			}},
		},
	}
	fixture.runner = Runner{
		Ledger:  fixture.ledger,
		Tree:    worktree,
		Envoy:   publisher,
		Owner:   owner,
		Repo:    "r",
		Journal: filepath.Join(t.TempDir(), "state", "undelivered.jsonl"),
	}
	return fixture
}

func TestStepRecoversFromEveryDependencyFailureWithoutDuplicatingWork(t *testing.T) {
	baseline := newStepFixture(t)
	baselineOutcome, err := baseline.runner.Run(baseline.ctx, baseline.step)
	if err != nil {
		t.Fatal(err)
	}
	if baselineOutcome.Commit == "" {
		t.Fatal("baseline step did not create a record commit")
	}
	calls := append([]string(nil), baseline.gate.calls...)
	if len(calls) == 0 {
		t.Fatal("baseline step made no dependency calls")
	}

	for failedCall := 1; failedCall <= len(calls); failedCall++ {
		failedCall := failedCall
		t.Run(calls[failedCall-1], func(t *testing.T) {
			fixture := newStepFixture(t)
			fixture.gate.failAt = failedCall
			_, err := fixture.runner.Run(fixture.ctx, fixture.step)
			if calls[failedCall-1] == "publish" {
				if err != nil {
					t.Fatalf("Run after a transient publish failure: %v", err)
				}
			} else if !errors.Is(err, errInjected) {
				t.Fatalf("Run error = %v, want injected failure", err)
			}

			fixture.gate.failAt = 0
			if _, err := fixture.runner.Run(fixture.ctx, fixture.step); err != nil {
				t.Fatalf("Run after injected failure: %v", err)
			}
			assertStepComplete(t, fixture)
			assertStableNoticeKeys(t, fixture.publisher.published)
		})
	}
}

func TestStepDoneItemMakesNoMutation(t *testing.T) {
	fixture := newStepFixture(t)
	lease, err := fixture.store.Claim(fixture.ctx, "r", fixture.step.Answers, fixture.runner.Owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Complete(fixture.ctx, "r", fixture.step.Answers, fixture.runner.Owner, lease.Generation, "already"); err != nil {
		t.Fatal(err)
	}
	fixture.gate.reset()

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.AlreadyDone {
		t.Fatalf("outcome = %+v, want AlreadyDone", outcome)
	}
	if got, want := fixture.gate.calls, []string{"item"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dependency calls = %v, want %v", got, want)
	}
	if len(fixture.publisher.published) != 0 {
		t.Fatalf("published = %+v, want none", fixture.publisher.published)
	}
}

func TestStepAdoptsLiveLeaseHeldBySameOwner(t *testing.T) {
	fixture := newStepFixture(t)
	lease, err := fixture.store.Claim(fixture.ctx, "r", fixture.step.Answers, fixture.runner.Owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	fixture.gate.reset()

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Commit == "" {
		t.Fatal("Run did not commit a record")
	}
	if got := fixture.gate.count("claim"); got != 0 {
		t.Fatalf("Claim calls = %d, want 0 after adopting generation %d", got, lease.Generation)
	}
	assertStepComplete(t, fixture)
}

func TestStepRefusesFollowUpOwnedByAnotherOwner(t *testing.T) {
	fixture := newStepFixture(t)
	followUp := fixture.step.FollowUps[0]
	if err := fixture.store.Enqueue(fixture.ctx, "r", followUp.Item, "function:other"); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if !errors.Is(err, ErrIDOwner) {
		t.Fatalf("Run error = %v, want ErrIDOwner", err)
	}
}

func TestStepJournalsNoticeAfterFourPublisherFailures(t *testing.T) {
	fixture := newStepFixture(t)
	fixture.publisher.alwaysFail = true

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcome.Undelivered, []string{fixture.step.Notices[0].Topic}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Undelivered = %v, want %v", got, want)
	}
	if got := fixture.gate.count("publish"); got != 4 {
		t.Fatalf("Publish calls = %d, want 4", got)
	}
	journal, err := os.ReadFile(fixture.runner.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), fixture.step.Notices[0].Topic) {
		t.Fatalf("journal = %q, want topic %q", journal, fixture.step.Notices[0].Topic)
	}
	assertStepComplete(t, fixture)
}

// Bytes another step already committed are no proof that this step answered its item: the item stays open.
func TestStepRefusesToCompleteAnItemWhoseRecordCommitLacksItsTrailer(t *testing.T) {
	fixture := newStepFixture(t)
	if _, err := fixture.runner.Tree.CommitFile(fixture.ctx, fixture.step.Path, fixture.step.Data, "owner: another step", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.runner.Run(fixture.ctx, fixture.step); !errors.Is(err, ErrUnattributed) {
		t.Fatalf("Run error = %v, want ErrUnattributed", err)
	}
	item, err := fixture.store.Item(fixture.ctx, "r", fixture.step.Answers)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "open" {
		t.Fatalf("answer item = %+v, want open", item)
	}
}

// A step that answers no item (a need, a spec revision with no open item) writes its record once, queues its
// follow-ups and publishes its notices, whichever dependency call failed first; it never reads, claims or
// completes an item.
func TestStepAnsweringNoItemRecoversFromEveryDependencyFailure(t *testing.T) {
	baseline := newNoAnswerStepFixture(t)
	if _, err := baseline.runner.Run(baseline.ctx, baseline.step); err != nil {
		t.Fatal(err)
	}
	calls := append([]string(nil), baseline.gate.calls...)
	if got, want := strings.Join(calls, ","), "enqueue,enqueue,publish"; got != want {
		t.Fatalf("dependency calls = %s, want %s", got, want)
	}

	for failedCall := 1; failedCall <= len(calls); failedCall++ {
		t.Run(calls[failedCall-1], func(t *testing.T) {
			fixture := newNoAnswerStepFixture(t)
			fixture.gate.failAt = failedCall
			_, err := fixture.runner.Run(fixture.ctx, fixture.step)
			if calls[failedCall-1] == "publish" {
				if err != nil {
					t.Fatalf("Run after a transient publish failure: %v", err)
				}
			} else if !errors.Is(err, errInjected) {
				t.Fatalf("Run error = %v, want injected failure", err)
			}

			fixture.gate.failAt = 0
			outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
			if err != nil {
				t.Fatalf("Run after injected failure: %v", err)
			}
			assertNoAnswerStepRecorded(t, fixture, outcome)
			assertStableNoticeKeys(t, fixture.publisher.published)
		})
	}
}

// The notice of a step that answers no item is journaled like any other: its record and follow-ups are durable
// already, so a listener that refuses it does not fail the step.
func TestStepAnsweringNoItemJournalsNoticeAfterFourPublisherFailures(t *testing.T) {
	fixture := newNoAnswerStepFixture(t)
	fixture.publisher.alwaysFail = true

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcome.Undelivered, []string{fixture.step.Notices[0].Topic}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Undelivered = %v, want %v", got, want)
	}
	if got := fixture.gate.count("publish"); got != 4 {
		t.Fatalf("Publish calls = %d, want 4", got)
	}
	journal, err := os.ReadFile(fixture.runner.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), fixture.step.Notices[0].Key) {
		t.Fatalf("journal = %q, want key %q", journal, fixture.step.Notices[0].Key)
	}
	assertNoAnswerStepRecorded(t, fixture, outcome)
}

// runnerEnv is an owner session's environment: a reachable listener, and no owner variable.
func runnerEnv(overrides map[string]string) func(string) string {
	env := map[string]string{"ENVOY_URL": "http://127.0.0.1:1", "ENVOY_TOKEN": "token"}
	for name, value := range overrides {
		env[name] = value
	}
	return func(name string) string { return env[name] }
}

// The runner acts for the owner and the ledger repository it is given; the environment names neither.
func TestNewRunnerActsForTheOwnerAndRepositoryItIsGiven(t *testing.T) {
	runner, err := NewRunner("function:ns_match", "r", nil, "/srv/owners/state", runnerEnv(map[string]string{"LEGION_OWNER": "function:other", "LEGION_LEDGER_REPOSITORY": "other"}))
	if err != nil {
		t.Fatal(err)
	}
	if runner.Owner != "function:ns_match" || runner.Repo != "r" {
		t.Fatalf("runner acts for %q in %q", runner.Owner, runner.Repo)
	}
	for _, missing := range [][2]string{{"", "r"}, {"function:ns_match", ""}} {
		if _, err := NewRunner(missing[0], missing[1], nil, "/srv/owners/state", runnerEnv(nil)); err == nil {
			t.Errorf("NewRunner(%q, %q) built a runner", missing[0], missing[1])
		}
	}
}

func TestNewRunnerRefusesARelativeStateDir(t *testing.T) {
	_, err := NewRunner("function:ns_match", "r", nil, "state", runnerEnv(nil))
	if err == nil || !strings.Contains(err.Error(), "state_dir") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("NewRunner error = %v, want a refused relative state_dir", err)
	}
}

// A listener configuration that cannot be read is no delivery failure to retry and journal: the runner is never
// built, so the command stops before it writes anything.
func TestNewRunnerRefusesAnUnresolvableListener(t *testing.T) {
	home := t.TempDir()
	_, err := NewRunner("function:ns_match", "r", nil, "/srv/owners/state", runnerEnv(map[string]string{"ENVOY_URL": "", "ENVOY_TOKEN": "", "HOME": home}))
	if !errors.Is(err, ErrListenerConfig) || !strings.Contains(err.Error(), "envoy.json") {
		t.Fatalf("NewRunner error = %v, want ErrListenerConfig naming the configuration it could not read", err)
	}
}

// The journal of a runner is the deployment's state_dir/undelivered.jsonl, wherever the owner's command runs.
func TestNewRunnerJournalsUnderTheDeploymentStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	runner, err := NewRunner("function:ns_match", "r", nil, stateDir, runnerEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateDir, "undelivered.jsonl"); runner.Journal != want {
		t.Fatalf("Journal = %q, want %q", runner.Journal, want)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := runner.journal(Notice{Topic: "notifications.example.all", Key: "k"}, errInjected); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runner.Journal); err != nil {
		t.Fatalf("journal not written under state_dir: %v", err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("journal wrote into the working directory: %v", entries)
	}
}

// OpenLedgerFile opens the ledger whose DSN a file holds, and names where that file came from when it cannot.
func TestOpenLedgerFileNamesTheFilesSource(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{empty, filepath.Join(dir, "missing")} {
		if _, err := OpenLedgerFile(context.Background(), "ledger_dsn_file", path); err == nil || !strings.Contains(err.Error(), "ledger_dsn_file") || !strings.Contains(err.Error(), path) {
			t.Errorf("OpenLedgerFile(%q) error = %v, want one naming ledger_dsn_file and the path", path, err)
		}
	}
	dsnFile := filepath.Join(dir, "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte(testpg.Database(t, "owner_open_ledger")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenLedgerFile(context.Background(), "ledger_dsn_file", dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
}

// Supersede completes an item with no record, and claims or adopts its lease by the same rule as Run.
func TestSupersedeAdoptsOnlyALiveLeaseOfTheSameOwner(t *testing.T) {
	fixture := newStepFixture(t)
	item, owner := fixture.step.Answers, fixture.runner.Owner
	if _, err := fixture.store.Claim(fixture.ctx, "r", item, owner, 0); err != nil {
		t.Fatal(err)
	}
	fixture.gate.reset()
	if err := Supersede(fixture.ctx, fixture.ledger, "r", owner, item, "r2"); err != nil {
		t.Fatal(err)
	}
	if got := fixture.gate.count("claim"); got != 0 {
		t.Fatalf("Supersede claimed %d times over the owner's own live lease, want 0", got)
	}
	if done, err := fixture.store.Item(fixture.ctx, "r", item); err != nil || done.State != "done" || done.Result != "superseded:r2" {
		t.Fatalf("superseded item = %+v, %v", done, err)
	}

	// A lease of the same owner that has ended is no earlier process of the owner: it is claimed anew.
	expired := "need:function:ns_match->struct:ns_common@r3"
	if err := fixture.store.Enqueue(fixture.ctx, "r", expired, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Claim(fixture.ctx, "r", expired, owner, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	fixture.gate.reset()
	if err := Supersede(fixture.ctx, fixture.ledger, "r", owner, expired, "r4"); err != nil {
		t.Fatalf("Supersede over the owner's ended lease: %v", err)
	}
	if got := fixture.gate.count("claim"); got != 1 {
		t.Fatalf("Supersede claimed %d times over an ended lease, want 1", got)
	}

	// Another owner's live lease is refused.
	held := "need:function:ns_match->struct:ns_common@r5"
	if err := fixture.store.Enqueue(fixture.ctx, "r", held, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Claim(fixture.ctx, "r", held, "a-person", 0); err != nil {
		t.Fatal(err)
	}
	var refusal *ledger.Refusal
	if err := Supersede(fixture.ctx, fixture.ledger, "r", owner, held, "r6"); !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonLeaseHeld {
		t.Fatalf("Supersede over another holder's lease error = %v, want a held-lease refusal", err)
	}
}

func TestJournalRefusesAPathRelativeToTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	for _, path := range []string{"", "undelivered.jsonl"} {
		err := Runner{Journal: path}.journal(Notice{Topic: "notifications.example.all", Key: "k"}, errInjected)
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("journal(%q) error = %v, want a refused relative path", path, err)
		}
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("journal wrote into the working directory: %v", entries)
	}
}

func newNoAnswerStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	fixture := newStepFixture(t)
	fixture.step.Answers = ""
	return fixture
}

func assertNoAnswerStepRecorded(t *testing.T, fixture *stepFixture, outcome Outcome) {
	t.Helper()
	// Item calls may check a follow-up that an earlier run queued; nothing claims or completes an answer.
	if got := fixture.gate.count("claim") + fixture.gate.count("complete"); got != 0 {
		t.Fatalf("a step that answers no item made %d claim or complete calls: %v", got, fixture.gate.calls)
	}
	branch := fixture.runner.Tree.Branch
	if got := gitOutput(t, fixture.repo.Path, nil, "rev-list", "--count", branch, "--", fixture.step.Path); got != "1" {
		t.Fatalf("commits of %s = %s, want 1", fixture.step.Path, got)
	}
	if commit := gitOutput(t, fixture.repo.Path, nil, "log", "-1", "--format=%H", branch, "--", fixture.step.Path); outcome.Commit != commit || outcome.Result != commit {
		t.Fatalf("outcome = %+v, want commit and result %s", outcome, commit)
	}
	for _, followUp := range fixture.step.FollowUps {
		queued, err := fixture.store.Item(fixture.ctx, "r", followUp.Item)
		if err != nil {
			t.Fatalf("follow-up %q: %v", followUp.Item, err)
		}
		if queued.Owner != followUp.Owner || queued.State != "open" {
			t.Fatalf("follow-up %q = %+v, want open for %q", followUp.Item, queued, followUp.Owner)
		}
	}
}

func assertStepComplete(t *testing.T, fixture *stepFixture) {
	t.Helper()
	item, err := fixture.store.Item(fixture.ctx, "r", fixture.step.Answers)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "done" {
		t.Fatalf("answer item = %+v, want done", item)
	}
	commits := commitsForOwnerItem(t, fixture.repo, fixture.step.Answers)
	if len(commits) != 1 {
		t.Fatalf("commits with Owner-Item %q = %v, want exactly one", fixture.step.Answers, commits)
	}
	if item.Result != commits[0] {
		t.Fatalf("answer result = %q, want record commit %q", item.Result, commits[0])
	}
	for _, followUp := range fixture.step.FollowUps {
		queued, err := fixture.store.Item(fixture.ctx, "r", followUp.Item)
		if err != nil {
			t.Fatalf("follow-up %q: %v", followUp.Item, err)
		}
		if queued.Owner != followUp.Owner {
			t.Fatalf("follow-up %q owner = %q, want %q", followUp.Item, queued.Owner, followUp.Owner)
		}
	}
}

func commitsForOwnerItem(t *testing.T, repo Repo, item string) []string {
	t.Helper()
	output := gitOutput(t, repo.Path, nil, "log", "--all", "--format=%H%x09%(trailers:key=Owner-Item,valueonly,separator=%x09)")
	var commits []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if slices.Contains(fields[1:], item) {
			commits = append(commits, fields[0])
		}
	}
	return commits
}

func assertStableNoticeKeys(t *testing.T, notices []publishedNotice) {
	t.Helper()
	keys := map[string]string{}
	for _, notice := range notices {
		if prior, found := keys[notice.topic]; found && prior != notice.key {
			t.Fatalf("notice keys for %q changed from %q to %q", notice.topic, prior, notice.key)
		}
		keys[notice.topic] = notice.key
	}
}
