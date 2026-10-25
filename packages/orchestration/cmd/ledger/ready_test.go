package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type readyListRecord struct {
	ID                 string            `json:"id"`
	Owner              string            `json:"owner"`
	Generation         int64             `json:"generation"`
	Head               string            `json:"head"`
	ReadyEvent         int64             `json:"ready_event"`
	ReadyAt            time.Time         `json:"ready_at"`
	ChangedContracts   map[string]string `json:"changed_contracts"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []struct {
		Contract string `json:"contract"`
		Reviewer string `json:"reviewer"`
		Head     string `json:"head"`
	} `json:"reviews"`
	ApprovedPinned []string `json:"approved_pinned"`
	Upstream       string   `json:"upstream"`
}

func TestReadyCommandRoundTripsFlagAndRequestFileForms(t *testing.T) {
	call := ledgerCommand(t)
	invoke := func(args ...string) string {
		t.Helper()
		return mustRun(t, call, args...)
	}

	invoke("repo", "ensure", "r", "0")
	invoke("open", "r", "M-flag", "alice", "base", "branch", "--write", "a.lean")
	invoke("open", "r", "M-file", "bob", "base", "branch", "--write", "b.lean")
	invoke("ready", "r", "M-flag", "1", "--key", "k-flag", "--head", "head-flag", "--changed-contract", "C.flag=hash-flag",
		"--compatibility-proof", "C.compat", "--reviewed-head", "head-flag", "--review", "C.flag=alice@head-flag",
		"--approved-pinned", "C.flag")

	request := `{"repo":"r","id":"M-file","generation":1,"key":"k-file","head":"head@file","changed_contracts":{"C@file":"hash=from-file"},"compatibility_proof":"C@compat","reviewed_head":"head@file","reviews":[{"contract":"C@file","reviewer":"alice=reviewer","head":"head@file"}],"approved_pinned":["C@file"]}`
	file := filepath.Join(t.TempDir(), "ready.json")
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke("ready", "--request-file", file)

	var records []readyListRecord
	if err := json.Unmarshal([]byte(invoke("--json", "ready-list", "r")), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != "M-flag" || records[1].ID != "M-file" || records[0].ReadyEvent >= records[1].ReadyEvent {
		t.Fatalf("ready-list ordering: %+v", records)
	}
	flag := records[0]
	if flag.Owner != "alice" || flag.Generation != 1 || flag.Head != "head-flag" || flag.ReadyEvent == 0 || flag.ReadyAt.IsZero() ||
		!reflect.DeepEqual(flag.ChangedContracts, map[string]string{"C.flag": "hash-flag"}) || flag.CompatibilityProof != "C.compat" ||
		flag.ReviewedHead != "head-flag" || len(flag.Reviews) != 1 || flag.Reviews[0].Contract != "C.flag" ||
		flag.Reviews[0].Reviewer != "alice" || flag.Reviews[0].Head != "head-flag" || !reflect.DeepEqual(flag.ApprovedPinned, []string{"C.flag"}) ||
		flag.Upstream != "" {
		t.Fatalf("flag request did not round-trip: %+v", flag)
	}
	fromFile := records[1]
	if fromFile.Owner != "bob" || fromFile.Generation != 1 || fromFile.Head != "head@file" || fromFile.ReadyEvent == 0 || fromFile.ReadyAt.IsZero() ||
		!reflect.DeepEqual(fromFile.ChangedContracts, map[string]string{"C@file": "hash=from-file"}) || fromFile.CompatibilityProof != "C@compat" ||
		fromFile.ReviewedHead != "head@file" || len(fromFile.Reviews) != 1 || fromFile.Reviews[0].Contract != "C@file" ||
		fromFile.Reviews[0].Reviewer != "alice=reviewer" || fromFile.Reviews[0].Head != "head@file" || !reflect.DeepEqual(fromFile.ApprovedPinned, []string{"C@file"}) ||
		fromFile.Upstream != "" {
		t.Fatalf("request-file request did not round-trip: %+v", fromFile)
	}

	text := invoke("ready-list", "r")
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' })
	if len(lines) != 2 || !strings.Contains(lines[0], "M-flag") || !strings.Contains(lines[0], "ready_event=") ||
		!strings.Contains(lines[1], "M-file") || !strings.Contains(lines[1], "ready_event=") {
		t.Fatalf("ready-list text output is not one readable line per record: %q", text)
	}
}

// TestReadyRecordsPrintEmptyCollectionsInBothForms catches a ready record whose absent lists and maps print as null,
// and a record whose shape depends on the input form. A ready with only a head, through flags or a request file,
// prints {} for changed_contracts and [] for reviews and approved_pinned in ready, ready-list and its migration.ready
// event, and a landing with no review prints [] for reviews in its migration.landed event.
func TestReadyRecordsPrintEmptyCollectionsInBothForms(t *testing.T) {
	call := ledgerCommand(t)
	invoke := func(args ...string) string {
		t.Helper()
		return mustRun(t, call, args...)
	}
	empty := func(what, output string, fields map[string]json.RawMessage) {
		t.Helper()
		for field, want := range map[string]string{"changed_contracts": "{}", "reviews": "[]", "approved_pinned": "[]"} {
			if got := string(fields[field]); got != want {
				t.Fatalf("%s prints %s = %s, want %s: %s", what, field, got, want, output)
			}
		}
	}
	invoke("repo", "ensure", "r", "0")
	invoke("open", "r", "M-flag", "alice", "base", "branch", "--write", "a.lean")
	invoke("open", "r", "M-file", "bob", "base", "branch", "--write", "b.lean")
	file := filepath.Join(t.TempDir(), "ready.json")
	if err := os.WriteFile(file, []byte(`{"repo":"r","id":"M-file","generation":1,"key":"k-file","head":"head-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--json", "ready", "r", "M-flag", "1", "--key", "k-flag", "--head", "head-flag"},
		{"--json", "ready", "--request-file", file},
	} {
		output := invoke(args...)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(output), &fields); err != nil {
			t.Fatal(err)
		}
		empty(strings.Join(args, " "), output, fields)
	}
	output := invoke("--json", "ready-list", "r")
	var listed []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &listed); err != nil || len(listed) != 2 {
		t.Fatalf("ready-list: %v %s", err, output)
	}
	for _, fields := range listed {
		empty("ready-list", output, fields)
	}
	for _, id := range []string{"M-flag", "M-file"} {
		output := invoke("--json", "events", "r", "0", id)
		var events []struct {
			Kind string                     `json:"kind"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &events); err != nil {
			t.Fatal(err)
		}
		ready := 0
		for _, event := range events {
			if event.Kind == "migration.ready" {
				ready++
				empty("migration.ready of "+id, output, event.Data)
			}
		}
		if ready != 1 {
			t.Fatalf("%d migration.ready events for %s: %s", ready, id, output)
		}
	}

	invoke("land", "--repo", "r", "--migration", "M-flag", "--generation", "1", "--commit", "commit-1", "--path", "a.lean")
	output = invoke("--json", "events", "r", "0", "M-flag")
	var events []struct {
		Kind string                     `json:"kind"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &events); err != nil {
		t.Fatal(err)
	}
	landed := events[len(events)-1]
	if landed.Kind != "migration.landed" || string(landed.Data["reviews"]) != "[]" || string(landed.Data["contracts"]) != "[]" {
		t.Fatalf("migration.landed event with no review: %s", output)
	}
}

// A flag value that cannot be split one way is a usage error that points to the request file, as it is for check and
// land: a second = in a changed contract, and an @ in a review's contract name. A name with @ in a changed contract or
// an approved pinned contract splits one way, so ready takes it, as check and land do.
func TestReadyFlagsRejectAmbiguousDelimiters(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	for _, args := range [][]string{
		{"ready", "r", "M-1", "1", "--key", "k1", "--head", "head", "--changed-contract", "C=name=hash"},
		{"ready", "r", "M-1", "1", "--key", "k1", "--head", "head", "--review", "C@name=alice@head"},
	} {
		if code, _, stderr := call(args...); code != 2 || !strings.Contains(stderr, "request-file") {
			t.Fatalf("%v: code=%d stderr=%q; want delimiter usage refusal", args, code, stderr)
		}
	}
	if code, _, stderr := call("ready", "r", "M-1", "1", "--key", "k1", "--head", "head", "--changed-contract", "C@name=hash",
		"--approved-pinned", "C@name"); code != 0 {
		t.Fatalf("names with @: code=%d stderr=%q; want them taken as check and land take them", code, stderr)
	}
}

// ready without --head is a usage error that names the flag, through flags, as before this change.
func TestReadyWithoutHeadNamesTheFlag(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	if code, _, stderr := call("--json", "ready", "r", "M-1", "1", "--key", "k1"); code != 2 || !strings.Contains(stderr, "ready needs --head") {
		t.Fatalf("ready without --head: code=%d stderr=%q", code, stderr)
	}
}

// ready without --key is a usage error that names the flag, and records nothing.
func TestReadyWithoutKeyNamesTheFlag(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	if code, _, stderr := call("--json", "ready", "r", "M-1", "1", "--head", "H1"); code != 2 || !strings.Contains(stderr, "ready needs --key") {
		t.Fatalf("ready without --key: code=%d stderr=%q", code, stderr)
	}
	if listed := mustRun(t, call, "--json", "ready-list", "r"); listed != "[]\n" {
		t.Fatalf("ready without --key was recorded: %s", listed)
	}
}

// The retry of a submission the landing program already returned publishes nothing, through flags and through a
// request file: ready H1 with key k1, a round for its event, ready H1 with k1 again. ready-list stays empty, and only
// one migration.ready event exists. ready H1 with k2 publishes it again (control).
func TestReadyCommandRetryOfAConsumedSubmissionPublishesNothing(t *testing.T) {
	for _, form := range []string{"flags", "request file"} {
		t.Run(form, func(t *testing.T) {
			call := ledgerCommand(t)
			mustExit(t, call, 0, "repo", "ensure", "r", "0")
			mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
			ready := func(key string) []string {
				if form == "flags" {
					return []string{"--json", "ready", "r", "M-1", "1", "--key", key, "--head", "H1"}
				}
				file := filepath.Join(t.TempDir(), key+".json")
				body := `{"repo":"r","id":"M-1","generation":1,"key":"` + key + `","head":"H1"}`
				if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"--json", "ready", "--request-file", file}
			}
			var first readyListRecord
			if err := json.Unmarshal([]byte(mustRun(t, call, ready("k1")...)), &first); err != nil || first.ReadyEvent == 0 {
				t.Fatalf("first ready: %v %+v", err, first)
			}
			mustRun(t, call, "--json", "round", "r", "M-1", "1", "H1 build failed", "--key", "round-1",
				"--ready-event", strconv.FormatInt(first.ReadyEvent, 10))

			var retry readyListRecord
			if err := json.Unmarshal([]byte(mustRun(t, call, ready("k1")...)), &retry); err != nil || !reflect.DeepEqual(retry, first) {
				t.Fatalf("retry of k1: %v %+v, want the record k1 stored %+v", err, retry, first)
			}
			if listed := mustRun(t, call, "--json", "ready-list", "r"); listed != "[]\n" {
				t.Fatalf("the retry published the consumed submission again: %s", listed)
			}
			var events []struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "events", "r", "0", "M-1")), &events); err != nil {
				t.Fatal(err)
			}
			readyEvents := 0
			for _, event := range events {
				if event.Kind == "migration.ready" {
					readyEvents++
				}
			}
			if readyEvents != 1 {
				t.Fatalf("%d migration.ready events, want 1", readyEvents)
			}

			mustRun(t, call, ready("k2")...)
			var listed []readyListRecord
			if err := json.Unmarshal([]byte(mustRun(t, call, "--json", "ready-list", "r")), &listed); err != nil ||
				len(listed) != 1 || listed[0].Head != "H1" || listed[0].ReadyEvent == first.ReadyEvent {
				t.Fatalf("a new key did not publish H1 again: %v %+v", err, listed)
			}
		})
	}
}
