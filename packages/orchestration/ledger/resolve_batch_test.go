package ledger_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// unrecordedLanding is the landing fixture after GitHub merged batch-1 and the ledger refused its landing: the landing
// program saved the record landed_unrecorded with escalate_to and its post-landing effects, and with pullRequest as the
// record's pull_request (absent when nil).
func unrecordedLanding(t *testing.T, pullRequest map[string]any) landingFixture {
	t.Helper()
	f := admittedLanding(t)
	fields := []any{"reason", "merged, but the ledger refused the landing", "escalate_to", escalationOwner,
		"post_land_effects", landingEffects("batch-1")}
	if pullRequest != nil {
		fields = append(fields, "pull_request", pullRequest)
	}
	f.admitted = must(f.s.SaveBatch(context.Background(), "r", "batch-1", batchRecord("landed_unrecorded", fields...), f.admitted.RecordVersion))
	return f
}

// ResolveBatch is the operator's resolution of landed_unrecorded. It derives the record from the stored one (state
// landed, reason "resolved by operator", pull_request.merged_commit the given commit, every other field kept), and
// does what LandBatch does with it at the stored version: the members land at the commit, the fence ends, each
// post-landing effect gets an intent with the commit, and batch.resolved names the operator's login. This holds for a
// batch escalated with another merged commit, with none, and with no pull request in its record.
func TestResolveBatchLandsAnUnrecordedBatch(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name        string
		pullRequest map[string]any
	}{
		{"with a merged commit", map[string]any{"number": 7, "merged_commit": "merged-0"}},
		{"with no merged commit", map[string]any{"number": 7}},
		{"with no pull request", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := unrecordedLanding(t, test.pullRequest)
			if item := must(f.s.Item(ctx, "r", "landing:batch-1")); item.Owner != escalationOwner || item.State != "open" {
				t.Fatalf("escalation item = %+v", item)
			}
			_, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe")
			wantRefusal(t, err, ledger.ReasonBatchAdmitted)

			operator, operatorDSN := testpg.Login(t, f.dsn, "ledger_operator")
			store := must(ledger.Open(ctx, operatorDSN))
			defer store.Close()
			before := len(must(f.s.Events(ctx, "r", 0, "")))
			resolved := must(store.ResolveBatch(ctx, "r", "batch-1", "merged-9"))

			want := map[string]any{}
			if err := json.Unmarshal(f.admitted.Record, &want); err != nil {
				t.Fatal(err)
			}
			pull := map[string]any{"merged_commit": "merged-9"}
			if test.pullRequest != nil {
				pull["number"] = 7
			}
			want["state"], want["reason"], want["pull_request"] = "landed", "resolved by operator", pull
			if resolved.Phase != ledger.PhaseLanded || resolved.Admission != "landed" || resolved.MergedCommit != "merged-9" ||
				resolved.RecordVersion != f.admitted.RecordVersion+1 || !sameJSON(t, resolved.Record, must(json.Marshal(want))) {
				t.Fatalf("resolved batch = %+v with record %s", resolved, resolved.Record)
			}
			for _, id := range []string{"M-1", "M-2"} {
				if m := must(f.s.Migration(ctx, "r", id)); m.State != ledger.StateLanded || m.LandedCommit != "merged-9" {
					t.Fatalf("member %s after the resolution: %+v", id, m)
				}
			}
			if c := must(f.s.Contract(ctx, "r", "C")); c.StatementHash != "c2" || c.Version != 2 {
				t.Fatalf("contract C after the resolution: %+v", c)
			}
			for _, effect := range effectRows(t, f.dsn, "batch-1") {
				if effect.State != "intent" || !sameJSON(t, json.RawMessage(effect.Result), json.RawMessage(`{"commit":"merged-9"}`)) {
					t.Fatalf("effect after the resolution: %+v", effect)
				}
			}
			if keys := resolved.PendingEffects; !slices.Equal(keys, []string{"hook/batch-1/0/report", "hook/batch-1/1/score"}) {
				t.Fatalf("pending effects = %q", keys)
			}
			described, events := eventsSince(t, f.s, before)
			if last := events[len(events)-1]; described[len(described)-1] != "batch.resolved batch-1" || last.Login != operator {
				t.Fatalf("events of the resolution = %q, last by %q, want batch.resolved by %q", described, last.Login, operator)
			}
			if transferred, err := f.s.TransferContract(ctx, "r", "C", "alice", "zoe"); err != nil || transferred.Owner != "zoe" {
				t.Fatalf("transfer after the resolution: %+v, %v", transferred, err)
			}

			if active := activeIDs(t, f.s); !slices.Equal(active, []string{"batch-1"}) {
				t.Fatalf("active batches = %q, want batch-1 while its effects are pending", active)
			}
			// BatchEffectDone finishes the effects; the test marks them done directly.
			batchSQL(t, f.dsn, `UPDATE batch_effects SET state = 'done' WHERE repo = 'r' AND batch = 'batch-1'`)
			if active := activeIDs(t, f.s); len(active) != 0 {
				t.Fatalf("active batches after the effects = %q", active)
			}
		})
	}
}

// A repeat of a resolution with the same merged commit returns the stored batch and changes nothing.
func TestResolveBatchReplaysTheSameMergedCommit(t *testing.T) {
	ctx := context.Background()
	f := unrecordedLanding(t, map[string]any{"number": 7})
	resolved := must(f.s.ResolveBatch(ctx, "r", "batch-1", "merged-9"))
	before := f.state(t)
	if repeated := must(f.s.ResolveBatch(ctx, "r", "batch-1", "merged-9")); !reflect.DeepEqual(repeated, resolved) {
		t.Fatalf("the repeat returned %+v, want the stored %+v", repeated, resolved)
	}
	if after := f.state(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("the repeat changed the ledger:\n got %+v\nwant %+v", after, before)
	}
}
