package landing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// containerFake is the container runner of the build VM: it records each run and answers from result.
type containerFake struct {
	runs   []ContainerRun
	result func(ContainerRun) (ContainerResult, error)
}

func (f *containerFake) Run(_ context.Context, run ContainerRun) (ContainerResult, error) {
	f.runs = append(f.runs, run)
	return f.result(run)
}

// names are the hooks each run was for, in the order they ran.
func (f *containerFake) names() []string {
	names := make([]string, 0, len(f.runs))
	for _, run := range f.runs {
		names = append(names, run.Env["LAND_EFFECT_KEY"])
	}
	return names
}

// treesFake is the landed commit's worktree in the program's clone.
type treesFake struct {
	path    string
	removed []string
}

func (f *treesFake) LandedTree(_ context.Context, commit string) (string, error) {
	if f.path == "" {
		f.path = "/srv/land/repo.git/trees/" + commit
	}
	return f.path, nil
}

func (f *treesFake) RemoveLandedTree(_ context.Context, commit string) error {
	f.removed = append(f.removed, commit)
	return nil
}

// scoreFake is the remote score ref: present names the paths it already holds, and pushed the ones an apply added.
type scoreFake struct {
	previous []byte
	present  map[string]string
	pushed   []string
	addErr   error
}

func (f *scoreFake) Latest(context.Context, string, string, string) ([]byte, error) {
	return f.previous, nil
}

func (f *scoreFake) Present(_ context.Context, _, _, path string) (string, error) {
	return f.present[path], nil
}

func (f *scoreFake) Add(_ context.Context, _, _, path string, _ []byte) (string, error) {
	if f.addErr != nil {
		return "", f.addErr
	}
	f.pushed = append(f.pushed, path)
	return "score-commit", nil
}

// defectFake counts the defect messages published, by their id.
type defectFake struct {
	published []string
	topics    []string
	err       error
}

func (f *defectFake) Publish(_ context.Context, topic, id string, _ []byte) error {
	if f.err != nil {
		return f.err
	}
	f.topics, f.published = append(f.topics, topic), append(f.published, id)
	return nil
}

// landedHooks is a landed batch whose hook effects are frozen and recorded as intents, with the fakes the executor
// reads: the runner, the effect log and the container runner.
func landedHooks(t *testing.T, hooks []HookConfig, parts []string, result func(ContainerRun) (ContainerResult, error)) (Runner, *effectLogFake, *containerFake, Batch) {
	t.Helper()
	runner := hookRunner(hooks, parts)
	effects, containers := newEffectLog(), &containerFake{result: result}
	runner.Effects, runner.Containers, runner.Trees = effects, containers, &treesFake{}
	runner.RetryBackoff = time.Nanosecond
	batch := Batch{ID: "batch", Repository: "r", State: Landed, PullRequest: PullRequest{Number: 7, MergedCommit: "merged", State: PullRequestMerged}}
	batch.PostLandEffects = runner.postLandEffects(batch)
	for _, effect := range batch.PostLandEffects {
		effects.create(effect.Key, effect.Op, "", effect.Member, map[string]any{"commit": "merged"})
		batch.PendingEffects = append(batch.PendingEffects, effect.Key)
	}
	slices.Sort(batch.PendingEffects)
	return runner, effects, containers, batch
}

// exits is a container result of the given exit, with a body for each declared output.
func exits(code int, outputs map[string][]byte) func(ContainerRun) (ContainerResult, error) {
	return func(run ContainerRun) (ContainerResult, error) {
		result := ContainerResult{Exit: code, Outputs: map[string][]byte{}}
		for _, name := range run.Outputs {
			body, given := outputs[name]
			if !given {
				body = []byte("{}")
			}
			result.Outputs[name] = body
		}
		return result, nil
	}
}

// A hook that exits 0 is recorded with its outputs, and its apply follows it at once: the score is pushed, the
// contracts are applied, and the effect of each is done. Every hook runs with the landing commit, its part, the
// landed tree and its own effect key, in a container of the frozen image with the frozen mounts.
func TestAHookThatDecidesIsRecordedAndItsApplyFollowsIt(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t,
		[]HookConfig{hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, map[string]MountMode{"index": ReadOnly})},
		[]string{"slice"}, exits(0, map[string][]byte{"ops.jsonl": []byte("{\"op\":\"register\"}\n")}))
	scores := &scoreFake{}
	runner.Scores = scores

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	if got := containers.names(); !slices.Equal(got, []string{"hook/batch/0/slice"}) {
		t.Fatalf("the runs were %q", got)
	}
	run := containers.runs[0]
	if run.Env["LAND_COMMIT"] != "merged" || run.Env["LAND_PART"] != "slice" ||
		run.Env["LAND_TREE"] != "/srv/land/repo.git/trees/merged" || run.Env["LAND_GIT_DIR"] != "/srv/land/repo.git" {
		t.Fatalf("the hook's environment is %v", run.Env)
	}
	if run.Image != "image@sha256:one" || run.User != "1002:1003" || run.Network != NetworkNone ||
		!slices.Equal(run.Mounts, []Mount{{Name: "index", HostPath: "/srv/land/index", Mode: ReadOnly}}) {
		t.Fatalf("the hook's container is %+v", run)
	}
	if string(run.Inputs[ContractsInput]) != "[]" {
		t.Fatalf("the contracts input is %q, want the ledger's contract list", run.Inputs[ContractsInput])
	}
	if !slices.Equal(effects.applied, []string{"apply/batch/0/slice"}) {
		t.Fatalf("the applies that ran are %q", effects.applied)
	}
	if got := effects.pending(); len(got) != 0 {
		t.Fatalf("effects still pending: %q", got)
	}
	if string(effects.outputs["hook/batch/0/slice"]["ops.jsonl"]) == "" {
		t.Fatal("the hook's output was not stored in the ledger")
	}
}

// A hook whose database is unreachable exits 75: that is "retry later", not a verdict. Each failed run is an
// attempt, the ledger escalates once when the attempts reach the frozen threshold, nothing is applied, and the
// batch's later hooks do not run. After an operator skips the effect, the later hook runs.
func TestAHookThatCannotReachItsDatabaseIsRetriedAndThenEscalated(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t, []HookConfig{
		hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil),
		hookConfig("score", ApplyScore, []string{"score.json"}, nil),
	}, []string{"slice"}, exits(75, nil))
	runner.Scores = &scoreFake{}

	for round := 1; round <= 5; round++ {
		err := runner.finishEffects(ctx, &batch)
		if !errors.Is(err, ErrEffectsPending) {
			t.Fatalf("round %d: finishEffects() error = %v, want the effects still pending", round, err)
		}
	}
	if got := containers.names(); len(got) != 5 || slices.Contains(got, "hook/batch/1/slice") {
		t.Fatalf("the runs were %q, want five of the first hook and none of the second", got)
	}
	if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice@3 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want exactly %q", got, want)
	}
	if len(effects.applied) != 0 || len(effects.outputs) != 0 {
		t.Fatalf("a hook that never decided applied %q and stored %v", effects.applied, effects.outputs)
	}
	// The operator skips the held effect, as `ledger effect-skip` does, and the later hook runs.
	effects.effects["hook/batch/0/slice"].State = EffectDone
	containers.result = exits(0, nil)
	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("after the skip: finishEffects() error = %v", err)
	}
	if got := containers.names(); got[len(got)-1] != "hook/batch/1/slice" {
		t.Fatalf("after the skip the runs were %q, want the second hook last", got)
	}
}

// A defect (exit 1) is recorded, published once with the effect's key as the message id, escalated once, and its
// output is still applied: the score of a defective landing is still committed.
func TestADefectIsRecordedPublishedEscalatedAndStillApplied(t *testing.T) {
	ctx := context.Background()
	hook := hookConfig("score", ApplyScore, []string{"score.json"}, nil)
	hook.Publish = "notifications.defects"
	runner, effects, _, batch := landedHooks(t, []HookConfig{hook}, []string{"slice"}, exits(1, nil))
	scores, defects := &scoreFake{}, &defectFake{}
	runner.Scores, runner.Defects = scores, defects

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	if !slices.Equal(defects.published, []string{"publish/batch/0/slice"}) || !slices.Equal(defects.topics, []string{"notifications.defects"}) {
		t.Fatalf("published %q on %q", defects.published, defects.topics)
	}
	if !slices.Equal(scores.pushed, []string{"slice/merged.json"}) {
		t.Fatalf("the score paths pushed are %q", scores.pushed)
	}
	if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want %q", got, want)
	}
	if got := effects.pending(); len(got) != 0 {
		t.Fatalf("effects still pending: %q", got)
	}
}

// Exit 2 and exit 3 are "unable to check": the run is recorded and escalated, and nothing is applied.
func TestAHookThatCouldNotCheckIsRecordedAndEscalatedWithNothingApplied(t *testing.T) {
	for _, code := range []int{2, 3} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			ctx := context.Background()
			runner, effects, _, batch := landedHooks(t,
				[]HookConfig{hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil)},
				[]string{"slice"}, exits(code, nil))
			if err := runner.finishEffects(ctx, &batch); err != nil {
				t.Fatalf("finishEffects() error = %v", err)
			}
			if len(effects.applied) != 0 {
				t.Fatalf("an unable hook applied %q", effects.applied)
			}
			if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice escalations"}; !slices.Equal(got, want) {
				t.Fatalf("escalations = %q, want %q", got, want)
			}
		})
	}
}

// A missing or oversize output is a deterministic failure: the effect is escalated at once and held, it is not run
// again, and no apply is created. After an operator releases it, a run that leaves the output applies it.
func TestAMissingOutputHoldsTheEffectUntilAnOperatorReleasesIt(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t,
		[]HookConfig{hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil)}, []string{"slice"},
		func(ContainerRun) (ContainerResult, error) {
			return ContainerResult{Exit: 0, Missing: []string{"ops.jsonl"}}, nil
		})

	for round := 1; round <= 3; round++ {
		if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
			t.Fatalf("round %d: finishEffects() error = %v", round, err)
		}
	}
	if got := containers.names(); len(got) != 1 {
		t.Fatalf("a held effect ran %d times, want once", len(got))
	}
	if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice@1 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want one at once", got)
	}
	if len(effects.applied) != 0 {
		t.Fatalf("a held hook applied %q", effects.applied)
	}
	// The operator releases the effect after fixing its cause, and the rerun applies.
	effects.effects["hook/batch/0/slice"].Held = false
	containers.result = exits(0, nil)
	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("after the release: finishEffects() error = %v", err)
	}
	if !slices.Equal(effects.applied, []string{"apply/batch/0/slice"}) {
		t.Fatalf("after the release the applies are %q", effects.applied)
	}
}

// The hooks of a batch run in the frozen order, by the hook's index and then the part's position, never by sorting
// their keys, and the batch's landed tree is removed only when every effect is done.
func TestTheHooksOfABatchRunInTheFrozenOrder(t *testing.T) {
	ctx := context.Background()
	hooks := make([]HookConfig, 0, 12)
	for index := range 12 {
		hooks = append(hooks, hookConfig(fmt.Sprintf("hook%d", index), "", nil, nil))
	}
	runner, _, containers, batch := landedHooks(t, hooks, []string{"slice", "all"}, exits(0, nil))
	trees := runner.Trees.(*treesFake)

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	want := make([]string, 0, 24)
	for index := range 12 {
		want = append(want, fmt.Sprintf("hook/batch/%d/slice", index), fmt.Sprintf("hook/batch/%d/all", index))
	}
	if got := containers.names(); !slices.Equal(got, want) {
		t.Fatalf("the hooks ran as %q, want %q", got, want)
	}
	if !slices.Equal(trees.removed, []string{"merged"}) {
		t.Fatalf("the landed trees removed are %q, want the landing's once", trees.removed)
	}
}

// A crash after a hook's done and before its score push finishes the apply on the restart, and a score path that is
// already on the ref was pushed by a run whose done the crash lost, so nothing is pushed twice.
func TestAnApplyIsFinishedAfterACrashAndPushesOnlyOnce(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t,
		[]HookConfig{hookConfig("score", ApplyScore, []string{"score.json"}, nil)}, []string{"slice"}, exits(0, nil))
	scores := &scoreFake{}
	runner.Scores = scores
	// The hook is done and its apply is the intent a crash left behind.
	effects.effects["hook/batch/0/slice"].State = EffectDone
	effects.outputs["hook/batch/0/slice"] = map[string][]byte{"score.json": []byte("{}")}
	effects.create("apply/batch/0/slice", OpApply, ApplyScore, "",
		map[string]any{"hook": "hook/batch/0/slice", "input": "score.json", "commit": "merged"})

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	if len(containers.runs) != 0 {
		t.Fatalf("a done hook ran again: %q", containers.names())
	}
	if !slices.Equal(scores.pushed, []string{"slice/merged.json"}) {
		t.Fatalf("the score paths pushed are %q", scores.pushed)
	}
	// A crash before the apply's done leaves the path on the ref: the retry finds it and pushes nothing.
	scores.present = map[string]string{"slice/merged.json": "blob-1"}
	effects.effects["apply/batch/0/slice"].State = EffectIntent
	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("after the crash: finishEffects() error = %v", err)
	}
	if len(scores.pushed) != 1 {
		t.Fatalf("the score was pushed %d times, want once", len(scores.pushed))
	}
}

// Each near pair of a contracts apply is delivered to the librarian once, with its id and proposer and a 0600
// proposal file, after the apply and before the next hook. The file is gone after the run.
func TestEachNearPairIsDeliveredToTheLibrarianOnceBeforeTheNextHook(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t, []HookConfig{
		hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil),
		hookConfig("score", "", nil, nil),
	}, []string{"slice"}, exits(0, nil))
	record := filepath.Join(t.TempDir(), "deliveries")
	runner.Librarian = []string{"sh", "-c",
		`printf '%s %s %s %s %s\n' "$2" "$4" "$6" "$(stat -c %a "$6")" "$(cat "$6")" >> ` + record, "librarian"}
	for _, effect := range batch.PostLandEffects {
		if effect.Definition != nil && effect.Definition.Apply == ApplyContracts {
			effect.Definition.Librarian = runner.Librarian
		}
	}
	// The apply creates one librarian intent per near pair, as the ledger does from the hook's emit file.
	effects.applyErr = nil
	deliver := func() {
		for _, id := range []string{"pair-1", "pair-2"} {
			effects.create("librarian/batch/"+id, OpLibrarian, "", "", map[string]any{
				"hook": "hook/batch/0/slice", "commit": "merged",
				"line": map[string]any{"op": "librarian", "id": id, "proposer": "owner:alice", "envelope": "envelope " + id},
			})
		}
	}
	effects.onApply = deliver

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the deliveries: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pair-1 owner:alice ") || !strings.HasSuffix(lines[0], " 600 envelope pair-1") ||
		!strings.HasPrefix(lines[1], "pair-2 owner:alice ") || !strings.HasSuffix(lines[1], " 600 envelope pair-2") {
		t.Fatalf("the deliveries were %q", lines)
	}
	proposal := strings.Fields(lines[0])[2]
	if _, err := os.Stat(proposal); !os.IsNotExist(err) {
		t.Fatalf("the proposal file %s is still there: %v", proposal, err)
	}
	if got := containers.names(); !slices.Equal(got, []string{"hook/batch/0/slice", "hook/batch/1/slice"}) {
		t.Fatalf("the hooks ran as %q", got)
	}
	// The deliveries come before the second hook: it is pending while a delivery is.
	if got := effects.doneOrder; !slices.Contains(got, "librarian/batch/pair-2") ||
		slices.Index(got, "librarian/batch/pair-2") > slices.Index(got, "hook/batch/1/slice") {
		t.Fatalf("the effects finished in the order %q", got)
	}
}

// A librarian command that always fails is an attempt each time, escalated once at the frozen threshold, and the
// batch's later hooks wait behind it.
func TestALibrarianThatAlwaysFailsEscalatesOnceAndHoldsTheLaterHooks(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t, []HookConfig{
		hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil),
		hookConfig("score", "", nil, nil),
	}, []string{"slice"}, exits(0, nil))
	runner.Librarian = []string{"false"}
	for _, effect := range batch.PostLandEffects {
		if effect.Definition != nil && effect.Definition.Apply == ApplyContracts {
			effect.Definition.Librarian = runner.Librarian
		}
	}
	effects.onApply = func() {
		effects.create("librarian/batch/pair-1", OpLibrarian, "", "", map[string]any{
			"hook": "hook/batch/0/slice", "commit": "merged",
			"line": map[string]any{"op": "librarian", "id": "pair-1", "proposer": "owner:alice", "envelope": "envelope"},
		})
	}

	for round := 1; round <= 4; round++ {
		if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
			t.Fatalf("round %d: finishEffects() error = %v", round, err)
		}
	}
	if got, want := effects.escalations, []string{"landing:batch:librarian/batch/pair-1@3 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want %q", got, want)
	}
	if got := containers.names(); slices.Contains(got, "hook/batch/1/slice") {
		t.Fatalf("the second hook ran behind a failing delivery: %q", got)
	}
}

// A host-side input that fails makes the hook "unable to check": it is recorded, escalated and not retried, and the
// hook never runs.
func TestAFailedHostInputIsRecordedAndNotRetried(t *testing.T) {
	ctx := context.Background()
	hook := hookConfig("score", ApplyScore, []string{"score.json"}, nil)
	hook.Inputs = []HookInput{{Name: "cost.json", Command: []string{"/bin/false"}}}
	runner, effects, containers, batch := landedHooks(t, []HookConfig{hook}, []string{"slice"}, exits(0, nil))
	runner.Scores = &scoreFake{}

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	if len(containers.runs) != 0 {
		t.Fatalf("the hook ran with a failed input: %q", containers.names())
	}
	if got, want := effects.escalations, []string{"landing:batch:hook/batch/0/slice escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want %q", got, want)
	}
	var result struct {
		Outcome string `json:"outcome"`
		Input   string `json:"input"`
	}
	if err := json.Unmarshal(effects.effects["hook/batch/0/slice"].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "input_failed" || result.Input != "cost.json" {
		t.Fatalf("the recorded result is %+v", result)
	}
}

// The program writes the two inputs itself: the newest score of the part for a score hook, and the ledger's
// contract list for a contracts hook, each before the run.
func TestTheProgramWritesThePreviousScoreAndTheContractList(t *testing.T) {
	ctx := context.Background()
	runner, _, containers, batch := landedHooks(t, []HookConfig{
		hookConfig("score", ApplyScore, []string{"score.json"}, nil),
	}, []string{"slice"}, exits(0, nil))
	runner.Scores = &scoreFake{previous: []byte(`{"score":1}`)}

	if err := runner.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("finishEffects() error = %v", err)
	}
	if got := string(containers.runs[0].Inputs[PreviousInput]); got != `{"score":1}` {
		t.Fatalf("the previous score input is %q", got)
	}
}

// A timeout is an attempt, retried with backoff, and the hook's outputs are never applied.
func TestATimedOutHookIsAnAttempt(t *testing.T) {
	ctx := context.Background()
	runner, effects, _, batch := landedHooks(t,
		[]HookConfig{hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil)}, []string{"slice"},
		func(ContainerRun) (ContainerResult, error) { return ContainerResult{TimedOut: true}, nil })

	if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
		t.Fatalf("finishEffects() error = %v, want the effect pending", err)
	}
	if effects.effects["hook/batch/0/slice"].Attempts != 1 || len(effects.applied) != 0 {
		t.Fatalf("after a timeout the effect is %+v", effects.effects["hook/batch/0/slice"])
	}
}

// A contract conflict applies nothing and would fail the same way on a rerun, so the apply is held and escalated
// once, and the batch's later hooks wait behind it.
func TestAContractConflictHoldsTheApplyAndEscalatesOnce(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t, []HookConfig{
		hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil),
		hookConfig("score", "", nil, nil),
	}, []string{"slice"}, exits(0, nil))
	effects.applyErr = &Refusal{Reason: "contract_conflict", Message: "line 1 registers X for another owner"}

	for round := 1; round <= 3; round++ {
		if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
			t.Fatalf("round %d: finishEffects() error = %v", round, err)
		}
	}
	if got, want := effects.escalations, []string{"landing:batch:apply/batch/0/slice@1 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want one at once", got)
	}
	if got := containers.names(); slices.Contains(got, "hook/batch/1/slice") {
		t.Fatalf("the second hook ran behind a held apply: %q", got)
	}
}

// A configuration change after admission (a reorder of the hooks or the parts, a new image, another score ref or
// escalations owner) changes nothing for a batch that is already admitted: its effects run from the definitions
// frozen in its record, in the frozen order.
func TestAConfigurationChangeAfterAdmissionDoesNotReachTheBatch(t *testing.T) {
	ctx := context.Background()
	refresh := hookConfig("refresh", "", nil, map[string]MountMode{"index": ReadWrite})
	score := hookConfig("score", ApplyScore, []string{"score.json"}, nil)
	runner, effects, containers, batch := landedHooks(t, []HookConfig{refresh, score}, []string{"slice", "all"}, exits(75, nil))
	runner.Scores = &scoreFake{}
	if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
		t.Fatalf("finishEffects() error = %v, want the first hook still to do", err)
	}

	frozen := append([]PostLandEffect(nil), batch.PostLandEffects...)
	changed := runner
	changed.PostLanding = PostLanding{Parts: []string{"all"}, Hooks: []HookConfig{score, refresh}}
	changed.Container = Container{Image: "image@sha256:two", User: "0:0", GitDir: "/other.git", Mounts: declaredMounts()}
	changed.EscalationsOwner = "another-owner"
	containers.runs = nil
	containers.result = func(run ContainerRun) (ContainerResult, error) {
		if strings.HasPrefix(run.Env["LAND_EFFECT_KEY"], "hook/batch/1/") {
			return ContainerResult{Exit: 2}, nil
		}
		return exits(0, nil)(run)
	}

	if err := changed.finishEffects(ctx, &batch); err != nil {
		t.Fatalf("after the configuration change: finishEffects() error = %v", err)
	}
	want := []string{"hook/batch/0/slice", "hook/batch/0/all", "hook/batch/1/slice", "hook/batch/1/all"}
	if got := containers.names(); !slices.Equal(got, want) {
		t.Fatalf("the hooks ran as %q, want the frozen order %q", got, want)
	}
	for _, run := range containers.runs {
		if run.Image != "image@sha256:one" || run.User != "1002:1003" || run.Env["LAND_GIT_DIR"] != "/srv/land/repo.git" {
			t.Fatalf("a run used the new configuration: %+v", run)
		}
	}
	if !slices.Equal(batch.PostLandEffects, frozen) {
		t.Fatal("the batch's frozen effects changed with the configuration")
	}
	for _, escalation := range effects.escalations {
		if !strings.HasSuffix(escalation, " escalations") {
			t.Fatalf("an escalation went to the new owner: %q", effects.escalations)
		}
	}
}

// After a restart, the runner finishes the effects of a landed batch from the ledger's rows: a crash after the
// landing and before a hook ran leaves the hook to the round after it.
func TestARestartFinishesTheEffectsOfALandedBatch(t *testing.T) {
	ctx := context.Background()
	runner, effects, containers, batch := landedHooks(t,
		[]HookConfig{hookConfig("ledger", ApplyContracts, []string{"ops.jsonl"}, nil)}, []string{"slice"}, exits(0, nil))

	resumed, err := runner.resume(ctx, batch)
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if got := containers.names(); !slices.Equal(got, []string{"hook/batch/0/slice"}) {
		t.Fatalf("the restart ran %q", got)
	}
	if len(resumed.PendingEffects) != 0 || !slices.Equal(effects.applied, []string{"apply/batch/0/slice"}) {
		t.Fatalf("after the restart: pending %q, applied %q", resumed.PendingEffects, effects.applied)
	}
}

// An effect whose op this program has no rule for is never dropped silently: it is escalated at once and held for
// an operator, and the batch stays listed behind it.
func TestAnEffectWithAnUnknownOpIsHeldForAnOperator(t *testing.T) {
	ctx := context.Background()
	runner, effects, _, batch := landedHooks(t,
		[]HookConfig{hookConfig("refresh", "", nil, nil)}, []string{"slice"}, exits(0, nil))
	effects.create("kick/batch", "kick", "", "", map[string]any{"commit": "merged"})

	if err := runner.finishEffects(ctx, &batch); !errors.Is(err, ErrEffectsPending) {
		t.Fatalf("finishEffects() error = %v, want the unknown effect pending", err)
	}
	if got, want := effects.escalations, []string{"landing:batch:kick/batch@1 escalations"}; !slices.Equal(got, want) {
		t.Fatalf("escalations = %q, want %q", got, want)
	}
	if !effects.effects["kick/batch"].Held {
		t.Fatal("the unknown effect is not held")
	}
	if !slices.Equal(batch.PendingEffects, []string{"kick/batch"}) {
		t.Fatalf("the batch's pending effects are %q", batch.PendingEffects)
	}
}
