package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type retryCLIMigration struct {
	ID               string     `json:"id"`
	State            string     `json:"state"`
	Generation       int64      `json:"generation"`
	LandingRounds    int        `json:"landing_rounds"`
	EscalatedAt      *time.Time `json:"escalated_at"`
	EscalationReason string     `json:"escalation_reason"`
}

type retryCLIEvent struct {
	Kind    string          `json:"kind"`
	Subject string          `json:"subject"`
	Data    json.RawMessage `json:"data"`
}

func retryEvents(t *testing.T, call ledgerCall, id string) []retryCLIEvent {
	t.Helper()
	var events []retryCLIEvent
	if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "events", "r", "0", id)), &events); err != nil {
		t.Fatal(err)
	}
	return events
}

// retryRefused runs retry on id, which must exit 1 with the refusal reason and append no event.
func retryRefused(t *testing.T, call ledgerCall, id, reason string) {
	t.Helper()
	before := retryEvents(t, call, id)
	code, stdout, stderr := call("--json", "retry", "r", id)
	var refusal struct {
		Reason string `json:"reason"`
	}
	if code != 1 || json.Unmarshal([]byte(stdout), &refusal) != nil || refusal.Reason != reason {
		t.Fatalf("retry of %s: code=%d stdout=%q stderr=%q, want exit 1 with reason %s", id, code, stdout, stderr, reason)
	}
	if after := retryEvents(t, call, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused retry of %s appended events: %+v", id, after[len(before):])
	}
}

// An escalated migration is reopened by retry: it stays open at its generation, its landing-round count starts again
// from zero, its escalation is cleared, and migration.retried records the count it had.
func TestRetryCommandReopensAnEscalatedMigration(t *testing.T) {
	call := ledgerCommand(t)
	mustRun(t, call, "repo", "ensure", "r", "1")
	mustRun(t, call, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	var escalated retryCLIMigration
	if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "round", "r", "M-1", "1", "build failed",
		"--key", "round-1", "--ready-event", fmt.Sprint(cliUnreadyEvent))), &escalated); err != nil {
		t.Fatal(err)
	}
	if escalated.EscalatedAt == nil || escalated.LandingRounds != 1 {
		t.Fatalf("the round at the limit did not escalate: %+v", escalated)
	}

	var retried retryCLIMigration
	if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "retry", "r", "M-1")), &retried); err != nil {
		t.Fatal(err)
	}
	want := retryCLIMigration{ID: "M-1", State: "open", Generation: 1}
	if !reflect.DeepEqual(retried, want) {
		t.Fatalf("retry printed %+v, want %+v", retried, want)
	}
	events := retryEvents(t, call, "M-1")
	last := events[len(events)-1]
	var data map[string]any
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatal(err)
	}
	if last.Kind != "migration.retried" || last.Subject != "M-1" || !reflect.DeepEqual(data, map[string]any{"landing_rounds": 1.0}) {
		t.Fatalf("the last event is %s %s %s, want migration.retried M-1 {\"landing_rounds\":1}", last.Kind, last.Subject, last.Data)
	}
	var status struct {
		Migrations []retryCLIMigration `json:"Migrations"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "status", "r")), &status); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.Migrations, []retryCLIMigration{want}) {
		t.Fatalf("status lists %+v after the retry, want the stored migration %+v", status.Migrations, want)
	}

	// The retried migration is no longer escalated, so a second retry is refused.
	retryRefused(t, call, "M-1", "not_escalated")
}

// A retry of an open migration that never escalated exits 1 with not_escalated, one of a closed migration exits 1
// with not_open, and neither appends an event. An unknown migration exits 3, and a wrong argument count exits 2.
func TestRetryCommandRefusesWhatIsNotEscalated(t *testing.T) {
	call := ledgerCommand(t)
	mustRun(t, call, "repo", "ensure", "r", "1")
	mustRun(t, call, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	retryRefused(t, call, "M-1", "not_escalated")

	mustRun(t, call, "open", "r", "M-2", "bob", "base", "branch", "--write", "b.lean")
	mustRun(t, call, "abandon", "r", "M-2", "1", "superseded")
	retryRefused(t, call, "M-2", "not_open")

	mustExit(t, call, 3, "--json", "retry", "r", "M-missing")
	mustExit(t, call, 2, "retry", "r")
	mustExit(t, call, 2, "retry", "r", "M-1", "extra")
}
