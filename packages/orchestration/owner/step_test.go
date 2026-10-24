package owner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	answer    string // the item the step answers
	record    Record // what the step's Build makes, whatever HEAD holds
	notice    Notice // the notice the record reports, with no commit
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
		answer:    answer,
		notice: Notice{
			Topic:   "notifications.example.node.struct:ns_common.needs",
			Message: "need changed",
			Payload: Payload{"item": answer},
			Key:     "c-need-ns-match-r1",
		},
	}
	fixture.record = Record{
		Data:    []byte("need: ns_common\n"),
		Subject: "owner: record need",
		FollowUps: []FollowUp{
			{Item: "offer:function:ns_match->struct:ns_common@r1", Owner: "struct:ns_common"},
			{Item: "review:function:ns_match@r1", Owner: owner},
		},
		Notices: func(commit string) []Notice {
			notice := fixture.notice
			notice.Payload = Payload{"item": answer, "commit": commit}
			return []Notice{notice}
		},
	}
	fixture.step = Step{
		Answers: []string{answer},
		Path:    "skeleton/nodes/Kb401aaa780da5ad4/needs/E9c01e86f8749cad3.yaml",
		Build:   func([]byte, bool) (Record, error) { return fixture.record, nil },
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

// A step whose every answer is done, and whose Build makes the bytes HEAD holds, is the repeat of the step that
// answered them: it reads its answers and changes nothing, not even its notices.
func TestStepWhoseAnswersAreDoneRepeatsItsRecordWithNoMutation(t *testing.T) {
	fixture := newStepFixture(t)
	first, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	fixture.gate.reset()
	fixture.publisher.published = nil

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.AlreadyDone || outcome.Commit != first.Commit {
		t.Fatalf("outcome = %+v, want AlreadyDone at %s", outcome, first.Commit)
	}
	if got, want := fixture.gate.calls, []string{"item"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dependency calls = %v, want %v", got, want)
	}
	if len(fixture.publisher.published) != 0 {
		t.Fatalf("published = %+v, want none", fixture.publisher.published)
	}
}

// A step whose every answer is done but whose Build makes other bytes is a new record: it answers none of them, so
// its commit carries no trailer and it completes nothing, and it queues its follow-ups and sends its notices.
func TestStepWhoseAnswersAreDoneWritesAChangedRecordAnsweringNone(t *testing.T) {
	fixture := newStepFixture(t)
	first, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	fixture.record.Data = []byte("need: ns_common, revised\n")
	fixture.record.FollowUps = []FollowUp{{Item: "offer:function:ns_match->struct:ns_common@r2", Owner: "struct:ns_common"}}
	fixture.gate.reset()
	fixture.publisher.published = nil

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.AlreadyDone || outcome.Commit == "" || outcome.Commit == first.Commit {
		t.Fatalf("outcome = %+v, want a new record commit after %s", outcome, first.Commit)
	}
	if got := gitOutput(t, fixture.repo.Path, nil, "log", "-1", "--format=%(trailers:key=Owner-Item,valueonly)", outcome.Commit); got != "" {
		t.Fatalf("the new record's commit carries Owner-Item %q, but it answers no open item", got)
	}
	if got := fixture.gate.count("claim") + fixture.gate.count("complete"); got != 0 {
		t.Fatalf("the new record claimed or completed %d times: %v", got, fixture.gate.calls)
	}
	if queued, err := fixture.store.Item(fixture.ctx, "r", fixture.record.FollowUps[0].Item); err != nil || queued.State != "open" {
		t.Fatalf("follow-up = %+v, %v", queued, err)
	}
	if len(fixture.publisher.published) != 1 {
		t.Fatalf("published = %+v, want the new record's notice", fixture.publisher.published)
	}
}

// One record answers several items: its commit carries an Owner-Item trailer for each open one, each completes with
// that commit under its own lease generation, and a done one gets neither.
func TestStepAnsweringSeveralItemsCarriesATrailerForEachAndCompletesEach(t *testing.T) {
	fixture := newStepFixture(t)
	owner := fixture.runner.Owner
	held, done := "reply:function:ns_match->struct:ns_common@abc", "start:function:ns_match@1"
	for _, item := range []string{held, done} {
		if err := fixture.store.Enqueue(fixture.ctx, "r", item, owner); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier process of the owner holds one of them; another item's lease raises its generation first.
	for range 2 {
		lease, err := fixture.store.Claim(fixture.ctx, "r", held, owner, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Release(fixture.ctx, "r", held, owner, lease.Generation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.Claim(fixture.ctx, "r", held, owner, 0); err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.store.Claim(fixture.ctx, "r", done, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Complete(fixture.ctx, "r", done, owner, lease.Generation, "earlier"); err != nil {
		t.Fatal(err)
	}
	fixture.step.Answers = []string{fixture.answer, held, done}

	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	trailers := gitOutput(t, fixture.repo.Path, nil, "log", "-1", "--format=%(trailers:key=Owner-Item,valueonly)", outcome.Commit)
	if got, want := strings.Fields(trailers), []string{fixture.answer, held}; !slices.Equal(got, want) {
		t.Fatalf("Owner-Item trailers = %q, want %q", got, want)
	}
	for _, item := range []string{fixture.answer, held} {
		if got, err := fixture.store.Item(fixture.ctx, "r", item); err != nil || got.State != "done" || got.Result != outcome.Commit {
			t.Errorf("%s = %+v, %v; want done with %s", item, got, err, outcome.Commit)
		}
	}
	if got, err := fixture.store.Item(fixture.ctx, "r", done); err != nil || got.Result != "earlier" {
		t.Errorf("the done answer = %+v, %v; want its earlier result", got, err)
	}
}

// An open answer that another holder's live lease holds stops the step before it claims any answer or writes.
func TestStepRefusesAnAnswerAnotherHoldsBeforeItClaimsAny(t *testing.T) {
	fixture := newStepFixture(t)
	held := "reply:function:ns_match->struct:ns_common@abc"
	if err := fixture.store.Enqueue(fixture.ctx, "r", held, fixture.runner.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Claim(fixture.ctx, "r", held, "a-person", 0); err != nil {
		t.Fatal(err)
	}
	fixture.step.Answers = []string{fixture.answer, held}
	fixture.gate.reset()

	var refusal *ledger.Refusal
	if _, err := fixture.runner.Run(fixture.ctx, fixture.step); !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonLeaseHeld {
		t.Fatalf("Run error = %v, want a held-lease refusal", err)
	}
	if got := fixture.gate.count("claim"); got != 0 {
		t.Fatalf("the refused step claimed %d answers", got)
	}
	if got := gitOutput(t, fixture.repo.Path, nil, "rev-list", "--count", fixture.runner.Tree.Branch, "--", fixture.step.Path); got != "0" {
		t.Fatalf("the refused step committed %s records", got)
	}
}

// A Build that refuses writes nothing and claims nothing: the refusal is the step's error, as Build returned it.
func TestStepWhoseBuildRefusesWritesNothing(t *testing.T) {
	fixture := newStepFixture(t)
	refused := errors.New("the claim tree reuses a retired id")
	fixture.step.Build = func([]byte, bool) (Record, error) { return Record{}, refused }
	fixture.gate.reset()
	if _, err := fixture.runner.Run(fixture.ctx, fixture.step); !errors.Is(err, refused) {
		t.Fatalf("Run error = %v, want the Build's refusal", err)
	}
	if got := fixture.gate.count("claim") + fixture.gate.count("enqueue"); got != 0 {
		t.Fatalf("the refused step made %d claim or enqueue calls", got)
	}
	if item, err := fixture.store.Item(fixture.ctx, "r", fixture.answer); err != nil || item.Holder != "" {
		t.Fatalf("answer = %+v, %v; want unheld", item, err)
	}
}

// Build runs with the worktree's commit lock held, from its read of HEAD to the step's end: a second step of the owner
// that starts while the first builds waits, then builds from the first's record, so the revision each picks from HEAD
// is the one it commits.
func TestStepBuildsItsRecordUnderTheCommitLock(t *testing.T) {
	fixture := newStepFixture(t)
	fixture.step.Answers = nil
	building, proceed := make(chan struct{}), make(chan struct{})
	var builds atomic.Int32
	revisioned := func(previous []byte, found bool) (Record, error) {
		revision := 1
		if found {
			if _, err := fmt.Sscanf(string(previous), "revision %d", &revision); err != nil {
				return Record{}, err
			}
			revision++
		}
		if builds.Add(1) == 1 {
			close(building)
			<-proceed
		}
		return Record{Data: []byte(fmt.Sprintf("revision %d\n", revision)), Subject: fmt.Sprintf("owner: r%d", revision)}, nil
	}
	fixture.step.Build = revisioned

	first := make(chan error, 1)
	go func() { _, err := fixture.runner.Run(fixture.ctx, fixture.step); first <- err }()
	<-building
	second := make(chan error, 1)
	go func() { _, err := fixture.runner.Run(fixture.ctx, fixture.step); second <- err }()
	time.Sleep(300 * time.Millisecond)
	if got := builds.Load(); got != 1 {
		t.Fatalf("%d builds ran while the first step held the lock", got)
	}
	close(proceed)
	for _, done := range []chan error{first, second} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := gitOutput(t, fixture.repo.Path, nil, "show", fixture.runner.Tree.Branch+":"+fixture.step.Path); got != "revision 2" {
		t.Fatalf("the record after two steps holds %q, want revision 2", got)
	}
}

// A record commit writes the files a Build puts beside its record (an escalation beside its reply), in one commit.
func TestStepCommitsTheFilesBesideItsRecordInItsCommit(t *testing.T) {
	fixture := newStepFixture(t)
	beside := File{Path: "skeleton/nodes/Kb401aaa780da5ad4/escalations/0123456789abcdef.yaml", Data: []byte("subject: x\n")}
	fixture.record.Also = []File{beside}
	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	changed := gitOutput(t, fixture.repo.Path, nil, "show", "--name-only", "--format=", outcome.Commit)
	if got, want := strings.Fields(changed), []string{beside.Path, fixture.step.Path}; !slices.Equal(got, want) {
		t.Fatalf("the record commit changed %q, want %q", got, want)
	}
	if got := gitOutput(t, fixture.repo.Path, nil, "show", outcome.Commit+":"+beside.Path); got != "subject: x" {
		t.Fatalf("%s holds %q", beside.Path, got)
	}
}

// The notices are built once the record's commit is known, so each names the commit that holds its record.
func TestStepBuildsItsNoticesFromTheRecordCommit(t *testing.T) {
	fixture := newStepFixture(t)
	outcome, err := fixture.runner.Run(fixture.ctx, fixture.step)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.publisher.published) != 1 || fixture.publisher.published[0].payload.(Payload)["commit"] != outcome.Commit {
		t.Fatalf("published = %+v, want one notice naming commit %s", fixture.publisher.published, outcome.Commit)
	}
}

func TestStepAdoptsLiveLeaseHeldBySameOwner(t *testing.T) {
	fixture := newStepFixture(t)
	lease, err := fixture.store.Claim(fixture.ctx, "r", fixture.answer, fixture.runner.Owner, 0)
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
	followUp := fixture.record.FollowUps[0]
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
	if got, want := outcome.Undelivered, []string{fixture.notice.Topic}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Undelivered = %v, want %v", got, want)
	}
	if got := fixture.gate.count("publish"); got != 4 {
		t.Fatalf("Publish calls = %d, want 4", got)
	}
	journal, err := os.ReadFile(fixture.runner.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), fixture.notice.Topic) {
		t.Fatalf("journal = %q, want topic %q", journal, fixture.notice.Topic)
	}
	assertStepComplete(t, fixture)
}

// Bytes another step already committed are no proof that this step answered its item: the item stays open.
func TestStepRefusesToCompleteAnItemWhoseRecordCommitLacksItsTrailer(t *testing.T) {
	fixture := newStepFixture(t)
	if _, err := fixture.runner.Tree.CommitFile(fixture.ctx, fixture.step.Path, fixture.record.Data, "owner: another step", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.runner.Run(fixture.ctx, fixture.step); !errors.Is(err, ErrUnattributed) {
		t.Fatalf("Run error = %v, want ErrUnattributed", err)
	}
	item, err := fixture.store.Item(fixture.ctx, "r", fixture.answer)
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
	if got, want := outcome.Undelivered, []string{fixture.notice.Topic}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Undelivered = %v, want %v", got, want)
	}
	if got := fixture.gate.count("publish"); got != 4 {
		t.Fatalf("Publish calls = %d, want 4", got)
	}
	journal, err := os.ReadFile(fixture.runner.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), fixture.notice.Key) {
		t.Fatalf("journal = %q, want key %q", journal, fixture.notice.Key)
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
	noRetryDelays(t)
	if _, err := Deliver(context.Background(), &topicPublisher{refuse: "notifications.example.all"}, runner.Journal, []Notice{{Topic: "notifications.example.all", Key: "k"}}); err != nil {
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
	item, owner := fixture.answer, fixture.runner.Owner
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

func newNoAnswerStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	fixture := newStepFixture(t)
	fixture.step.Answers = nil
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
	if commit := gitOutput(t, fixture.repo.Path, nil, "log", "-1", "--format=%H", branch, "--", fixture.step.Path); outcome.Commit != commit {
		t.Fatalf("outcome = %+v, want commit %s", outcome, commit)
	}
	for _, followUp := range fixture.record.FollowUps {
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
	item, err := fixture.store.Item(fixture.ctx, "r", fixture.answer)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "done" {
		t.Fatalf("answer item = %+v, want done", item)
	}
	commits := commitsForOwnerItem(t, fixture.repo, fixture.answer)
	if len(commits) != 1 {
		t.Fatalf("commits with Owner-Item %q = %v, want exactly one", fixture.answer, commits)
	}
	if item.Result != commits[0] {
		t.Fatalf("answer result = %q, want record commit %q", item.Result, commits[0])
	}
	for _, followUp := range fixture.record.FollowUps {
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
