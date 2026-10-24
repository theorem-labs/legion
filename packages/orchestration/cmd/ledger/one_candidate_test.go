package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// One candidate gets the same exit code from check and from land, through flags and through one request file, also
// when it is wrong in more than one way. The first candidate has no changed-path list and a head with a leading space.
func TestCheckAndLandGiveACandidateTheSameExitCode(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	for i, candidate := range []struct {
		name  string
		flags []string
		file  string
	}{
		{"no changed paths and a padded head", []string{"--commit", "c", "--head", " head"},
			`{"repo":"r","migration":"M-1","generation":1,"commit":"c","head":" head"}`},
		{"no changed paths", []string{"--commit", "c"},
			`{"repo":"r","migration":"M-1","generation":1,"commit":"c"}`},
		{"a padded path", []string{"--commit", "c", "--path", " a.lean"},
			`{"repo":"r","migration":"M-1","generation":1,"commit":"c","changed_paths":[" a.lean"]}`},
	} {
		file := filepath.Join(t.TempDir(), fmt.Sprintf("candidate-%d.json", i))
		if err := os.WriteFile(file, []byte(candidate.file), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, form := range []struct {
			name        string
			check, land []string
		}{
			{"flags", append([]string{"--json", "check", "r", "M-1", "1"}, candidate.flags...),
				append([]string{"--json", "land", "--repo", "r", "--migration", "M-1", "--generation", "1"}, candidate.flags...)},
			{"request file", []string{"--json", "check", "--request-file", file}, []string{"--json", "land", "--request-file", file}},
		} {
			checkCode, checkOut, checkErr := call(form.check...)
			landCode, landOut, landErr := call(form.land...)
			if checkCode != landCode || checkCode == 0 {
				t.Errorf("%s through %s: check exit %d (%s%s), land exit %d (%s%s); want one refusing exit code",
					candidate.name, form.name, checkCode, checkOut, checkErr, landCode, landOut, landErr)
			}
		}
	}
}

// check takes land's request file unchanged: the same file passes check and then lands.
func TestCheckTakesTheLandRequestFile(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	file := filepath.Join(t.TempDir(), "land.json")
	if err := os.WriteFile(file, []byte(`{"repo":"r","migration":"M-1","generation":1,"commit":"c","changed_paths":["a.lean"],"current":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := call("--json", "check", "--request-file", file); code != 0 || !strings.Contains(stdout, `"outcome":"pass"`) {
		t.Fatalf("check of a land request file: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	mustExit(t, call, 0, "land", "--request-file", file)
}

// ready, check and land read the statement-record flags with one parser and validate the records with one validator,
// so each value below gets one answer from all three: accepted, or refused as invalid input (exit 2).
func TestReadyCheckAndLandAgreeOnRecordFlags(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	for i, probe := range []struct {
		name    string
		flag    []string
		refused bool
	}{
		{"a pinned name with @", []string{"--approved-pinned", "a@b"}, false},
		{"a blank pinned name", []string{"--approved-pinned", ""}, true},
		{"a contract name with @", []string{"--changed-contract", "a@b=h"}, false},
		{"a contract name with spaces", []string{"--changed-contract", " C=h"}, true},
		{"a contract hash with =", []string{"--changed-contract", "C=h=1"}, true},
	} {
		id := fmt.Sprintf("M-%d", i)
		mustExit(t, call, 0, "open", "r", id, "alice", "base", "branch", "--write", id+".lean")
		candidate := append([]string{"--commit", "c", "--path", id + ".lean", "--head", "head"}, probe.flag...)
		for _, args := range [][]string{
			append([]string{"--json", "ready", "r", id, "1", "--key", "k-" + id, "--head", "head"}, probe.flag...),
			append([]string{"--json", "check", "r", id, "1"}, candidate...),
			append([]string{"--json", "land", "--repo", "r", "--migration", id, "--generation", "1"}, candidate...),
		} {
			code, stdout, stderr := call(args...)
			if (code == 2) != probe.refused {
				t.Errorf("%s: %v exited %d (stdout %q, stderr %q); refused as invalid input: want %t",
					probe.name, args[1], code, stdout, stderr, probe.refused)
			}
		}
	}
}

func TestDeferredCheckExitsFiveAndRequestFilesRejectUnknownFields(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean", "--read", "C=hash-1")
	assertIncomplete := func(code int, stdout, stderr string) {
		t.Helper()
		var result struct {
			Outcome  string   `json:"outcome"`
			Pass     bool     `json:"pass"`
			Deferred []string `json:"deferred"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil || code != 5 || result.Outcome != "incomplete" ||
			result.Pass || !slices.Equal(result.Deferred, []string{"C"}) {
			t.Fatalf("deferred check: code=%d result=%+v stdout=%q stderr=%q err=%v", code, result, stdout, stderr, err)
		}
	}
	code, stdout, stderr := call("--json", "check", "r", "M-1", "1", "--commit", "candidate", "--path", "a.lean", "--defer-reads")
	assertIncomplete(code, stdout, stderr)

	request := filepath.Join(t.TempDir(), "check.json")
	if err := os.WriteFile(request, []byte(`{"repo":"r","migration":"M-1","generation":1,"commit":"candidate","changed_paths":["a.lean"],"defer_reads":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = call("--json", "check", "--request-file", request)
	assertIncomplete(code, stdout, stderr)

	unknown := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"repo":"r","migration":"M-1","generation":1,"commit":"candidate","changed_paths":["a.lean"],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := call("check", "--request-file", unknown); code != 2 || !strings.Contains(stderr, "unknown field") {
		t.Fatalf("unknown request field: code=%d stderr=%q", code, stderr)
	}
}

// --defer-reads is check's flag alone: land names it as a flag it does not know, and check's identity flags are
// land's alone, so check names --repo as unknown. Neither treats the flag as one that takes a value.
func TestCandidateFlagsNameTheFlagTheCommandDoesNotTake(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "0")
	mustExit(t, call, 0, "open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	for _, probe := range []struct {
		args []string
		want string
	}{
		{[]string{"land", "--repo", "r", "--migration", "M-1", "--generation", "1", "--commit", "c", "--path", "a.lean", "--defer-reads"},
			`unknown land flag "--defer-reads"`},
		{[]string{"check", "r", "M-1", "1", "--commit", "c", "--path", "a.lean", "--repo", "r"}, `unknown check flag "--repo"`},
	} {
		if code, _, stderr := call(probe.args...); code != 2 || !strings.Contains(stderr, probe.want) {
			t.Errorf("%v: code=%d stderr=%q, want exit 2 with %s", probe.args, code, stderr, probe.want)
		}
	}
}
