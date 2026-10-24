package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// goSyntax matches what fmt prints for a Go value and JSON never contains outside a string: a struct or slice
// element opened by an unquoted word ({item-1 …}), a map (map[…]), a pointer (&{…}) or a nil (<nil>).
var goSyntax = regexp.MustCompile(`\{[^"\s}\]]|map\[|&\{|<nil>`)

// textForm is how a command prints its result without --json.
type textForm int

const (
	indentedJSON textForm = iota // a result with no text form: the --json bytes, indented
	lineText                     // a result with its own text form, such as a lease line
	readJSON                     // a read command: the --json bytes, in both modes
)

// TestTextOutputIsNeverGoSyntax catches a command printing its result with fmt's Go syntax when --json is not given.
// Each command that returns a value runs once without --json. A result with a text form prints that text; a read
// command prints the JSON that --json prints; every other result prints that JSON indented.
func TestTextOutputIsNeverGoSyntax(t *testing.T) {
	call := ledgerCommand(t)
	for _, args := range [][]string{{"repo", "ensure", "r", "0"}, {"enqueue", "r", "item-1", "owner:a"}, {"enqueue", "r", "item-2", "owner:a"}} {
		if code, _, stderr := call(append([]string{"--json"}, args...)...); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr)
		}
	}
	_, out, _ := call("--json", "events", "r", "0")
	var events []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &events); err != nil || len(events) == 0 {
		t.Fatalf("events: %v %q", err, out)
	}
	revision := strconv.FormatInt(events[len(events)-1].ID, 10)

	for _, step := range []struct {
		form textForm
		args []string
	}{
		{indentedJSON, []string{"repo", "ensure", "other", "0"}},
		{indentedJSON, []string{"refuse-owner", "r", "owner:a", revision, "no session template"}},
		{lineText, []string{"claim", "r", "item-1", "holder-1", "0"}},
		{lineText, []string{"claim", "r", "item-2", "holder-1", "1h"}},
		{lineText, []string{"renew", "r", "item-2", "holder-1", "1", "1m"}},
		{indentedJSON, []string{"release-holder", "r", "holder-1", "pane exited"}},
		{indentedJSON, []string{"expire", "r"}},
		{readJSON, []string{"item", "r", "item-1"}},
		{readJSON, []string{"items", "r", "--owner", "owner:a"}},
		{readJSON, []string{"owners", "r"}},
		{indentedJSON, []string{"contract", "register", "r", "C", "alice", "h1", "false"}},
		{indentedJSON, []string{"contract", "transfer", "r", "C", "alice", "bob"}},
		{readJSON, []string{"contract", "show", "r", "C"}},
		{readJSON, []string{"contract", "list", "r"}},
		{indentedJSON, []string{"open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean", "--read", "C=h1"}},
		{indentedJSON, []string{"extend", "r", "M-1", "1", "--write", "b.lean"}},
		{indentedJSON, []string{"ready", "r", "M-1", "1", "--key", "k1", "--head", "h1"}},
		{lineText, []string{"ready-list", "r"}},
		{indentedJSON, []string{"round", "r", "M-1", "1", "build failed", "--key", "k1", "--ready-event", "1099511627776"}},
		{indentedJSON, []string{"takeover", "r", "M-1", "carol", "1"}},
		{readJSON, []string{"check", "r", "M-1", "2", "--commit", "c1", "--path", "a.lean", "--current", "C=h1"}},
		{indentedJSON, []string{"land", "--repo", "r", "--migration", "M-1", "--generation", "2", "--commit", "c1",
			"--path", "a.lean", "--current", "C=h1"}},
		{indentedJSON, []string{"open", "r", "M-2", "dave", "base", "branch", "--write", "z.lean"}},
		{indentedJSON, []string{"abandon", "r", "M-2", "1", "superseded"}},
		{indentedJSON, []string{"repo", "ensure", "escalating", "1"}},
		{indentedJSON, []string{"open", "escalating", "M-3", "erin", "base", "branch", "--write", "e.lean"}},
		{indentedJSON, []string{"round", "escalating", "M-3", "1", "build failed", "--key", "k3", "--ready-event", "1099511627776"}},
		{indentedJSON, []string{"retry", "escalating", "M-3"}},
		{indentedJSON, []string{"agree", "r", "A-1", "--party", "alice", "--read", "C=1"}},
		{readJSON, []string{"stale", "r"}},
		{readJSON, []string{"status", "r"}},
		{readJSON, []string{"events", "r", "0"}},
	} {
		code, stdout, stderr := call(step.args...)
		if code != 0 {
			t.Fatalf("%v exited %d: stdout=%q stderr=%q", step.args, code, stdout, stderr)
		}
		if goSyntax.MatchString(stdout) {
			t.Fatalf("%v printed Go syntax without --json: %q", step.args, stdout)
		}
		switch step.form {
		case lineText:
			if json.Valid([]byte(stdout)) || strings.TrimSpace(stdout) == "" {
				t.Fatalf("%v printed %q, want its text form", step.args, stdout)
			}
		case readJSON:
			if !json.Valid([]byte(stdout)) || strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
				t.Fatalf("%v printed %q, want one line of JSON", step.args, stdout)
			}
		case indentedJSON:
			var compact, indented bytes.Buffer
			if err := json.Compact(&compact, []byte(stdout)); err != nil {
				t.Fatalf("%v printed %q, not JSON: %v", step.args, stdout, err)
			}
			if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
				t.Fatal(err)
			}
			if indented.String()+"\n" != stdout {
				t.Fatalf("%v printed %q, want its JSON indented: %q", step.args, stdout, indented.String()+"\n")
			}
		}
	}
}
