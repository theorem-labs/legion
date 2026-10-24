package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The states of a batch effect. An effect goes from intent to done, never back.
const (
	EffectIntent = "intent"
	EffectDone   = "done"
)

// Effect is the landing program's durable intent for one piece of work after a landing (a hook, an apply, a
// publication, a librarian delivery or the withdraw), keyed within its batch.
type Effect struct {
	Key    string `json:"key"`
	Member string `json:"member"`
	Op     string `json:"op"`
	// Kind is a follow-on's apply kind, and empty for every other effect.
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Held stops the effect until an operator releases or skips it: a deterministic attempt sets it.
	Held     bool  `json:"held"`
	Attempts int64 `json:"attempts"`
	// LastAttempt is the newest failed attempt's result, and null before the first one.
	LastAttempt json.RawMessage `json:"last_attempt"`
	// Result holds the intent's inputs (its landing commit, and for a follow-on the effect and output it follows) until
	// the effect is done, and then the done's result.
	Result json.RawMessage `json:"result"`
	// Outputs are the files the done stored, by name, each with its SHA-256.
	Outputs []OutputDigest `json:"outputs"`
	At      time.Time      `json:"at"`
}

// OutputDigest names one stored output and the SHA-256 of its bytes, in hex.
type OutputDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// EffectOutput is one stored output with its bytes.
type EffectOutput struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  []byte `json:"bytes"`
}

// OutputFile is one output that a done stores.
type OutputFile struct {
	Name  string `json:"name"`
	Bytes []byte `json:"bytes"`
}

// FollowOn is an effect that a done creates: an intent with Op and Kind, whose result names the completed effect's
// landing commit, its key, and Input, the name of the completed effect's output that the follow-on reads (empty when it
// reads none).
type FollowOn struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Kind  string `json:"kind"`
	Input string `json:"input"`
}

// AttemptRequest is one failed run of an effect: its result, and the owner and threshold of its escalation from the
// effect's frozen definition. EscalateAfter 0 means no threshold.
type AttemptRequest struct {
	Result        json.RawMessage `json:"result"`
	EscalateTo    string          `json:"escalate_to"`
	EscalateAfter int64           `json:"escalate_after"`
}

// DoneRequest completes an effect: its result, the follow-ons and outputs that commit with it, and the owner of its
// escalation, when the result needs one.
type DoneRequest struct {
	Result     json.RawMessage `json:"result"`
	Then       []FollowOn      `json:"then"`
	Outputs    []OutputFile    `json:"outputs"`
	EscalateTo string          `json:"escalate_to"`
}

// effectColumns selects an effect from a query over batch_effects e, in the order scanEffect reads it.
const effectColumns = `e.key, e.member, e.op, e.kind, e.state, e.held, e.attempts, coalesce(e.last_attempt, 'null'::jsonb),
	e.result, coalesce((SELECT jsonb_agg(jsonb_build_object('name', o.name, 'sha256', o.sha256) ORDER BY o.name)
		FROM batch_effect_outputs o WHERE o.repo = e.repo AND o.batch = e.batch AND o.key = e.key), '[]'::jsonb),
	e.at`

func scanEffect(row pgx.Row) (Effect, error) {
	var e Effect
	err := row.Scan(&e.Key, &e.Member, &e.Op, &e.Kind, &e.State, &e.Held, &e.Attempts, &e.LastAttempt, &e.Result, &e.Outputs, &e.At)
	return e, err
}

// loadEffect reads effect key of batch; forUpdate takes its row lock. A missing effect is NotFoundError.
func loadEffect(ctx context.Context, q querier, repo, batch, key string, forUpdate bool) (Effect, error) {
	sql := `SELECT ` + effectColumns + ` FROM batch_effects e WHERE e.repo = $1 AND e.batch = $2 AND e.key = $3`
	if forUpdate {
		sql += ` FOR UPDATE OF e`
	}
	effect, err := scanEffect(q.QueryRow(ctx, sql, repo, batch, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return effect, &NotFoundError{What: "effect", ID: batch + " " + key}
	}
	return effect, err
}

// lockEffect takes, in the order every effect call uses, the repository's row lock (shared) and then effect key's row
// lock, and returns the effect.
func lockEffect(ctx context.Context, tx pgx.Tx, repo, batch, key string) (Effect, error) {
	if _, err := lockRepo(ctx, tx, repo, false); err != nil {
		return Effect{}, err
	}
	return loadEffect(ctx, tx, repo, batch, key, true)
}

// refuseDone refuses a call that needs an intent on an effect that another executor has already finished.
func refuseDone(batch, key string) error {
	return refuse(ReasonEffectDone, "effect %s of batch %s is already done", key, batch)
}

// BatchEffects lists the effects of batch, by key.
func (s *Store) BatchEffects(ctx context.Context, repo, batch string) ([]Effect, error) {
	effects := []Effect{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		if _, err := loadBatch(ctx, tx, repo, batch, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+effectColumns+` FROM batch_effects e
			WHERE e.repo = $1 AND e.batch = $2 ORDER BY e.key`, repo, batch)
		if err != nil {
			return err
		}
		effects, err = pgx.AppendRows(effects[:0], rows, func(row pgx.CollectableRow) (Effect, error) { return scanEffect(row) })
		return err
	})
	return effects, err
}

// BatchEffectOutputs returns the outputs that effect key of batch stored with its done, by name.
func (s *Store) BatchEffectOutputs(ctx context.Context, repo, batch, key string) ([]EffectOutput, error) {
	outputs := []EffectOutput{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		if _, err := loadEffect(ctx, tx, repo, batch, key, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT name, sha256, bytes FROM batch_effect_outputs
			WHERE repo = $1 AND batch = $2 AND key = $3 ORDER BY name`, repo, batch, key)
		if err != nil {
			return err
		}
		outputs, err = pgx.AppendRows(outputs[:0], rows, pgx.RowToStructByPos[EffectOutput])
		return err
	})
	return outputs, err
}

// BeginWithdraw records the intent withdraw/<batch> (op withdraw) on batch and appends effect.created. A repeat returns
// the stored effect and changes nothing.
func (s *Store) BeginWithdraw(ctx context.Context, repo, batch string) (Effect, error) {
	key := "withdraw/" + batch
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		if _, err := loadBatch(ctx, tx, repo, batch, false); err != nil {
			return err
		}
		created, err := createEffect(ctx, tx, repo, batch, key, "withdraw", "", `'{}'::jsonb`)
		if err != nil {
			return err
		}
		if created {
			if err := event(ctx, tx, repo, "effect.created", batch, "", map[string]any{"key": key, "op": "withdraw"}); err != nil {
				return err
			}
		}
		effect, err = loadEffect(ctx, tx, repo, batch, key, false)
		return err
	})
	return effect, err
}

// BatchEffectAttempt records a failed run of intent key of batch: it counts the attempt, stores req.Result as the
// last attempt and appends effect.attempted. In the same transaction, when the count reaches req.EscalateAfter (0 is
// no threshold), or when the result's deterministic field is true (a failure that a rerun repeats), it enqueues the
// work item landing:<batch>:<key>@<count> for req.EscalateTo with Enqueue's idempotent rule, so no crash separates the
// threshold from its escalation. A deterministic attempt also holds the effect until an operator releases or skips
// it. The count in the item's id gives a later escalation of the effect, after a release, an item of its own. On a done
// effect the call is refused with effect_done and changes nothing.
func (s *Store) BatchEffectAttempt(ctx context.Context, repo, batch, key string, req AttemptRequest) (Effect, error) {
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		stored, err := lockEffect(ctx, tx, repo, batch, key)
		if err != nil {
			return err
		}
		if stored.State == EffectDone {
			return refuseDone(batch, key)
		}
		var attempts int64
		var deterministic bool
		if err := tx.QueryRow(ctx, `UPDATE batch_effects
			SET attempts = attempts + 1, last_attempt = $4::jsonb, at = clock_timestamp(),
				held = held OR coalesce($4::jsonb -> 'deterministic' = 'true'::jsonb, false)
			WHERE repo = $1 AND batch = $2 AND key = $3
			RETURNING attempts, coalesce($4::jsonb -> 'deterministic' = 'true'::jsonb, false)`,
			repo, batch, key, string(req.Result)).Scan(&attempts, &deterministic); err != nil {
			return err
		}
		escalation := ""
		if deterministic || (req.EscalateAfter > 0 && attempts == req.EscalateAfter) {
			escalation = fmt.Sprintf("landing:%s:%s@%d", batch, key, attempts)
			if err := enqueueItem(ctx, tx, repo, escalation, req.EscalateTo); err != nil {
				return err
			}
		}
		if err := event(ctx, tx, repo, "effect.attempted", batch, "", map[string]any{
			"key": key, "attempts": attempts, "result": req.Result, "escalation": escalation,
		}); err != nil {
			return err
		}
		effect, err = loadEffect(ctx, tx, repo, batch, key, false)
		return err
	})
	return effect, err
}

// completion is what completeEffect did.
type completion struct {
	effect Effect
	// replayed is set when the effect was already done with the same result, and nothing was written.
	replayed   bool
	followOns  []string
	outputs    []OutputDigest
	escalation string
}

// completeEffect marks intent key of batch done with req.Result inside tx, with req's outputs, follow-ons and
// escalation; the caller appends the event. A done effect with an equal result is a retry whose response was lost:
// it returns the stored effect, replayed, and writes nothing. A done effect with another result is refused with
// effect_done. The effect's row is written before its outputs, and its outputs before its follow-ons and escalation,
// all in tx.
func completeEffect(ctx context.Context, tx pgx.Tx, repo, batch, key string, req DoneRequest) (completion, error) {
	stored, err := lockEffect(ctx, tx, repo, batch, key)
	if err != nil {
		return completion{}, err
	}
	if stored.State == EffectDone {
		var same bool
		if err := tx.QueryRow(ctx, `SELECT result = $4::jsonb FROM batch_effects WHERE repo = $1 AND batch = $2 AND key = $3`,
			repo, batch, key, string(req.Result)).Scan(&same); err != nil {
			return completion{}, err
		}
		if !same {
			return completion{}, refuseDone(batch, key)
		}
		return completion{effect: stored, replayed: true}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE batch_effects SET state = 'done', result = $4::jsonb, at = clock_timestamp()
		WHERE repo = $1 AND batch = $2 AND key = $3`, repo, batch, key, string(req.Result)); err != nil {
		return completion{}, err
	}
	done := completion{followOns: []string{}, outputs: []OutputDigest{}}
	for _, output := range req.Outputs {
		sum := sha256.Sum256(output.Bytes)
		digest := OutputDigest{Name: output.Name, SHA256: hex.EncodeToString(sum[:])}
		data := output.Bytes
		if data == nil {
			data = []byte{} // a nil slice would reach Postgres as NULL; an empty output is zero bytes
		}
		if _, err := tx.Exec(ctx, `INSERT INTO batch_effect_outputs (repo, batch, key, name, sha256, bytes)
			VALUES ($1, $2, $3, $4, $5, $6)`, repo, batch, key, digest.Name, digest.SHA256, data); err != nil {
			return completion{}, err
		}
		done.outputs = append(done.outputs, digest)
	}
	slices.SortFunc(done.outputs, func(a, b OutputDigest) int { return strings.Compare(a.Name, b.Name) })
	for _, follow := range req.Then {
		created, err := createEffect(ctx, tx, repo, batch, follow.Key, follow.Op, follow.Kind,
			`jsonb_build_object('commit', $6::jsonb -> 'commit', 'hook', $7::text, 'input', $8::text)`,
			string(stored.Result), key, follow.Input)
		if err != nil {
			return completion{}, err
		}
		if created {
			done.followOns = append(done.followOns, follow.Key)
		}
	}
	if req.EscalateTo != "" {
		done.escalation = "landing:" + batch + ":" + key
		if err := enqueueItem(ctx, tx, repo, done.escalation, req.EscalateTo); err != nil {
			return completion{}, err
		}
	}
	done.effect, err = loadEffect(ctx, tx, repo, batch, key, false)
	return done, err
}

// createEffect inserts the intent key of batch with op and kind, whose result is the SQL expression result over the
// parameters $6 onwards (args), and reports whether it created it. An existing effect with that key is left as it is.
func createEffect(ctx context.Context, tx pgx.Tx, repo, batch, key, op, kind, result string, args ...any) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO batch_effects (repo, batch, key, op, kind, result)
		VALUES ($1, $2, $3, $4, $5, `+result+`) ON CONFLICT DO NOTHING`, append([]any{repo, batch, key, op, kind}, args...)...)
	return tag.RowsAffected() == 1, err
}

// BatchEffectDone marks intent key of batch done with req.Result and appends effect.done. In the same transaction it
// stores req.Outputs, each with the SHA-256 of its bytes; creates each of req.Then as an intent whose result holds the
// completed effect's landing commit, its key and the follow-on's input; and, when req.EscalateTo is set, enqueues the
// work item landing:<batch>:<key> for that owner with Enqueue's idempotent rule. So a follow-on or an escalation
// cannot be lost between the effect's completion and its own start. A done effect with the same result returns the
// stored effect and writes nothing; one with another result is refused with effect_done.
func (s *Store) BatchEffectDone(ctx context.Context, repo, batch, key string, req DoneRequest) (Effect, error) {
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		done, err := completeEffect(ctx, tx, repo, batch, key, req)
		if err != nil {
			return err
		}
		effect = done.effect
		if done.replayed {
			return nil
		}
		if err := event(ctx, tx, repo, "effect.done", batch, "", map[string]any{
			"key": key, "follow_ons": done.followOns, "outputs": done.outputs, "escalation": done.escalation,
		}); err != nil {
			return err
		}
		return holdTransaction(ctx, tx, "inside")
	})
	return effect, err
}

// BatchEffectSkip is an operator's done of intent key of batch, with the result {skipped_by: <login>, reason} and no
// follow-on, output or escalation, and appends effect.skipped, which names the login. A done effect follows the done's
// rule: the same skip returns it, and anything else is refused with effect_done. The ledger checks no role.
func (s *Store) BatchEffectSkip(ctx context.Context, repo, batch, key, reason string) (Effect, error) {
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var login string
		if err := tx.QueryRow(ctx, `SELECT session_user`).Scan(&login); err != nil {
			return err
		}
		result, err := json.Marshal(map[string]string{"skipped_by": login, "reason": reason})
		if err != nil {
			return err
		}
		done, err := completeEffect(ctx, tx, repo, batch, key, DoneRequest{Result: result})
		if err != nil {
			return err
		}
		effect = done.effect
		if done.replayed {
			return nil
		}
		return event(ctx, tx, repo, "effect.skipped", batch, "", map[string]any{"key": key, "reason": reason})
	})
	return effect, err
}

// BatchEffectRelease is an operator's release of held intent key of batch: it clears the hold and appends
// effect.released, which names the login and the reason. An effect that is not held is returned unchanged; a done one
// is refused with effect_done. The ledger checks no role.
func (s *Store) BatchEffectRelease(ctx context.Context, repo, batch, key, reason string) (Effect, error) {
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		stored, err := lockEffect(ctx, tx, repo, batch, key)
		if err != nil {
			return err
		}
		if stored.State == EffectDone {
			return refuseDone(batch, key)
		}
		if !stored.Held {
			effect = stored
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE batch_effects SET held = false, at = clock_timestamp()
			WHERE repo = $1 AND batch = $2 AND key = $3`, repo, batch, key); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "effect.released", batch, "", map[string]any{"key": key, "reason": reason}); err != nil {
			return err
		}
		effect, err = loadEffect(ctx, tx, repo, batch, key, false)
		return err
	})
	return effect, err
}

// registerLine and librarianLine are the two operations of a contracts apply's input, as `declindex ledger --emit`
// writes them, one JSON object per line.
type registerLine struct {
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	StatementHash string `json:"statement_hash"`
}

type librarianLine struct {
	ID string `json:"id"`
}

// applyOperations decodes a contracts apply's input: the registrations and the librarian lines, each with its line
// number, in line order. A line that is not one of the two operations, or a registration that RegisterContract's own
// argument rules refuse, is invalid.
func applyOperations(input []byte) (registers []numbered[registerLine], librarians []numbered[json.RawMessage], err error) {
	lines := bytes.Split(input, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		number := i + 1
		var head struct {
			Op string `json:"op"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, nil, invalid("line %d of the apply's input: %v", number, err)
		}
		switch head.Op {
		case "register":
			var register registerLine
			if err := json.Unmarshal(line, &register); err != nil {
				return nil, nil, invalid("line %d of the apply's input: %v", number, err)
			}
			if err := checkContractArgs(register.Name, register.Owner, register.StatementHash); err != nil {
				return nil, nil, invalid("line %d of the apply's input: %v", number, err)
			}
			registers = append(registers, numbered[registerLine]{number, register})
		case "librarian":
			var librarian librarianLine
			if err := json.Unmarshal(line, &librarian); err != nil {
				return nil, nil, invalid("line %d of the apply's input: %v", number, err)
			}
			librarians = append(librarians, numbered[json.RawMessage]{number, json.RawMessage(line)})
		default:
			return nil, nil, invalid("line %d of the apply's input: op %q is neither register nor librarian", number, head.Op)
		}
	}
	return registers, librarians, nil
}

type numbered[T any] struct {
	line  int
	value T
}

// librarianID is the id of a decoded librarian line.
func librarianID(line json.RawMessage) string {
	var librarian librarianLine
	_ = json.Unmarshal(line, &librarian)
	return librarian.ID
}

// BatchEffectApplyContracts completes contracts apply key of batch. It reads its operations from the output that the
// apply's result.hook and result.input name, which the hook's done stored, and its landing commit from the apply's
// result.commit. In one transaction it applies every line and marks the apply done with {applied, unchanged}:
//   - a register line registers an absent contract, as RegisterContract does, with an event that also names the batch,
//     the apply's key and the landing commit; leaves a present one with the same owner and hash unchanged; and refuses
//     anything else with contract_conflict, which names the line and applies nothing;
//   - a librarian line creates the intent librarian/<batch>/<id> (op librarian), whose result holds the line, the
//     landing commit and the apply's hook.
//
// Registrations run in name order, which every apply shares, and every row write comes before the first event: an
// event holds the repository's event lock until its transaction commits, so a call that waits on a row never holds it.
// So no two applies, and no apply and RegisterContract, deadlock. A line that is neither operation, or a registration
// that RegisterContract's argument rules refuse, is invalid; an apply whose named output does not exist is not found;
// a done apply is refused with effect_done. None of these changes anything.
func (s *Store) BatchEffectApplyContracts(ctx context.Context, repo, batch, key string) (Effect, error) {
	var effect Effect
	err := s.tx(ctx, func(tx pgx.Tx) error {
		apply, err := lockEffect(ctx, tx, repo, batch, key)
		if err != nil {
			return err
		}
		if apply.State == EffectDone {
			return refuseDone(batch, key)
		}
		var input []byte
		var commit, source string
		err = tx.QueryRow(ctx, `SELECT o.bytes, coalesce($3::jsonb ->> 'commit', ''), o.key || ' ' || o.name
			FROM batch_effect_outputs o
			WHERE o.repo = $1 AND o.batch = $2 AND o.key = $3::jsonb ->> 'hook' AND o.name = $3::jsonb ->> 'input'`,
			repo, batch, string(apply.Result)).Scan(&input, &commit, &source)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "output", ID: batch + " " + string(apply.Result)}
		}
		if err != nil {
			return err
		}
		registers, librarians, err := applyOperations(input)
		if err != nil {
			return err
		}
		slices.SortStableFunc(registers, func(a, b numbered[registerLine]) int { return strings.Compare(a.value.Name, b.value.Name) })
		applied, unchanged, registered := []string{}, []string{}, []Contract{}
		for _, register := range registers {
			want := register.value
			contract, inserted, err := insertContract(ctx, tx, repo, want.Name, want.Owner, want.StatementHash, false)
			if err != nil {
				return err
			}
			if inserted {
				applied, registered = append(applied, want.Name), append(registered, contract)
				continue
			}
			present, err := loadContract(ctx, tx, repo, want.Name, false)
			if err != nil {
				return err
			}
			if present.Owner != want.Owner || present.StatementHash != want.StatementHash {
				return refuse(ReasonContractConflict, "line %d of %s registers contract %s for %s with hash %s, but it is registered for %s with hash %s",
					register.line, source, want.Name, want.Owner, want.StatementHash, present.Owner, present.StatementHash)
			}
			unchanged = append(unchanged, want.Name)
		}
		followOns := []string{}
		for _, librarian := range librarians {
			intent := "librarian/" + batch + "/" + librarianID(librarian.value)
			created, err := createEffect(ctx, tx, repo, batch, intent, "librarian", "",
				`jsonb_build_object('line', $6::jsonb, 'commit', $7::jsonb -> 'commit', 'hook', $7::jsonb -> 'hook')`,
				string(librarian.value), string(apply.Result))
			if err != nil {
				return err
			}
			if created {
				followOns = append(followOns, intent)
			}
		}
		result, err := json.Marshal(map[string][]string{"applied": applied, "unchanged": unchanged})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE batch_effects SET state = 'done', result = $4::jsonb, at = clock_timestamp()
			WHERE repo = $1 AND batch = $2 AND key = $3`, repo, batch, key, string(result)); err != nil {
			return err
		}
		provenance := map[string]any{"batch": batch, "effect": key, "commit": commit}
		for _, contract := range registered {
			if err := contractRegistered(ctx, tx, repo, contract, provenance); err != nil {
				return err
			}
		}
		if err := event(ctx, tx, repo, "effect.applied", batch, "", map[string]any{
			"key": key, "commit": commit, "applied": applied, "unchanged": unchanged, "follow_ons": followOns,
		}); err != nil {
			return err
		}
		effect, err = loadEffect(ctx, tx, repo, batch, key, false)
		return err
	})
	return effect, err
}
