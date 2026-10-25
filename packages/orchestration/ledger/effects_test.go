package ledger_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

const (
	hookKey       = "hook/batch-1/0/report"
	landingCommit = "merged-1"
)

// hookIntent records the intent of effect key on batch, with the landing commit in its result, as LandBatch records one
// for each post_land_effects entry of a landed batch. LandBatch is built separately; until it is, the tests write the
// row directly.
func hookIntent(t *testing.T, dsn, batch, key, commit string) {
	t.Helper()
	batchSQL(t, dsn, `INSERT INTO batch_effects (repo, batch, key, op, result)
		VALUES ('r', $1, $2, 'hook', jsonb_build_object('commit', $3::text))`, batch, key, commit)
}

// landedBatch creates batch id of repository r, landed, with a hook intent for each key, as LandBatch leaves it.
func landedBatch(t *testing.T, s *ledger.Store, dsn, id string, keys ...string) {
	t.Helper()
	createBatch(t, s, id, batchRecord("landed"))
	setAdmission(t, dsn, id, "landed", "")
	for _, key := range keys {
		hookIntent(t, dsn, id, key, landingCommit)
	}
}

// effectFixture is repository r with batch-1 landed and one hook intent, hookKey.
func effectFixture(t *testing.T) (*ledger.Store, string) {
	t.Helper()
	s, dsn := newStoreDSN(t)
	landedBatch(t, s, dsn, "batch-1", hookKey)
	return s, dsn
}

func jsonValue(value any) json.RawMessage { return json.RawMessage(must(json.Marshal(value))) }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func effectOf(t *testing.T, s *ledger.Store, batch, key string) ledger.Effect {
	t.Helper()
	for _, effect := range must(s.BatchEffects(context.Background(), "r", batch)) {
		if effect.Key == key {
			return effect
		}
	}
	t.Fatalf("batch %s has no effect %s", batch, key)
	return ledger.Effect{}
}

func effectKeys(t *testing.T, s *ledger.Store, batch string) []string {
	t.Helper()
	keys := []string{}
	for _, effect := range must(s.BatchEffects(context.Background(), "r", batch)) {
		keys = append(keys, effect.Key)
	}
	return keys
}

// pendingOf returns the pending effects of batch id in ListActiveBatches, and whether it lists the batch at all.
func pendingOf(t *testing.T, s *ledger.Store, id string) ([]string, bool) {
	t.Helper()
	for _, batch := range must(s.ListActiveBatches(context.Background(), "r")) {
		if batch.ID == id {
			return batch.PendingEffects, true
		}
	}
	return nil, false
}

func eventsOf(t *testing.T, s *ledger.Store, subject, kind string) []ledger.Event {
	t.Helper()
	events := []ledger.Event{}
	for _, e := range must(s.Events(context.Background(), "r", 0, subject)) {
		if e.Kind == kind {
			events = append(events, e)
		}
	}
	return events
}

// workItems lists the work items of repository r as "id owner state", by id.
func workItems(t *testing.T, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	rows := must(conn.Query(ctx, `SELECT id || ' ' || owner || ' ' || state FROM work_items WHERE repo = 'r' ORDER BY id`))
	return must(pgx.CollectRows(rows, pgx.RowTo[string]))
}

// ledgerSnapshot is everything an effect call can change: the effects of the named batches, with their outputs, the
// contracts, the event log and the work items.
type ledgerSnapshot struct {
	Effects   map[string][]ledger.Effect
	Outputs   map[string][]ledger.EffectOutput
	Contracts []ledger.Contract
	Events    []ledger.Event
	Items     []string
}

func snapshot(t *testing.T, s *ledger.Store, dsn string, batches ...string) ledgerSnapshot {
	t.Helper()
	ctx := context.Background()
	state := ledgerSnapshot{Effects: map[string][]ledger.Effect{}, Outputs: map[string][]ledger.EffectOutput{}}
	for _, batch := range batches {
		effects := must(s.BatchEffects(ctx, "r", batch))
		state.Effects[batch] = effects
		for _, effect := range effects {
			state.Outputs[batch+" "+effect.Key] = must(s.BatchEffectOutputs(ctx, "r", batch, effect.Key))
		}
	}
	state.Contracts = must(s.Contracts(ctx, "r"))
	state.Events = must(s.Events(ctx, "r", 0, ""))
	state.Items = workItems(t, dsn)
	return state
}

func assertLedgerUnchanged(t *testing.T, s *ledger.Store, dsn string, before ledgerSnapshot) {
	t.Helper()
	batches := []string{}
	for batch := range before.Effects {
		batches = append(batches, batch)
	}
	if after := snapshot(t, s, dsn, batches...); !reflect.DeepEqual(after, before) {
		t.Fatalf("the ledger changed:\n got %+v\nwant %+v", after, before)
	}
}

func wantNotFound(t *testing.T, err error, what string) {
	t.Helper()
	var notFound *ledger.NotFoundError
	if !errors.As(err, &notFound) || notFound.What != what {
		t.Fatalf("want a missing %s, got %v", what, err)
	}
}

// Schema 0006's batch_effects holds what the effect log reads and writes, and no definition digest: nothing computes or
// reads one.
func TestBatchEffectsTableHasTheEffectLogColumns(t *testing.T) {
	ctx := context.Background()
	_, dsn := newStoreDSN(t)
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	rows := must(conn.Query(ctx, `SELECT column_name::text FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'batch_effects' ORDER BY ordinal_position`))
	columns := must(pgx.CollectRows(rows, pgx.RowTo[string]))
	want := []string{"repo", "batch", "key", "member", "op", "kind", "state", "held", "attempts", "last_attempt", "result", "at"}
	if !slices.Equal(columns, want) {
		t.Fatalf("batch_effects columns = %q, want %q", columns, want)
	}
}

// An attempt records a failed run of an intent: the count rises, the result becomes last_attempt, and the effect stays
// an intent that keeps its batch listed. A done then completes it with its result, and the batch leaves the list.
func TestBatchEffectAttemptThenDone(t *testing.T) {
	ctx := context.Background()
	s, _ := effectFixture(t)
	intent := effectOf(t, s, "batch-1", hookKey)
	if intent.Op != "hook" || intent.State != ledger.EffectIntent || intent.Attempts != 0 || intent.Held ||
		string(intent.LastAttempt) != "null" || !sameJSON(t, intent.Result, jsonValue(map[string]any{"commit": landingCommit})) ||
		len(intent.Outputs) != 0 {
		t.Fatalf("the intent LandBatch records is %+v", intent)
	}

	failure := jsonValue(map[string]any{"outcome": "timeout", "seconds": 600})
	attempted := must(s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey,
		ledger.AttemptRequest{Result: failure, EscalateTo: escalationOwner, EscalateAfter: 10}))
	if attempted.State != ledger.EffectIntent || attempted.Attempts != 1 || attempted.Held ||
		!sameJSON(t, attempted.LastAttempt, failure) || !sameJSON(t, attempted.Result, intent.Result) {
		t.Fatalf("after one attempt the effect is %+v", attempted)
	}
	if stored := effectOf(t, s, "batch-1", hookKey); !reflect.DeepEqual(stored, attempted) {
		t.Fatalf("the attempt returned %+v, but the ledger stores %+v", attempted, stored)
	}
	if pending, listed := pendingOf(t, s, "batch-1"); !listed || !slices.Equal(pending, []string{hookKey}) {
		t.Fatalf("after an attempt the batch is listed=%v with pending effects %q", listed, pending)
	}

	result := jsonValue(map[string]any{"outcome": "exit", "exit": 0})
	done := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{Result: result}))
	if done.State != ledger.EffectDone || done.Attempts != 1 || !sameJSON(t, done.Result, result) || !sameJSON(t, done.LastAttempt, failure) {
		t.Fatalf("after the done the effect is %+v", done)
	}
	if _, listed := pendingOf(t, s, "batch-1"); listed {
		t.Fatal("a landed batch with no effect left to do is still listed as active")
	}
	if got := eventKinds(t, s, "batch-1"); !slices.Equal(got, []string{"batch.created", "effect.attempted", "effect.done"}) {
		t.Fatalf("batch events = %q", got)
	}
	attempt := eventsOf(t, s, "batch-1", "effect.attempted")[0]
	if !sameJSON(t, attempt.Data, jsonValue(map[string]any{"key": hookKey, "attempts": 1, "result": failure, "escalation": ""})) {
		t.Fatalf("the attempt's event data is %s", attempt.Data)
	}
}

// A done repeated with the same result is the retry of a done whose response was lost: it returns the stored row and
// writes nothing, whatever follow-ons, outputs or escalation it carries.
func TestBatchEffectDoneRepeatReturnsTheStoredRow(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	done := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0})}))
	before := snapshot(t, s, dsn, "batch-1")
	replayed := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{
		Result:     json.RawMessage(`{ "exit": 0, "outcome": "exit" }`),
		Then:       []ledger.FollowOn{{Key: "apply/batch-1/0/report", Op: "apply", Kind: "contracts", Input: "ops.jsonl"}},
		Outputs:    []ledger.OutputFile{{Name: "ops.jsonl", Bytes: []byte("{}\n")}},
		EscalateTo: escalationOwner,
	}))
	if !reflect.DeepEqual(replayed, done) {
		t.Fatalf("the repeat returned %+v, want the stored %+v", replayed, done)
	}
	assertLedgerUnchanged(t, s, dsn, before)
}

// On a done row, a done with another result and an attempt are each refused with effect_done, because another
// executor finished the effect first, and neither changes anything: the effect never goes back to intent.
func TestBatchEffectCallsOnADoneRowAreRefused(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0})}))
	before := snapshot(t, s, dsn, "batch-1")
	_, err := s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 1})})
	wantRefusal(t, err, ledger.ReasonEffectDone)
	_, err = s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, ledger.AttemptRequest{
		Result: jsonValue(map[string]any{"outcome": "exit", "exit": 75}), EscalateTo: escalationOwner, EscalateAfter: 1,
	})
	wantRefusal(t, err, ledger.ReasonEffectDone)
	assertLedgerUnchanged(t, s, dsn, before)
	if _, listed := pendingOf(t, s, "batch-1"); listed {
		t.Fatal("a refused attempt listed the batch as active again")
	}
}

// Every effect call on a key that names no effect of the batch, or on a batch that does not exist, is not found and
// changes nothing.
func TestBatchEffectCallsOnAnUnknownEffectAreNotFound(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	before := snapshot(t, s, dsn, "batch-1")
	result := jsonValue(map[string]any{"outcome": "exit", "exit": 0})
	for _, target := range []struct{ batch, key string }{{"batch-1", "hook/batch-1/9/missing"}, {"batch-2", hookKey}} {
		_, err := s.BatchEffectAttempt(ctx, "r", target.batch, target.key, ledger.AttemptRequest{Result: result, EscalateTo: escalationOwner, EscalateAfter: 1})
		wantNotFound(t, err, "effect")
		_, err = s.BatchEffectDone(ctx, "r", target.batch, target.key, ledger.DoneRequest{Result: result, EscalateTo: escalationOwner})
		wantNotFound(t, err, "effect")
		_, err = s.BatchEffectSkip(ctx, "r", target.batch, target.key, "not needed")
		wantNotFound(t, err, "effect")
		_, err = s.BatchEffectRelease(ctx, "r", target.batch, target.key, "fixed")
		wantNotFound(t, err, "effect")
		_, err = s.BatchEffectApplyContracts(ctx, "r", target.batch, target.key)
		wantNotFound(t, err, "effect")
		_, err = s.BatchEffectOutputs(ctx, "r", target.batch, target.key)
		wantNotFound(t, err, "effect")
	}
	_, err := s.BatchEffects(ctx, "r", "batch-2")
	wantNotFound(t, err, "batch")
	_, err = s.BeginWithdraw(ctx, "r", "batch-2")
	wantNotFound(t, err, "batch")
	_, err = s.BatchEffects(ctx, "missing", "batch-1")
	wantNotFound(t, err, "repository")
	assertLedgerUnchanged(t, s, dsn, before)
}

// A done stores its outputs, each with the SHA-256 of its bytes, and creates its follow-ons in the same call. Each
// follow-on is an intent with the hook's landing commit, the hook's key and the output it reads; a follow-on whose key
// already names an effect leaves that effect as it was. A repeat creates nothing more.
func TestBatchEffectDoneStoresItsOutputsAndFollowOns(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	hookIntent(t, dsn, "batch-1", "hook/batch-1/1/score", "other-commit")
	existing := effectOf(t, s, "batch-1", "hook/batch-1/1/score")
	ops := []byte(`{"op":"register","name":"C","owner":"alice","statement_hash":"h1","version_read":0}` + "\n")
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	request := ledger.DoneRequest{
		Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
		Then: []ledger.FollowOn{
			{Key: "apply/batch-1/0/report", Op: "apply", Kind: "contracts", Input: "ops.jsonl"},
			{Key: "publish/batch-1/0/report", Op: "publish"},
			{Key: "hook/batch-1/1/score", Op: "apply", Kind: "score", Input: "report.bin"},
		},
		Outputs: []ledger.OutputFile{{Name: "report.bin", Bytes: binary}, {Name: "ops.jsonl", Bytes: ops}},
	}
	done := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, request))
	wantDigests := []ledger.OutputDigest{{Name: "ops.jsonl", SHA256: sha256Hex(ops)}, {Name: "report.bin", SHA256: sha256Hex(binary)}}
	if done.State != ledger.EffectDone || !reflect.DeepEqual(done.Outputs, wantDigests) {
		t.Fatalf("the done hook is %+v, want its outputs %+v", done, wantDigests)
	}
	outputs := must(s.BatchEffectOutputs(ctx, "r", "batch-1", hookKey))
	wantOutputs := []ledger.EffectOutput{{Name: "ops.jsonl", SHA256: sha256Hex(ops), Bytes: ops}, {Name: "report.bin", SHA256: sha256Hex(binary), Bytes: binary}}
	if !reflect.DeepEqual(outputs, wantOutputs) {
		t.Fatalf("the stored outputs are %+v, want %+v", outputs, wantOutputs)
	}
	for _, follow := range []struct{ key, op, kind, input string }{
		{"apply/batch-1/0/report", "apply", "contracts", "ops.jsonl"},
		{"publish/batch-1/0/report", "publish", "", ""},
	} {
		effect := effectOf(t, s, "batch-1", follow.key)
		if effect.Op != follow.op || effect.Kind != follow.kind || effect.State != ledger.EffectIntent || effect.Attempts != 0 ||
			!sameJSON(t, effect.Result, jsonValue(map[string]any{"commit": landingCommit, "hook": hookKey, "input": follow.input})) {
			t.Fatalf("the follow-on %s is %+v", follow.key, effect)
		}
	}
	if stored := effectOf(t, s, "batch-1", "hook/batch-1/1/score"); !reflect.DeepEqual(stored, existing) {
		t.Fatalf("a follow-on changed the existing effect it names:\n got %+v\nwant %+v", stored, existing)
	}
	if pending, _ := pendingOf(t, s, "batch-1"); !slices.Equal(pending, []string{"apply/batch-1/0/report", "hook/batch-1/1/score", "publish/batch-1/0/report"}) {
		t.Fatalf("pending effects after the done = %q", pending)
	}
	doneEvent := eventsOf(t, s, "batch-1", "effect.done")
	if len(doneEvent) != 1 || !sameJSON(t, doneEvent[0].Data, jsonValue(map[string]any{
		"key": hookKey, "follow_ons": []string{"apply/batch-1/0/report", "publish/batch-1/0/report"},
		"outputs": wantDigests, "escalation": "",
	})) {
		t.Fatalf("the done's events = %+v", doneEvent)
	}

	before := snapshot(t, s, dsn, "batch-1")
	if replayed := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, request)); !reflect.DeepEqual(replayed, done) {
		t.Fatalf("the repeat returned %+v, want %+v", replayed, done)
	}
	assertLedgerUnchanged(t, s, dsn, before)
}

// Attempts up to escalate_after enqueue landing:<batch>:<key>@<n> once, with the attempt that reaches the threshold;
// the attempts before it and after it enqueue nothing. With no threshold (0), no attempt escalates.
func TestBatchEffectAttemptsEscalateOnceAtTheThreshold(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	item := "landing:batch-1:" + hookKey + "@3"
	for n := 1; n <= 4; n++ {
		attempted := must(s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, ledger.AttemptRequest{
			Result: jsonValue(map[string]any{"outcome": "exit", "exit": 75, "run": n}), EscalateTo: escalationOwner, EscalateAfter: 3,
		}))
		if attempted.Attempts != int64(n) || attempted.Held || attempted.State != ledger.EffectIntent {
			t.Fatalf("after attempt %d the effect is %+v", n, attempted)
		}
		wantItems := []string{}
		if n >= 3 {
			wantItems = []string{item + " " + escalationOwner + " open"}
		}
		if got := workItems(t, dsn); !slices.Equal(got, wantItems) {
			t.Fatalf("after attempt %d the work items are %q, want %q", n, got, wantItems)
		}
	}
	if got := eventKinds(t, s, item); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q", got)
	}
	attempts := eventsOf(t, s, "batch-1", "effect.attempted")
	if len(attempts) != 4 || !sameJSON(t, attempts[2].Data, jsonValue(map[string]any{
		"key": hookKey, "attempts": 3, "result": map[string]any{"outcome": "exit", "exit": 75, "run": 3}, "escalation": item,
	})) {
		t.Fatalf("attempt events = %+v", attempts)
	}

	hookIntent(t, dsn, "batch-1", "hook/batch-1/1/score", landingCommit)
	for range 5 {
		must(s.BatchEffectAttempt(ctx, "r", "batch-1", "hook/batch-1/1/score", ledger.AttemptRequest{
			Result: jsonValue(map[string]any{"outcome": "exit", "exit": 75}), EscalateTo: escalationOwner,
		}))
	}
	if got := workItems(t, dsn); !slices.Equal(got, []string{item + " " + escalationOwner + " open"}) {
		t.Fatalf("attempts with no threshold enqueued %q", got)
	}
}

// A deterministic attempt (result.deterministic is true) holds the effect and escalates at once. The effect stays an
// intent and keeps its batch listed. A release clears the hold, with an event that names the operator's login and the
// reason; a repeat of the release changes nothing. A second deterministic attempt holds it again and gets its own
// escalation item. A deterministic field that is not the JSON true holds nothing.
func TestDeterministicAttemptHoldsTheEffectAndEscalatesAtOnce(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	operator, operatorDSN := testpg.Login(t, dsn, "ledger_operator")
	operatorStore := must(ledger.Open(ctx, operatorDSN))
	defer operatorStore.Close()
	deterministic := ledger.AttemptRequest{
		Result:     jsonValue(map[string]any{"deterministic": true, "reason": "output_too_large"}),
		EscalateTo: escalationOwner, EscalateAfter: 10,
	}
	held := must(s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, deterministic))
	if !held.Held || held.State != ledger.EffectIntent || held.Attempts != 1 {
		t.Fatalf("after a deterministic attempt the effect is %+v", held)
	}
	first := "landing:batch-1:" + hookKey + "@1 " + escalationOwner + " open"
	if got := workItems(t, dsn); !slices.Equal(got, []string{first}) {
		t.Fatalf("after a deterministic attempt the work items are %q", got)
	}
	if pending, listed := pendingOf(t, s, "batch-1"); !listed || !slices.Equal(pending, []string{hookKey}) {
		t.Fatalf("a held effect's batch is listed=%v with pending %q", listed, pending)
	}

	released := must(operatorStore.BatchEffectRelease(ctx, "r", "batch-1", hookKey, "raised max_output_bytes"))
	if released.Held || released.State != ledger.EffectIntent || released.Attempts != 1 {
		t.Fatalf("after the release the effect is %+v", released)
	}
	releases := eventsOf(t, s, "batch-1", "effect.released")
	if len(releases) != 1 || releases[0].Login != operator ||
		!sameJSON(t, releases[0].Data, jsonValue(map[string]any{"key": hookKey, "reason": "raised max_output_bytes"})) {
		t.Fatalf("release events = %+v, want one naming login %s and the reason", releases, operator)
	}
	before := snapshot(t, s, dsn, "batch-1")
	if repeated := must(operatorStore.BatchEffectRelease(ctx, "r", "batch-1", hookKey, "raised max_output_bytes")); !reflect.DeepEqual(repeated, released) {
		t.Fatalf("the repeated release returned %+v, want %+v", repeated, released)
	}
	assertLedgerUnchanged(t, s, dsn, before)

	again := must(s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, deterministic))
	if !again.Held || again.Attempts != 2 {
		t.Fatalf("after a second deterministic attempt the effect is %+v", again)
	}
	second := "landing:batch-1:" + hookKey + "@2 " + escalationOwner + " open"
	if got := workItems(t, dsn); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("after the second deterministic attempt the work items are %q", got)
	}

	hookIntent(t, dsn, "batch-1", "hook/batch-1/1/score", landingCommit)
	notHeld := must(s.BatchEffectAttempt(ctx, "r", "batch-1", "hook/batch-1/1/score", ledger.AttemptRequest{
		Result: jsonValue(map[string]any{"deterministic": "true"}), EscalateTo: escalationOwner, EscalateAfter: 10,
	}))
	if notHeld.Held || len(workItems(t, dsn)) != 2 {
		t.Fatalf("a deterministic field of \"true\" held the effect %+v or escalated it: %q", notHeld, workItems(t, dsn))
	}
}

// A done with escalate_to enqueues landing:<batch>:<key> for that owner in the done's own transaction, and its repeat
// enqueues nothing. When that item already belongs to another owner, Enqueue's rule refuses it, and the done is
// refused with it and changes nothing: the escalation and the completion commit together or not at all.
func TestBatchEffectDoneWithEscalateToEnqueuesItsItem(t *testing.T) {
	ctx := context.Background()
	s, dsn := effectFixture(t)
	request := ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 1}), EscalateTo: escalationOwner}
	done := must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, request))
	item := "landing:batch-1:" + hookKey
	if got := workItems(t, dsn); done.State != ledger.EffectDone || !slices.Equal(got, []string{item + " " + escalationOwner + " open"}) {
		t.Fatalf("after a done with escalate_to: effect %+v, work items %q", done, got)
	}
	must(s.BatchEffectDone(ctx, "r", "batch-1", hookKey, request))
	if got := eventKinds(t, s, item); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events after the repeat = %q", got)
	}

	hookIntent(t, dsn, "batch-1", "hook/batch-1/1/score", landingCommit)
	must(0, s.Enqueue(ctx, "r", "landing:batch-1:hook/batch-1/1/score", "owner:other"))
	before := snapshot(t, s, dsn, "batch-1")
	_, err := s.BatchEffectDone(ctx, "r", "batch-1", "hook/batch-1/1/score", request)
	wantRefusal(t, err, ledger.ReasonExists)
	assertLedgerUnchanged(t, s, dsn, before)
}

// Two batches of one repository with the same effect key each attempt and complete their own effect, and neither
// changes the other's.
func TestEffectKeysAreScopedToTheirBatch(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	landedBatch(t, s, dsn, "batch-1", "score")
	landedBatch(t, s, dsn, "batch-2", "score")
	second := effectOf(t, s, "batch-2", "score")
	attempted := must(s.BatchEffectAttempt(ctx, "r", "batch-1", "score", ledger.AttemptRequest{
		Result: jsonValue(map[string]any{"outcome": "exit", "exit": 75}), EscalateTo: escalationOwner, EscalateAfter: 1,
	}))
	if attempted.Attempts != 1 || !reflect.DeepEqual(effectOf(t, s, "batch-2", "score"), second) {
		t.Fatalf("an attempt on batch-1's score changed batch-2's: %+v", effectOf(t, s, "batch-2", "score"))
	}
	if got := workItems(t, dsn); !slices.Equal(got, []string{"landing:batch-1:score@1 " + escalationOwner + " open"}) {
		t.Fatalf("work items = %q", got)
	}
	secondResult := jsonValue(map[string]any{"outcome": "pushed", "commit": "score-2"})
	must(s.BatchEffectDone(ctx, "r", "batch-2", "score", ledger.DoneRequest{Result: secondResult}))
	if first := effectOf(t, s, "batch-1", "score"); !reflect.DeepEqual(first, attempted) {
		t.Fatalf("a done on batch-2's score changed batch-1's: %+v", first)
	}
	firstResult := jsonValue(map[string]any{"outcome": "pushed", "commit": "score-1"})
	must(s.BatchEffectDone(ctx, "r", "batch-1", "score", ledger.DoneRequest{Result: firstResult}))
	if first, other := effectOf(t, s, "batch-1", "score"), effectOf(t, s, "batch-2", "score"); !sameJSON(t, first.Result, firstResult) ||
		!sameJSON(t, other.Result, secondResult) || other.Attempts != 0 {
		t.Fatalf("after both dones: batch-1 %+v, batch-2 %+v", first, other)
	}
}

// BeginWithdraw records withdraw/<id> as an intent of the batch, and a repeat returns the stored row and writes
// nothing. The batch stays listed while the withdraw is an intent, and the withdraw completes like any effect.
func TestBeginWithdrawCreatesTheWithdrawIntentOnce(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	createBatch(t, s, "batch-1", batchRecord("publishing"))
	setAdmission(t, dsn, "batch-1", "admitted", "")
	withdraw := must(s.BeginWithdraw(ctx, "r", "batch-1"))
	if withdraw.Key != "withdraw/batch-1" || withdraw.Op != "withdraw" || withdraw.State != ledger.EffectIntent ||
		withdraw.Attempts != 0 || !sameJSON(t, withdraw.Result, json.RawMessage(`{}`)) {
		t.Fatalf("the withdraw intent is %+v", withdraw)
	}
	if got := eventKinds(t, s, "batch-1"); !slices.Equal(got, []string{"batch.created", "effect.created"}) {
		t.Fatalf("batch events = %q", got)
	}
	before := snapshot(t, s, dsn, "batch-1")
	if repeated := must(s.BeginWithdraw(ctx, "r", "batch-1")); !reflect.DeepEqual(repeated, withdraw) {
		t.Fatalf("the repeat returned %+v, want %+v", repeated, withdraw)
	}
	assertLedgerUnchanged(t, s, dsn, before)
	if pending, _ := pendingOf(t, s, "batch-1"); !slices.Equal(pending, []string{"withdraw/batch-1"}) {
		t.Fatalf("pending effects = %q", pending)
	}
	if done := must(s.BatchEffectDone(ctx, "r", "batch-1", "withdraw/batch-1", ledger.DoneRequest{
		Result: jsonValue(map[string]any{"outcome": "withdrawn"}),
	})); done.State != ledger.EffectDone {
		t.Fatalf("the withdraw after its done is %+v", done)
	}
}

// A skip is a done whose result names the login that ran it and the reason, with an event that names both. It
// creates no follow-on. A repeat of the same skip returns the stored row; a skip with another reason, or by another
// login, is refused as a done with another result. The batch leaves the active list once its other effects are done.
func TestBatchEffectSkipIsADoneThatNamesTheOperator(t *testing.T) {
	ctx := context.Background()
	s, dsn := newStoreDSN(t)
	landedBatch(t, s, dsn, "batch-1", hookKey, "hook/batch-1/1/score")
	operator, operatorDSN := testpg.Login(t, dsn, "ledger_operator")
	operatorStore := must(ledger.Open(ctx, operatorDSN))
	defer operatorStore.Close()

	skipped := must(operatorStore.BatchEffectSkip(ctx, "r", "batch-1", hookKey, "the hook's input is gone"))
	if skipped.State != ledger.EffectDone ||
		!sameJSON(t, skipped.Result, jsonValue(map[string]any{"skipped_by": operator, "reason": "the hook's input is gone"})) {
		t.Fatalf("the skipped effect is %+v", skipped)
	}
	skips := eventsOf(t, s, "batch-1", "effect.skipped")
	if len(skips) != 1 || skips[0].Login != operator ||
		!sameJSON(t, skips[0].Data, jsonValue(map[string]any{"key": hookKey, "reason": "the hook's input is gone"})) {
		t.Fatalf("skip events = %+v, want one naming login %s and the reason", skips, operator)
	}
	if keys := effectKeys(t, s, "batch-1"); !slices.Equal(keys, []string{hookKey, "hook/batch-1/1/score"}) {
		t.Fatalf("after the skip the batch's effects are %q", keys)
	}
	if pending, listed := pendingOf(t, s, "batch-1"); !listed || !slices.Equal(pending, []string{"hook/batch-1/1/score"}) {
		t.Fatalf("after the skip the batch is listed=%v with pending %q", listed, pending)
	}

	before := snapshot(t, s, dsn, "batch-1")
	if repeated := must(operatorStore.BatchEffectSkip(ctx, "r", "batch-1", hookKey, "the hook's input is gone")); !reflect.DeepEqual(repeated, skipped) {
		t.Fatalf("the repeated skip returned %+v, want %+v", repeated, skipped)
	}
	_, err := operatorStore.BatchEffectSkip(ctx, "r", "batch-1", hookKey, "another reason")
	wantRefusal(t, err, ledger.ReasonEffectDone)
	_, err = s.BatchEffectSkip(ctx, "r", "batch-1", hookKey, "the hook's input is gone")
	wantRefusal(t, err, ledger.ReasonEffectDone)
	assertLedgerUnchanged(t, s, dsn, before)

	must(s.BatchEffectDone(ctx, "r", "batch-1", "hook/batch-1/1/score", ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0})}))
	if _, listed := pendingOf(t, s, "batch-1"); listed {
		t.Fatal("the batch is still listed after its last effect was done")
	}
}

// Executors that complete one effect with different results at once commit exactly one done; every other one is
// refused with effect_done. The test holds the effect's row lock until every executor waits on a lock.
func TestConcurrentDonesCompleteAnEffectOnce(t *testing.T) {
	ctx := context.Background()
	_, dsn := newStoreDSN(t)
	const executors = 8
	s := openStoreWithPool(t, dsn, "effect_executors", executors)
	landedBatch(t, s, dsn, "batch-1", hookKey)
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	holder := must(conn.Begin(ctx))
	must(holder.Exec(ctx, `SELECT 1 FROM batch_effects WHERE repo = 'r' AND batch = 'batch-1' AND key = $1 FOR UPDATE`, hookKey))
	var wait sync.WaitGroup
	results := make([]error, executors)
	for i := range executors {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[i] = s.BatchEffectDone(ctx, "r", "batch-1", hookKey, ledger.DoneRequest{Result: jsonValue(map[string]any{"executor": i})})
		}()
	}
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	waitForBlockedConnections(t, observer, executors)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	winners := []int{}
	for i, err := range results {
		if err == nil {
			winners = append(winners, i)
			continue
		}
		wantRefusal(t, err, ledger.ReasonEffectDone)
	}
	if len(winners) != 1 {
		t.Fatalf("%d concurrent dones committed, want 1", len(winners))
	}
	if stored := effectOf(t, s, "batch-1", hookKey); !sameJSON(t, stored.Result, jsonValue(map[string]any{"executor": winners[0]})) {
		t.Fatalf("the stored effect is %+v, want executor %d's result", stored, winners[0])
	}
	if got := len(eventsOf(t, s, "batch-1", "effect.done")); got != 1 {
		t.Fatalf("%d effect.done events, want 1", got)
	}
}

// Two dones of one batch whose follow-ons share keys in opposite orders (x, y and y, x) run at once, and both finish
// with no deadlock: each follow-on is created once. A holder keeps uncommitted rows with both keys until both dones
// wait on them, so both reach their first follow-on together; then it rolls them back.
func TestConcurrentDonesWithFollowOnsInOppositeOrdersDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	// Every session of the database ends before the test reads its deadlock count, so the first store closes now.
	first, dsn := newStoreDSN(t)
	first.Close()
	sized := must(url.Parse(dsn))
	query := sized.Query()
	query.Set("pool_max_conns", "2")
	sized.RawQuery = query.Encode()
	s := must(ledger.Open(ctx, sized.String()))
	landedBatch(t, s, dsn, "batch-1", "hook/0", "hook/1")
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	holderConn := must(pgx.Connect(ctx, dsn))
	holder := must(holderConn.Begin(ctx))
	must(holder.Exec(ctx, `INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'batch-1', 'x', 'hold'), ('r', 'batch-1', 'y', 'hold')`))
	errs := make(chan error, 2)
	for hook, order := range map[string][]string{"hook/0": {"x", "y"}, "hook/1": {"y", "x"}} {
		go func() {
			_, err := s.BatchEffectDone(ctx, "r", "batch-1", hook, ledger.DoneRequest{
				Result: jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
				Then:   []ledger.FollowOn{{Key: order[0], Op: "publish"}, {Key: order[1], Op: "publish"}},
			})
			errs <- err
		}()
	}
	waitForBlockedConnections(t, observer, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	must(0, holderConn.Close(ctx))
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	created := []string{}
	for _, e := range eventsOf(t, s, "batch-1", "effect.done") {
		var data struct {
			FollowOns []string `json:"follow_ons"`
		}
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		created = append(created, data.FollowOns...)
	}
	slices.Sort(created)
	s.Close()
	if deadlocks := databaseDeadlocks(t, observer); deadlocks != 0 {
		t.Fatalf("the two dones deadlocked %d times", deadlocks)
	}
	if !slices.Equal(created, []string{"x", "y"}) {
		t.Fatalf("the dones created the follow-ons %q; want each of x and y created once", created)
	}
}

// Executors that attempt one effect at once each count once, and only the attempt that reaches the threshold
// escalates.
func TestConcurrentAttemptsCountEveryAttempt(t *testing.T) {
	ctx := context.Background()
	_, dsn := newStoreDSN(t)
	const executors = 8
	s := openStoreWithPool(t, dsn, "effect_executors", executors)
	landedBatch(t, s, dsn, "batch-1", hookKey)
	conn := must(pgx.Connect(ctx, dsn))
	defer conn.Close(ctx)
	holder := must(conn.Begin(ctx))
	must(holder.Exec(ctx, `SELECT 1 FROM batch_effects WHERE repo = 'r' AND batch = 'batch-1' AND key = $1 FOR UPDATE`, hookKey))
	var wait sync.WaitGroup
	results := make([]error, executors)
	for i := range executors {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[i] = s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, ledger.AttemptRequest{
				Result: jsonValue(map[string]any{"executor": i}), EscalateTo: escalationOwner, EscalateAfter: executors,
			})
		}()
	}
	observer := must(pgx.Connect(ctx, dsn))
	defer observer.Close(ctx)
	waitForBlockedConnections(t, observer, executors)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if stored := effectOf(t, s, "batch-1", hookKey); stored.Attempts != executors {
		t.Fatalf("after %d concurrent attempts the effect is %+v", executors, stored)
	}
	if got := workItems(t, dsn); !slices.Equal(got, []string{"landing:batch-1:" + hookKey + "@8 " + escalationOwner + " open"}) {
		t.Fatalf("work items after the concurrent attempts = %q", got)
	}
}
