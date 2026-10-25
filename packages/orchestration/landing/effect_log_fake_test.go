package landing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// effectLogFake keeps the ledger's rules for a landing's effects in memory: an intent per entry of the landed
// record, one attempt count with its escalation threshold, a hold after a deterministic failure, the outputs a done
// stores and the follow-ons it creates.
type effectLogFake struct {
	effects map[string]*Effect
	// outputs are the stored outputs of each effect, by effect key and output name.
	outputs map[string]map[string][]byte
	// escalations are the work items the ledger enqueued, in order.
	escalations []string
	// applied are the contracts applies that ran, in order, and applyErr is what the next one is refused with.
	applied   []string
	applyErr  error
	contracts []byte
	// failNextDone makes the next Done fail, as a crash between a run and its record does.
	failNextDone error
	// onApply runs after a contracts apply, as the ledger's own transaction creates the librarian intents of the
	// hook's emit file.
	onApply func()
	// doneOrder are the keys that were finished, in the order they were.
	doneOrder []string
}

func newEffectLog() *effectLogFake {
	return &effectLogFake{effects: map[string]*Effect{}, outputs: map[string]map[string][]byte{}, contracts: []byte("[]")}
}

// create records one intent, as a landing does in its own transaction.
func (f *effectLogFake) create(key, op, kind, member string, result map[string]any) {
	if _, found := f.effects[key]; found {
		return
	}
	body, _ := json.Marshal(result)
	f.effects[key] = &Effect{Key: key, Op: op, Kind: kind, Member: member, State: EffectIntent, Result: body}
}

func (f *effectLogFake) Effects(_ context.Context, _, _ string) ([]Effect, error) {
	listed := make([]Effect, 0, len(f.effects))
	for _, effect := range f.effects {
		listed = append(listed, *effect)
	}
	slices.SortFunc(listed, func(a, b Effect) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	return listed, nil
}

func (f *effectLogFake) Attempt(_ context.Context, _, batch, key string, result []byte, escalateTo string, escalateAfter int) error {
	effect, found := f.effects[key]
	if !found {
		return fmt.Errorf("no effect %s", key)
	}
	if effect.State == EffectDone {
		return &LedgerRefusal{Reason: "effect_done", Message: "the effect is done"}
	}
	effect.Attempts++
	effect.At = time.Now()
	var failure deterministicFailure
	_ = json.Unmarshal(result, &failure)
	if failure.Deterministic {
		effect.Held = true
	}
	if failure.Deterministic || (escalateAfter > 0 && int(effect.Attempts) == escalateAfter) {
		f.escalations = append(f.escalations,
			fmt.Sprintf("landing:%s:%s@%d %s", batch, key, effect.Attempts, escalateTo))
	}
	return nil
}

func (f *effectLogFake) Done(_ context.Context, _, batch, key string, result []byte, outputs []OutputFile,
	then []FollowOn, escalateTo string) error {
	if f.failNextDone != nil {
		err := f.failNextDone
		f.failNextDone = nil
		return err
	}
	effect, found := f.effects[key]
	if !found {
		return fmt.Errorf("no effect %s", key)
	}
	if effect.State == EffectDone {
		return nil
	}
	effect.State, effect.Result, effect.At = EffectDone, result, time.Now()
	f.doneOrder = append(f.doneOrder, key)
	for _, output := range outputs {
		if f.outputs[key] == nil {
			f.outputs[key] = map[string][]byte{}
		}
		f.outputs[key][output.Name] = output.Bytes
	}
	var commit struct {
		Commit string `json:"commit"`
	}
	_ = json.Unmarshal(effect.Result, &commit)
	for _, follow := range then {
		f.create(follow.Key, follow.Op, follow.Kind, "",
			map[string]any{"hook": key, "input": follow.Input, "commit": commit.Commit})
	}
	if escalateTo != "" {
		f.escalations = append(f.escalations, fmt.Sprintf("landing:%s:%s %s", batch, key, escalateTo))
	}
	return nil
}

func (f *effectLogFake) Outputs(_ context.Context, _, _, key string) (map[string][]byte, error) {
	return f.outputs[key], nil
}

func (f *effectLogFake) ApplyContracts(_ context.Context, _, _, key string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, key)
	effect, found := f.effects[key]
	if !found {
		return fmt.Errorf("no effect %s", key)
	}
	if effect.State == EffectDone {
		return &LedgerRefusal{Reason: "effect_done", Message: "the apply is done"}
	}
	effect.State, effect.At = EffectDone, time.Now()
	effect.Result = []byte(`{"applied":[]}`)
	f.doneOrder = append(f.doneOrder, key)
	if f.onApply != nil {
		f.onApply()
	}
	return nil
}

func (f *effectLogFake) ContractsFile(context.Context, string) ([]byte, error) {
	return f.contracts, nil
}

// pending are the keys the ledger still lists as intents, sorted.
func (f *effectLogFake) pending() []string {
	pending := []string{}
	for key, effect := range f.effects {
		if effect.State == EffectIntent {
			pending = append(pending, key)
		}
	}
	slices.Sort(pending)
	return pending
}

// done are the keys the ledger holds as done, sorted.
func (f *effectLogFake) doneKeys() []string {
	done := []string{}
	for key, effect := range f.effects {
		if effect.State == EffectDone {
			done = append(done, key)
		}
	}
	slices.Sort(done)
	return done
}
