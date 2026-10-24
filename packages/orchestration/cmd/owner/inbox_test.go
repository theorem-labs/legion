package main

import (
	"encoding/json"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestInboxOrdersByRuleAndNamesRecordsAndCommits(t *testing.T) {
	f := newCommandFixture(t)
	f.writeScore(map[string]int{caller: 7, second: 3})
	f.as(second).ok("need", state, f.write("need-second.md", needDraft))
	f.as(caller).ok("need", state, f.write("need-caller.md", needDraft))
	f.enqueue("start:"+state+"@1", state)

	var inbox []struct {
		ID                  string `json:"id"`
		Record              string `json:"record"`
		RecordOwner         string `json:"record_owner"`
		Commit              string `json:"commit"`
		StoppedInstructions int    `json:"stopped_instructions"`
		CallHeight          int    `json:"call_height"`
	}
	if err := json.Unmarshal([]byte(f.as(state).ok("--json", "inbox")), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 3 {
		t.Fatalf("inbox = %+v", inbox)
	}
	wantIDs := []string{"start:" + state + "@1", "need:" + ownerpkg.EdgeID(caller, state) + "@r1", "need:" + ownerpkg.EdgeID(second, state) + "@r1"}
	wantStopped := []int{10, 7, 3}
	for index := range inbox {
		if inbox[index].ID != wantIDs[index] || inbox[index].StoppedInstructions != wantStopped[index] {
			t.Fatalf("inbox[%d] = %+v, want %s with %d", index, inbox[index], wantIDs[index], wantStopped[index])
		}
	}
	callerNeed := ownerpkg.NeedRecord("skeleton", caller, state)
	// callee is outside the fixture's active set, so caller is a leaf of the active call graph.
	if inbox[1].Record != callerNeed || inbox[1].RecordOwner != caller || inbox[1].Commit != f.lastCommit(caller, callerNeed) || inbox[1].CallHeight != 0 {
		t.Fatalf("caller need entry = %+v", inbox[1])
	}
	if inbox[0].Commit != "" {
		t.Fatalf("start entry names commit %q before state wrote a spec", inbox[0].Commit)
	}
}

// The score comes from the score branch of the bare repository, which owner serve's landing step fetches there
// (plan Task 2.2, step 1). Before the first score that branch does not exist: RULE.yaml makes every
// stopped-instruction count 0, so the next keys decide and the inbox still answers.
func TestInboxBeforeTheFirstScoreCountsNoStoppedInstructions(t *testing.T) {
	f := newCommandFixture(t)
	f.as(second).ok("need", state, f.write("need-second.md", needDraft))
	f.as(caller).ok("need", state, f.write("need-caller.md", needDraft))
	f.enqueue("start:"+state+"@1", state)

	var inbox []struct {
		ID                  string `json:"id"`
		StoppedInstructions int    `json:"stopped_instructions"`
		CallHeight          int    `json:"call_height"`
	}
	if err := json.Unmarshal([]byte(f.as(state).ok("--json", "inbox")), &inbox); err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"need:" + ownerpkg.EdgeID(second, state) + "@r1", "need:" + ownerpkg.EdgeID(caller, state) + "@r1", "start:" + state + "@1"}
	if len(inbox) != len(wantIDs) {
		t.Fatalf("inbox = %+v", inbox)
	}
	for index, entry := range inbox {
		if entry.ID != wantIDs[index] || entry.StoppedInstructions != 0 || entry.CallHeight != 0 {
			t.Fatalf("inbox[%d] = %+v, want %s with no stopped instructions, in enqueue order", index, entry, wantIDs[index])
		}
	}
}
