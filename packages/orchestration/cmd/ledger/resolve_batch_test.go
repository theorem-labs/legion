package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The post-merge path, as the landing program and an operator take it: save-batch moves an admitted batch to
// landed_unrecorded with escalate_to and no merged commit, which enqueues the escalation item; the fence still holds
// the members. resolve-batch then lands every member at the operator's merged commit, writes that commit into the
// record's pull_request.merged_commit, releases the fence, and records each post-landing effect's intent with the
// commit. batches --active lists the batch until those effects are done, and a repeat prints the stored batch.
func TestResolveBatchCommandLandsAnUnrecordedBatch(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	unrecorded := unrecordedCLIRecord()
	unrecorded["pull_request"] = map[string]any{"number": 7}
	unrecorded["post_land_effects"] = []map[string]any{{"key": "hook/batch-1/0/report", "op": "hook"}}
	saved := cli.save("batch-1", unrecorded, admitted.RecordVersion)
	var item struct {
		Owner string `json:"owner"`
		State string `json:"state"`
	}
	cli.decode(cli.invoke("--json", "item", "r", "landing:batch-1"), &item)
	if item.Owner != cliEscalationOwner || item.State != "open" {
		t.Fatalf("escalation item = %+v", item)
	}
	if active := cli.active(); len(active) != 0 {
		t.Fatalf("batches --active lists the unrecorded batch: %q", active)
	}
	cli.refused("batch_admitted", "takeover", "r", "M-1", "zoe", "1")

	var resolved cliBatch
	cli.decode(cli.invoke("resolve-batch", "r", "batch-1", "merged-9"), &resolved)
	var record map[string]any
	if err := json.Unmarshal(resolved.Record, &record); err != nil {
		t.Fatal(err)
	}
	pull, _ := record["pull_request"].(map[string]any)
	if resolved.Phase != "landed" || resolved.Admission != "landed" || resolved.MergedCommit != "merged-9" ||
		resolved.RecordVersion != saved.RecordVersion+1 || record["reason"] != "resolved by operator" ||
		pull["merged_commit"] != "merged-9" || pull["number"] != float64(7) {
		t.Fatalf("resolve-batch printed %+v with record %s", resolved, resolved.Record)
	}
	for _, id := range []string{"M-1", "M-2"} {
		if state, commit := cli.migration(id); state != "landed" || commit != "merged-9" {
			t.Fatalf("migration %s after resolve-batch: %s at %q", id, state, commit)
		}
	}
	want := []cliEffectRow{{Key: "hook/batch-1/0/report", Op: "hook", State: "intent", Result: map[string]any{"commit": "merged-9"}}}
	if effects := cli.effectRows("batch-1"); !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects = %+v, want %+v", effects, want)
	}
	cli.invoke("contract", "transfer", "r", "C", "alice", "bob")
	cli.refused("not_open", "takeover", "r", "M-1", "zoe", "1")
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active = %q, want batch-1 while its effect is pending", active)
	}

	events := len(cli.events(""))
	var repeated cliBatch
	cli.decode(cli.invoke("resolve-batch", "r", "batch-1", "merged-9"), &repeated)
	if !reflect.DeepEqual(repeated, resolved) {
		t.Fatalf("the repeat printed %+v, want the stored %+v", repeated, resolved)
	}
	cli.assertUnchanged(resolved, events)

	// BatchEffectDone finishes the effect; the test marks it done directly.
	cli.sql(`UPDATE batch_effects SET state = 'done' WHERE repo = 'r' AND batch = 'batch-1'`)
	if active := cli.active(); len(active) != 0 {
		t.Fatalf("batches --active after the effect = %q", active)
	}
}

// resolve-batch takes exactly a repository, a batch and a merged commit; anything else exits 2 and changes nothing.
func TestResolveBatchCommandRefusesMalformedInput(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	events := len(cli.events(""))
	for _, args := range [][]string{{"resolve-batch", "r", "batch-1"}, {"resolve-batch", "r", "batch-1", "m", "extra"}} {
		if code, stdout, stderr := cli.call(args...); code != 2 || !strings.Contains(stderr, "resolve-batch <repo> <id> <merged-commit>") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 with the usage", args, code, stdout, stderr)
		}
	}
	cli.assertUnchanged(admitted, events)
}
