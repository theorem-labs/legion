package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestReturnedPrintsEveryC13Field(t *testing.T) {
	f := newCommandFixture(t)
	migration := "m:" + ownerpkg.Key(caller) + ":1"
	eventID := f.insertEvent(migration, `{"generation":1,"landing_rounds":1,"reason":"check_refused:skeleton","detail":"edge proof uses sorry","log":"logs/b-7/check.log","batch":"b-7","key":"b-7/m-2/1","ready_cleared":true}`)
	out := f.as(caller).ok("--json", "returned", fmt.Sprintf("returned:%s@%d", migration, eventID))
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("returned output %q: %v", out, err)
	}
	want := map[string]string{"reason": "check_refused:skeleton", "detail": "edge proof uses sorry", "log": "logs/b-7/check.log", "batch": "b-7", "key": "b-7/m-2/1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("returned = %v, want %v", got, want)
	}
}

// The C13 producer requires a round's reason and key, and writes each of batch, detail and log that it was not given
// as an empty string: owner returned prints those as absent, in JSON by leaving them out.
func TestReturnedPrintsAReturnWithoutBatchDetailOrLog(t *testing.T) {
	f := newCommandFixture(t)
	migration := "m:" + ownerpkg.Key(caller) + ":1"
	eventID := f.insertEvent(migration, `{"generation":1,"landing_rounds":1,"reason":"head_unavailable","batch":"","detail":"","log":"","key":"b-7/m-2/1","ready_cleared":true}`)
	item := fmt.Sprintf("returned:%s@%d", migration, eventID)
	out := f.as(caller).ok("--json", "returned", item)
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("returned output %q: %v", out, err)
	}
	if want := map[string]string{"reason": "head_unavailable", "key": "b-7/m-2/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("returned = %v, want %v", got, want)
	}
	if out := strings.TrimSpace(f.as(caller).ok("returned", item)); out != "reason head_unavailable\ndetail (absent)\nlog (absent)\nbatch (absent)\nkey b-7/m-2/1" {
		t.Fatalf("returned printed %q", out)
	}
}

// A return with no reason or no key is not one the producer writes: the owner could neither act on it nor name the
// round.
func TestReturnedRefusesAnEventWithoutAReasonOrKey(t *testing.T) {
	f := newCommandFixture(t)
	migration := "m:" + ownerpkg.Key(caller) + ":1"
	for field, data := range map[string]string{
		"key":    `{"generation":1,"landing_rounds":1,"reason":"build_red"}`,
		"reason": `{"generation":1,"landing_rounds":1,"reason":"","key":"b-7/m-2/1"}`,
	} {
		eventID := f.insertEvent(migration, data)
		code, stderr := f.as(caller).fails("returned", fmt.Sprintf("returned:%s@%d", migration, eventID))
		if code != 4 || !strings.Contains(stderr, field) {
			t.Fatalf("returned on an event without %s exited %d: %s", field, code, stderr)
		}
	}
}

// needDraftR2 adds a claim under the first, so a second need is a new revision.
const needDraftR2 = "---\nauthor_model: openai-codex/gpt-5.6-terra\ncites: []\n---\n1. [C1] caller needs the state value to stay stable while it reads it. by: code kernel/caller.c:2\n    1. [C2] caller reads state.value once. by: code kernel/caller.c:2\n"

func TestClaimAndDoneMarkWorkForAPersonOrASupersededVersion(t *testing.T) {
	f := newCommandFixture(t)
	start := "start:" + state + "@1"
	f.enqueue(start, state)
	f.as(state).ok("claim", start)
	if item := f.item(start); item.Holder != state || item.State != "open" {
		t.Fatalf("claimed item = %+v", item)
	}
	if code, _ := f.as(state).fails("claim", "start:function:nobody@1"); code != 1 {
		t.Fatalf("claim of a missing item exited %d, want 1", code)
	}
	// Only a need or an offer has revisions that a newer one supersedes.
	if code, stderr := f.as(state).fails("done", start, "--superseded", "r2"); code != 2 || !strings.Contains(stderr, "need") {
		t.Fatalf("done of a start item exited %d: %s", code, stderr)
	}

	edge := ownerpkg.EdgeID(caller, state)
	need := "need:" + edge + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	f.as(caller).ok("need", state, f.write("need-2.md", needDraftR2))
	for version, want := range map[string]int{"r1": 1, "r3": 1, "r02": 2, "2": 2} {
		if code, stderr := f.as(state).fails("done", need, "--superseded", version); code != want {
			t.Errorf("done %s --superseded %s exited %d, want %d: %s", need, version, code, want, stderr)
		}
	}
	if item := f.item(need); item.State != "open" {
		t.Fatalf("%s after refused supersessions = %+v", need, item)
	}
	f.as(state).ok("done", need, "--superseded", "r2")
	f.as(state).ok("done", need, "--superseded", "r2")
	if item := f.item(need); item.State != "done" || item.Result != "superseded:r2" {
		t.Fatalf("superseded item = %+v", item)
	}
	if code, _ := f.as(caller).fails("done", "need:"+edge+"@r2", "--superseded", "r3"); code != 1 {
		t.Fatalf("done of another owner's item exited %d, want 1", code)
	}

	// An offer names the target's spec revision: only a spec revision that exists supersedes it.
	f.as(state).ok("offer", f.write("offer.md", offerDraft), "--answers", "need:"+edge+"@r2")
	offer := "offer:" + edge + "@r1"
	if code, stderr := f.as(caller).fails("done", offer, "--superseded", "r2"); code != 1 || !strings.Contains(stderr, "r1") {
		t.Fatalf("done of an offer superseded by a missing spec revision exited %d: %s", code, stderr)
	}
}

// done claims or adopts a lease by the step runner's rule: a live lease of the same owner is its own earlier
// process, and a lease of the same owner that has ended is claimed anew.
func TestDoneClaimsAgainOverItsOwnEndedLease(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need := "need:" + edge + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	f.as(caller).ok("need", state, f.write("need-2.md", needDraftR2))
	if _, err := f.store.Claim(f.ctx, "r", need, state, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	f.as(state).ok("done", need, "--superseded", "r2")
	if item := f.item(need); item.State != "done" || item.Result != "superseded:r2" || item.Generation != 2 {
		t.Fatalf("superseded item = %+v, want done at a new generation", item)
	}
}
