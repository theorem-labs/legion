package ledgerstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// Effects lists every effect of a landed batch, with the state, the hold and the attempt count the ledger holds,
// which is what the executor dispatches from.
func (l *Ledger) Effects(ctx context.Context, repo, batch string) ([]landing.Effect, error) {
	stored, err := l.ledger.BatchEffects(ctx, repo, batch)
	if err != nil {
		return nil, refusal(err)
	}
	effects := make([]landing.Effect, 0, len(stored))
	for _, effect := range stored {
		effects = append(effects, landing.Effect{
			Key:      effect.Key,
			Member:   effect.Member,
			Op:       effect.Op,
			Kind:     effect.Kind,
			State:    effect.State,
			Held:     effect.Held,
			Attempts: effect.Attempts,
			Result:   effect.Result,
			At:       effect.At,
		})
	}
	return effects, nil
}

// Attempt records one failed run of an effect. The ledger counts it and, at the threshold or for a deterministic
// failure, enqueues the escalation in the same transaction; a deterministic failure also holds the effect.
func (l *Ledger) Attempt(ctx context.Context, repo, batch, key string, result []byte, escalateTo string, escalateAfter int) error {
	_, err := l.ledger.BatchEffectAttempt(ctx, repo, batch, key, ledger.AttemptRequest{
		Result:        json.RawMessage(result),
		EscalateTo:    escalateTo,
		EscalateAfter: int64(escalateAfter),
	})
	if done(err) {
		return nil
	}
	return refusal(err)
}

// Done records an effect's outcome with the outputs it stores, the follow-ons it creates and the escalation its
// result needs, in one transaction. A repeat of the same done finds it stored and succeeds.
func (l *Ledger) Done(ctx context.Context, repo, batch, key string, result []byte, outputs []landing.OutputFile,
	then []landing.FollowOn, escalateTo string) error {
	request := ledger.DoneRequest{Result: json.RawMessage(result), EscalateTo: escalateTo}
	for _, output := range outputs {
		request.Outputs = append(request.Outputs, ledger.OutputFile{Name: output.Name, Bytes: output.Bytes})
	}
	for _, follow := range then {
		request.Then = append(request.Then, ledger.FollowOn{Key: follow.Key, Op: follow.Op, Kind: follow.Kind, Input: follow.Input})
	}
	_, err := l.ledger.BatchEffectDone(ctx, repo, batch, key, request)
	if done(err) {
		return nil
	}
	return refusal(err)
}

// Outputs reads the outputs an effect stored with its done, by name, so an apply never reads a local file.
func (l *Ledger) Outputs(ctx context.Context, repo, batch, key string) (map[string][]byte, error) {
	stored, err := l.ledger.BatchEffectOutputs(ctx, repo, batch, key)
	if err != nil {
		return nil, refusal(err)
	}
	outputs := make(map[string][]byte, len(stored))
	for _, output := range stored {
		outputs[output.Name] = output.Bytes
	}
	return outputs, nil
}

// ApplyContracts applies a contracts apply's stored operations and records the apply done, in one ledger
// transaction. A done apply is a success: another executor finished it.
func (l *Ledger) ApplyContracts(ctx context.Context, repo, batch, key string) error {
	_, err := l.ledger.BatchEffectApplyContracts(ctx, repo, batch, key)
	if done(err) {
		return nil
	}
	return refusal(err)
}

// ContractsFile is the repository's contract list as the program writes it for a contracts hook: the JSON array
// that `ledger contract list` prints, and an empty array when the repository has no contract.
func (l *Ledger) ContractsFile(ctx context.Context, repo string) ([]byte, error) {
	contracts, err := l.ledger.Contracts(ctx, repo)
	if err != nil {
		return nil, refusal(err)
	}
	if contracts == nil {
		contracts = []ledger.Contract{}
	}
	return json.Marshal(contracts)
}

// done reports whether the error is the ledger's refusal of a call on an effect another executor already finished,
// which leaves the caller nothing to do.
func done(err error) bool {
	var refused *ledger.Refusal
	return errors.As(err, &refused) && refused.Reason == ledger.ReasonEffectDone
}
