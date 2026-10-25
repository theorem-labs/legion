package landing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The kinds of post-landing effect. Every effect of a landed batch is one of them, and the executor dispatches it
// by its op.
const (
	// OpHook runs one configured hook for one part, in a container on the build VM.
	OpHook = "hook"
	// OpApply applies a hook's output with the program's credentials: the score push or the contract operations.
	OpApply = "apply"
	// OpPublish publishes a hook's defect to the hook's frozen topic.
	OpPublish = "publish"
	// OpLibrarian delivers one near pair that a contracts apply found to the librarian.
	OpLibrarian = "librarian"
	// OpEvent publishes one member's landing event.
	OpEvent = "event"
	// OpWithdraw is the publish path's own effect, which the hook and apply executors skip.
	OpWithdraw = "withdraw"
)

// librarianTimeout is how long one librarian delivery may take.
const librarianTimeout = 120 * time.Second

// maxEffectBackoff caps the wait between two attempts of one effect.
const maxEffectBackoff = 5 * time.Minute

// Effect is one effect of a landed batch as the ledger holds it: its state, whether it is held for an operator, how
// many attempts it has had, and the result that carries an intent's inputs.
type Effect struct {
	Key      string
	Member   string
	Op       string
	Kind     string
	State    string
	Held     bool
	Attempts int64
	Result   json.RawMessage
	At       time.Time
}

// EffectStates of an effect in the ledger.
const (
	EffectIntent = "intent"
	EffectDone   = "done"
)

// OutputFile is one file a hook left in its scratch directory, which the done stores in the ledger.
type OutputFile struct {
	Name  string
	Bytes []byte
}

// FollowOn is an effect that a done creates in the same transaction: a hook's apply or publication.
type FollowOn struct {
	Key   string
	Op    string
	Kind  string
	Input string
}

// EffectLog is the ledger's durable log of a landing's effects. Every write of an executor goes through it, so no
// crash can lose an effect, apply a hook's output twice or lose an escalation.
type EffectLog interface {
	// Effects lists every effect of the batch, by key, with its state, hold and attempt count.
	Effects(ctx context.Context, repo, batch string) ([]Effect, error)
	// Attempt records one failed run of an effect with its result, and, at the threshold or for a deterministic
	// failure, enqueues the escalation in the same transaction. A deterministic failure also holds the effect.
	Attempt(ctx context.Context, repo, batch, key string, result []byte, escalateTo string, escalateAfter int) error
	// Done records an effect's outcome with the outputs it stores, the follow-ons it creates and the escalation its
	// result needs, in one transaction. A repeat with the same result finds it done and succeeds.
	Done(ctx context.Context, repo, batch, key string, result []byte, outputs []OutputFile, then []FollowOn, escalateTo string) error
	// Outputs reads the outputs an effect stored with its done, by name, so an apply never reads a local file.
	Outputs(ctx context.Context, repo, batch, key string) (map[string][]byte, error)
	// ApplyContracts applies a contracts apply's stored operations and records the apply done, in one transaction.
	ApplyContracts(ctx context.Context, repo, batch, key string) error
	// Contracts is the repository's contract list, which the program writes as the contracts hook's input.
	Contracts(ctx context.Context, repo string) ([]byte, error)
}

// ScoreStore is the remote score ref that a score apply reads and pushes, with the program's own credential.
type ScoreStore interface {
	// Latest is the newest score of part on remote's ref, or nil when there is none.
	Latest(ctx context.Context, remote, ref, part string) ([]byte, error)
	// Present reports whether path is already on remote's ref, with the blob it holds there.
	Present(ctx context.Context, remote, ref, path string) (string, error)
	// Add commits body at path on remote's ref as a fast-forward and returns the commit it pushed.
	Add(ctx context.Context, remote, ref, path string, body []byte) (string, error)
}

// DefectPublisher publishes one hook defect to the hook's frozen topic. The message id is the effect's key, so a
// republication after a crash is one a subscriber drops.
type DefectPublisher interface {
	Publish(ctx context.Context, topic, id string, body []byte) error
}

// deterministicFailure is a failure that a rerun would repeat: the ledger holds the effect for an operator and
// escalates it at once.
type deterministicFailure struct {
	Deterministic bool   `json:"deterministic"`
	Reason        string `json:"reason"`
	Detail        string `json:"detail,omitempty"`
}

// finishEffects does every effect of a landed batch that the ledger still lists, in the frozen order: each member
// event, then each hook, and right after a hook its apply, its publication and the librarian deliveries its apply
// created. An effect that is held, or that failed and waits out its backoff, stops the batch's later effects: the
// hook order is part of the contract, and an operator ends the block by fixing the cause or skipping the effect.
// The batch record is never saved again: the ledger's effect rows are the durable state.
func (r Runner) finishEffects(ctx context.Context, batch *Batch) error {
	if len(batch.PendingEffects) == 0 {
		return nil
	}
	if r.Effects == nil {
		return fmt.Errorf("an effect log is required to finish the effects of batch %q", batch.ID)
	}
	stored, err := r.effectsByKey(ctx, *batch)
	if err != nil {
		return err
	}
	commit := batch.PullRequest.MergedCommit
	for _, effect := range batch.PostLandEffects {
		if effect.Op != OpEvent {
			continue
		}
		if err := r.dispatch(ctx, *batch, effect, stored, commit); err != nil {
			return err
		}
	}
	for _, effect := range hookOrder(batch.PostLandEffects) {
		if err := r.dispatch(ctx, *batch, effect, stored, commit); err != nil {
			return err
		}
		stored, err = r.effectsByKey(ctx, *batch)
		if err != nil {
			return err
		}
		for _, follow := range childEffects(stored, effect.Key, batch.ID, effect.Index, effect.Part) {
			if err := r.dispatch(ctx, *batch, follow, stored, commit); err != nil {
				return err
			}
			if follow.Op == OpApply {
				// A contracts apply creates one librarian intent per near pair, which this hook's deliveries
				// run before the next hook.
				if stored, err = r.effectsByKey(ctx, *batch); err != nil {
					return err
				}
				for _, delivery := range childEffects(stored, effect.Key, batch.ID, effect.Index, effect.Part) {
					if delivery.Op != OpLibrarian {
						continue
					}
					if err := r.dispatch(ctx, *batch, delivery, stored, commit); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := r.closeEffects(ctx, batch, commit); err != nil {
		return err
	}
	return nil
}

// effectsByKey reads the batch's effects from the ledger and fills the batch's pending keys from them, so the
// runner's view of what is left is the ledger's.
func (r Runner) effectsByKey(ctx context.Context, batch Batch) (map[string]Effect, error) {
	listed, err := r.Effects.Effects(ctx, batch.Repository, batch.ID)
	if err != nil {
		return nil, fmt.Errorf("read the effects of batch %q: %w", batch.ID, err)
	}
	stored := make(map[string]Effect, len(listed))
	for _, effect := range listed {
		stored[effect.Key] = effect
	}
	return stored, nil
}

// closeEffects leaves the batch with the keys the ledger still lists as pending, removes the landed commit's
// worktree once nothing is left, and reports the effects still to do.
func (r Runner) closeEffects(ctx context.Context, batch *Batch, commit string) error {
	stored, err := r.effectsByKey(ctx, *batch)
	if err != nil {
		return err
	}
	pending := []string{}
	for _, effect := range stored {
		if effect.State == EffectIntent {
			pending = append(pending, effect.Key)
		}
	}
	slices.Sort(pending)
	batch.PendingEffects = pending
	if len(pending) != 0 {
		return fmt.Errorf("%w: %s", ErrEffectsPending, strings.Join(pending, ", "))
	}
	if r.Trees != nil && commit != "" {
		if err := r.Trees.RemoveLandedTree(ctx, commit); err != nil {
			return fmt.Errorf("remove the landed tree of %q: %w", commit, err)
		}
	}
	return nil
}

// hookOrder is the batch's hook effects in the frozen order: by the hook's index, then by the part's position at
// admission, never by sorting keys.
func hookOrder(effects []PostLandEffect) []PostLandEffect {
	hooks := make([]PostLandEffect, 0, len(effects))
	for _, effect := range effects {
		if effect.Op == OpHook {
			hooks = append(hooks, effect)
		}
	}
	slices.SortStableFunc(hooks, func(a, b PostLandEffect) int {
		if a.Index != b.Index {
			return a.Index - b.Index
		}
		return a.partPosition() - b.partPosition()
	})
	return hooks
}

// childEffects are the effects the ledger created for one hook: its apply and publication, then the librarian
// deliveries of its apply, each with the hook's frozen definition.
func childEffects(stored map[string]Effect, hookKey, batch string, index int, part string) []PostLandEffect {
	children := []PostLandEffect{}
	for _, key := range slices.Sorted(effectKeys(stored)) {
		effect := stored[key]
		switch effect.Op {
		case OpApply, OpPublish:
			if key != fmt.Sprintf("%s/%s/%d/%s", effect.Op, batch, index, part) {
				continue
			}
		case OpLibrarian:
			if hookOf(effect) != hookKey {
				continue
			}
		default:
			continue
		}
		children = append(children, PostLandEffect{Key: key, Op: effect.Op, Index: index, Part: part})
	}
	slices.SortStableFunc(children, func(a, b PostLandEffect) int { return opRank(a.Op) - opRank(b.Op) })
	return children
}

func effectKeys(stored map[string]Effect) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range stored {
			if !yield(key) {
				return
			}
		}
	}
}

// opRank orders a hook's children: its apply and its publication, then the deliveries the apply created.
func opRank(op string) int {
	switch op {
	case OpApply:
		return 0
	case OpPublish:
		return 1
	default:
		return 2
	}
}

// hookOf is the hook key that an effect's result names, which a follow-on and a librarian delivery carry.
func hookOf(effect Effect) string {
	var result struct {
		Hook string `json:"hook"`
	}
	_ = json.Unmarshal(effect.Result, &result)
	return result.Hook
}

// dispatch does one effect, unless the ledger has no intent for it, it is already done, it is held for an operator,
// or its backoff has not run out. A held or waiting effect stops the batch's later effects.
func (r Runner) dispatch(ctx context.Context, batch Batch, effect PostLandEffect, stored map[string]Effect, commit string) error {
	current, listed := stored[effect.Key]
	if !listed || current.State == EffectDone {
		return nil
	}
	if current.Held {
		return fmt.Errorf("%w: effect %s of batch %s is held for an operator", ErrEffectsPending, effect.Key, batch.ID)
	}
	if wait := r.effectWait(current); wait > 0 {
		return fmt.Errorf("%w: effect %s of batch %s waits %s after %d attempts",
			ErrEffectsPending, effect.Key, batch.ID, wait.Round(time.Second), current.Attempts)
	}
	definition := r.definitionOf(batch, effect, current)
	var err error
	switch effect.Op {
	case OpEvent:
		err = r.runEvent(ctx, batch, effect, commit)
	case OpHook:
		err = r.runHook(ctx, batch, effect, current, commit)
	case OpApply:
		err = r.runApply(ctx, batch, effect, current, definition, commit)
	case OpPublish:
		err = r.runPublish(ctx, batch, effect, current, definition, commit)
	case OpLibrarian:
		err = r.runLibrarian(ctx, batch, effect, current, definition, commit)
	case OpWithdraw:
		// The publish path alone runs the withdraw, and the runner never lands a batch while one is pending.
		return nil
	default:
		err = r.deterministic(ctx, batch, effect.Key, definition, "unknown_op", effect.Op)
		if err == nil {
			err = fmt.Errorf("%w: effect %s of batch %s has the unknown op %q", ErrEffectsPending, effect.Key, batch.ID, effect.Op)
		}
	}
	if err != nil {
		return err
	}
	return nil
}

// definitionOf is the frozen definition an effect runs under: a hook's own, and for an apply, a publication or a
// delivery the definition of the hook it follows.
func (r Runner) definitionOf(batch Batch, effect PostLandEffect, current Effect) HookDefinition {
	if effect.Definition != nil {
		return *effect.Definition
	}
	hook := hookOf(current)
	for _, frozen := range batch.PostLandEffects {
		if frozen.Definition == nil {
			continue
		}
		if frozen.Key == hook || (hook == "" && frozen.Index == effect.Index && frozen.Part == effect.Part) {
			return *frozen.Definition
		}
	}
	return HookDefinition{EscalateTo: r.EscalationsOwner, EscalateAfter: r.escalateAfter()}
}

// effectWait is how long an effect that failed before still waits, doubling from the configured backoff and capped,
// so a retry does not run on every tick.
func (r Runner) effectWait(effect Effect) time.Duration {
	if effect.Attempts <= 0 || effect.At.IsZero() {
		return 0
	}
	backoff := r.retryBackoff()
	for range effect.Attempts - 1 {
		if backoff >= maxEffectBackoff {
			break
		}
		backoff *= 2
	}
	if backoff > maxEffectBackoff {
		backoff = maxEffectBackoff
	}
	if wait := backoff - time.Since(effect.At); wait > 0 {
		return wait
	}
	return 0
}

// runEvent publishes one member's landing event and records the effect done.
func (r Runner) runEvent(ctx context.Context, batch Batch, effect PostLandEffect, commit string) error {
	if r.Events != nil {
		if err := r.Events.Publish(ctx, LandingEvent{Batch: batch.ID, Member: effect.Member, Commit: commit}); err != nil {
			return r.attempt(ctx, batch, effect.Key, HookDefinition{EscalateTo: r.EscalationsOwner, EscalateAfter: r.escalateAfter()},
				map[string]any{"outcome": "failed", "reason": err.Error()})
		}
	}
	return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "published"}, nil, nil, "")
}

// runHook runs one hook of one part in its container and records what it did. Exit 0 is recorded with its outputs
// and its apply; exit 1 is a defect, which is recorded, published and escalated, and whose outputs are still
// applied; exit 2 and 3 are unable to check, recorded and escalated with nothing applied; every other exit (75
// included) and a timeout is an attempt, retried with backoff and escalated at the frozen threshold.
func (r Runner) runHook(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, commit string) error {
	if r.Containers == nil {
		return fmt.Errorf("a container runner is required to run the hooks of batch %q", batch.ID)
	}
	definition := *effect.Definition
	inputs, failed, err := r.hookInputs(ctx, batch, definition, commit, effect.Part)
	if err != nil {
		return fmt.Errorf("write the inputs of %s: %w", effect.Key, err)
	}
	if failed != nil {
		return r.done(ctx, batch, effect.Key, failed, nil, nil, definition.EscalateTo)
	}
	tree := ""
	if r.Trees != nil {
		tree, err = r.Trees.LandedTree(ctx, commit)
		if err != nil {
			return r.attempt(ctx, batch, effect.Key, definition,
				map[string]any{"outcome": "tree_unavailable", "reason": err.Error()})
		}
	}
	run := ContainerRun{
		Name:  runName(effect.Key, current.Attempts),
		Image: definition.Image,
		User:  definition.User,
		Argv:  definition.Command,
		Env: map[string]string{
			"LAND_COMMIT":     commit,
			"LAND_PART":       effect.Part,
			"LAND_TREE":       tree,
			"LAND_GIT_DIR":    definition.GitDir,
			"LAND_EFFECT_KEY": effect.Key,
		},
		SafeDirectory:  tree,
		Mounts:         definition.Mounts,
		Network:        definition.Network,
		Inputs:         inputs,
		Outputs:        definition.Outputs,
		MaxOutputBytes: definition.outputCap(),
		Timeout:        definition.timeout(),
	}
	result, err := r.Containers.Run(ctx, run)
	if err != nil {
		return r.attempt(ctx, batch, effect.Key, definition,
			map[string]any{"outcome": "transport", "reason": err.Error()})
	}
	switch {
	case result.TimedOut:
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "timeout"})
	case result.Exit == 0 || result.Exit == 1:
		return r.recordHook(ctx, batch, effect, definition, result)
	case result.Exit == 2 || result.Exit == 3:
		return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "exit", "exit": result.Exit}, nil, nil, definition.EscalateTo)
	default:
		// Exit 75 included: the hook could not reach a resource it needs, so it is retried later and nothing it
		// wrote is applied.
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "exit", "exit": result.Exit})
	}
}

// recordHook records a hook that decided (exit 0 or 1) with the outputs it declared, the apply its kind asks for
// and, for a defect with a topic, its publication. A missing or oversize output is a deterministic failure: the
// effect is held for an operator and nothing is applied.
func (r Runner) recordHook(ctx context.Context, batch Batch, effect PostLandEffect, definition HookDefinition, result ContainerResult) error {
	if len(result.Missing) != 0 {
		return r.deterministic(ctx, batch, effect.Key, definition, "output_missing", strings.Join(result.Missing, ", "))
	}
	if len(result.TooLarge) != 0 {
		return r.deterministic(ctx, batch, effect.Key, definition, "output_too_large", strings.Join(result.TooLarge, ", "))
	}
	outputs := make([]OutputFile, 0, len(definition.Outputs))
	for _, name := range definition.Outputs {
		outputs = append(outputs, OutputFile{Name: name, Bytes: result.Outputs[name]})
	}
	var then []FollowOn
	if result.Exit == 1 && definition.Publish != "" {
		then = append(then, FollowOn{
			Key: fmt.Sprintf("publish/%s/%d/%s", batch.ID, effect.Index, effect.Part), Op: OpPublish,
		})
	}
	if definition.Apply != ApplyNone && len(definition.Outputs) != 0 {
		then = append(then, FollowOn{
			Key:   fmt.Sprintf("apply/%s/%d/%s", batch.ID, effect.Index, effect.Part),
			Op:    OpApply,
			Kind:  definition.Apply,
			Input: definition.Outputs[0],
		})
	}
	escalate := ""
	if result.Exit == 1 {
		escalate = definition.EscalateTo
	}
	return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "exit", "exit": result.Exit}, outputs, then, escalate)
}

// hookInputs are the files the hook's container reads under /land/inputs: the two the program writes itself
// (previous.json for a score hook and contracts.json for a contracts hook), and each configured host-side input.
// A configured input that fails is "unable to check": it is recorded, escalated and not retried, which the second
// return value carries.
func (r Runner) hookInputs(ctx context.Context, batch Batch, definition HookDefinition, commit, part string) (map[string][]byte, map[string]any, error) {
	inputs := map[string][]byte{}
	switch definition.Apply {
	case ApplyScore:
		previous := []byte{}
		if r.Scores != nil && definition.Target != nil {
			latest, err := r.Scores.Latest(ctx, definition.Target.Remote, definition.Target.Ref, part)
			if err != nil {
				return nil, nil, err
			}
			if latest != nil {
				previous = latest
			}
		}
		inputs[PreviousInput] = previous
	case ApplyContracts:
		contracts, err := r.Effects.Contracts(ctx, batch.Repository)
		if err != nil {
			return nil, nil, err
		}
		inputs[ContractsInput] = contracts
	}
	for _, input := range definition.Inputs {
		body, err := runHostInput(ctx, input, commit, part, definition.outputCap())
		if err != nil {
			return nil, map[string]any{"outcome": "input_failed", "input": input.Name, "reason": err.Error()}, nil
		}
		inputs[input.Name] = body
	}
	return inputs, nil, nil
}

// runHostInput runs one configured input command on the program's host with only the variables its env names,
// substituting $LAND_COMMIT and $LAND_PART in its arguments, and returns its stdout up to the limit.
func runHostInput(ctx context.Context, input HookInput, commit, part string, limit int64) ([]byte, error) {
	argv := make([]string, len(input.Command))
	for index, argument := range input.Command {
		switch argument {
		case "$LAND_COMMIT":
			argument = commit
		case "$LAND_PART":
			argument = part
		}
		argv[index] = argument
	}
	process := exec.CommandContext(ctx, argv[0], argv[1:]...)
	process.Env = make([]string, 0, len(input.Env))
	for _, name := range input.Env {
		if value, set := os.LookupEnv(name); set {
			process.Env = append(process.Env, name+"="+value)
		}
	}
	output, err := process.Output()
	if err != nil {
		return nil, fmt.Errorf("run input %s: %w", input.Name, err)
	}
	if int64(len(output)) > limit {
		return nil, fmt.Errorf("input %s is %d bytes, over the limit of %d", input.Name, len(output), limit)
	}
	return output, nil
}

// runApply applies a hook's output with the program's credentials, after the hook: the score push, or the contract
// operations through the ledger.
func (r Runner) runApply(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, definition HookDefinition, commit string) error {
	switch current.Kind {
	case ApplyScore:
		return r.applyScore(ctx, batch, effect, current, definition, commit)
	case ApplyContracts:
		return r.applyContracts(ctx, batch, effect, definition)
	default:
		return r.deterministic(ctx, batch, effect.Key, definition, "unknown_apply", current.Kind)
	}
}

// applyScore puts the hook's score on the target ref, once: a path that is already there was pushed by a run whose
// done a crash lost, so nothing is pushed again.
func (r Runner) applyScore(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, definition HookDefinition, commit string) error {
	if r.Scores == nil || definition.Target == nil {
		return r.deterministic(ctx, batch, effect.Key, definition, "no_score_target", "")
	}
	path := scorePath(effect.Part, commit)
	blob, err := r.Scores.Present(ctx, definition.Target.Remote, definition.Target.Ref, path)
	if err != nil {
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "failed", "reason": err.Error()})
	}
	if blob != "" {
		return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "present", "blob": blob}, nil, nil, "")
	}
	body, err := r.applyInput(ctx, batch, current)
	if err != nil {
		return r.deterministic(ctx, batch, effect.Key, definition, "output_missing", err.Error())
	}
	pushed, err := r.Scores.Add(ctx, definition.Target.Remote, definition.Target.Ref, path, body)
	if err != nil {
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "failed", "reason": err.Error()})
	}
	return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "pushed", "commit": pushed}, nil, nil, "")
}

// applyContracts applies the hook's operations and records the apply done in one ledger transaction. A conflict or
// an operation the ledger cannot read would fail the same way on a rerun, so it holds the effect for an operator.
func (r Runner) applyContracts(ctx context.Context, batch Batch, effect PostLandEffect, definition HookDefinition) error {
	err := r.Effects.ApplyContracts(ctx, batch.Repository, batch.ID, effect.Key)
	if err == nil {
		return nil
	}
	var refusal *Refusal
	if errors.As(err, &refusal) {
		if refusal.Reason == "effect_done" {
			return nil
		}
		return r.deterministic(ctx, batch, effect.Key, definition, refusal.Reason, refusal.Message)
	}
	return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "failed", "reason": err.Error()})
}

// applyInput is the hook output that an apply reads, from the ledger and never from a local file, so the loss of
// the program's host loses nothing.
func (r Runner) applyInput(ctx context.Context, batch Batch, current Effect) ([]byte, error) {
	var result struct {
		Hook  string `json:"hook"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(current.Result, &result); err != nil {
		return nil, fmt.Errorf("read the apply's input: %w", err)
	}
	outputs, err := r.Effects.Outputs(ctx, batch.Repository, batch.ID, result.Hook)
	if err != nil {
		return nil, err
	}
	body, stored := outputs[result.Input]
	if !stored {
		return nil, fmt.Errorf("hook %s stored no output %s", result.Hook, result.Input)
	}
	return body, nil
}

// runPublish publishes a hook's defect to the hook's frozen topic, with the effect's key as the message id, so a
// republication after a crash is one a subscriber drops.
func (r Runner) runPublish(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, definition HookDefinition, commit string) error {
	if r.Defects == nil || definition.Publish == "" {
		return r.deterministic(ctx, batch, effect.Key, definition, "no_defect_topic", "")
	}
	body, err := r.defectMessage(ctx, batch, effect, current, definition, commit)
	if err != nil {
		return r.deterministic(ctx, batch, effect.Key, definition, "output_missing", err.Error())
	}
	if err := r.Defects.Publish(ctx, definition.Publish, effect.Key, body); err != nil {
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{"outcome": "failed", "reason": err.Error()})
	}
	return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "published", "message_id": effect.Key}, nil, nil, "")
}

// defectMessage names the landing, the part and the hook's defect output by digest and path.
func (r Runner) defectMessage(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, definition HookDefinition, commit string) ([]byte, error) {
	message := map[string]any{
		"batch": batch.ID, "repository": batch.Repository, "commit": commit, "part": effect.Part,
		"hook": definition.Name, "key": effect.Key,
	}
	if len(definition.Outputs) != 0 {
		outputs, err := r.Effects.Outputs(ctx, batch.Repository, batch.ID, hookOf(current))
		if err != nil {
			return nil, err
		}
		name := definition.Outputs[0]
		body, stored := outputs[name]
		if !stored {
			return nil, fmt.Errorf("the hook stored no output %s", name)
		}
		message["output"] = map[string]string{"name": name, "sha256": digestOf(body)}
	}
	return json.Marshal(message)
}

// runLibrarian delivers one near pair to the librarian: the intent's envelope goes to a fresh 0600 file under a
// private directory, the frozen librarian command reads it, and the file is deleted whatever the command did. A
// repeated delivery after a crash carries the same id, which the librarian treats as a success.
func (r Runner) runLibrarian(ctx context.Context, batch Batch, effect PostLandEffect, current Effect, definition HookDefinition, _ string) error {
	var result struct {
		Line struct {
			ID       string `json:"id"`
			Proposer string `json:"proposer"`
			Envelope string `json:"envelope"`
		} `json:"line"`
	}
	if err := json.Unmarshal(current.Result, &result); err != nil || result.Line.ID == "" {
		return r.deterministic(ctx, batch, effect.Key, definition, "unreadable_librarian_line", string(current.Result))
	}
	if len(definition.Librarian) == 0 {
		return r.deterministic(ctx, batch, effect.Key, definition, "no_librarian_command", "")
	}
	directory, err := os.MkdirTemp("", "land-librarian")
	if err != nil {
		return fmt.Errorf("make the librarian's directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	proposal := filepath.Join(directory, "proposal.json")
	if err := os.WriteFile(proposal, []byte(result.Line.Envelope), 0o600); err != nil {
		return fmt.Errorf("write the librarian's proposal: %w", err)
	}
	run, cancel := context.WithTimeout(ctx, librarianTimeout)
	defer cancel()
	argv := append(append([]string(nil), definition.Librarian...),
		"--id", result.Line.ID, "--proposer", result.Line.Proposer, "--proposal", proposal)
	process := exec.CommandContext(run, argv[0], argv[1:]...)
	process.Stdin = strings.NewReader("")
	if output, err := process.CombinedOutput(); err != nil {
		return r.attempt(ctx, batch, effect.Key, definition, map[string]any{
			"outcome": "failed", "reason": err.Error(), "detail": strings.TrimSpace(string(output)),
		})
	}
	return r.done(ctx, batch, effect.Key, map[string]any{"outcome": "delivered", "exit": 0}, nil, nil, "")
}

// done records an effect's outcome with its outputs, follow-ons and escalation.
func (r Runner) done(ctx context.Context, batch Batch, key string, result map[string]any, outputs []OutputFile, then []FollowOn, escalateTo string) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := r.Effects.Done(ctx, batch.Repository, batch.ID, key, body, outputs, then, escalateTo); err != nil {
		return fmt.Errorf("record %s done: %w", key, err)
	}
	return nil
}

// attempt records one failed run, which the ledger counts and escalates at the frozen threshold, and stops the
// batch's later effects until the next round.
func (r Runner) attempt(ctx context.Context, batch Batch, key string, definition HookDefinition, result map[string]any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := r.Effects.Attempt(ctx, batch.Repository, batch.ID, key, body, definition.EscalateTo, definition.EscalateAfter); err != nil {
		return fmt.Errorf("record an attempt of %s: %w", key, err)
	}
	return fmt.Errorf("%w: effect %s of batch %s failed", ErrEffectsPending, key, batch.ID)
}

// deterministic records a failure that a rerun would repeat, which the ledger escalates at once and holds until an
// operator releases or skips the effect.
func (r Runner) deterministic(ctx context.Context, batch Batch, key string, definition HookDefinition, reason, detail string) error {
	body, err := json.Marshal(deterministicFailure{Deterministic: true, Reason: reason, Detail: detail})
	if err != nil {
		return err
	}
	if err := r.Effects.Attempt(ctx, batch.Repository, batch.ID, key, body, definition.EscalateTo, definition.EscalateAfter); err != nil {
		return fmt.Errorf("record the deterministic failure of %s: %w", key, err)
	}
	return fmt.Errorf("%w: effect %s of batch %s is held after %s", ErrEffectsPending, key, batch.ID, reason)
}

// runName names one container run: the effect's key with its attempt, with the characters docker refuses replaced.
func runName(key string, attempts int64) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, key)
	return fmt.Sprintf("%s-%d", safe, attempts)
}

// digestOf is the hex SHA-256 of body, which a defect message names its output by.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
