package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

type ownerRefusalCLI struct {
	t   *testing.T
	dsn string
}

type cliOwnerRefusal struct {
	Owner    string    `json:"owner"`
	Revision int64     `json:"revision"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

type cliOwnerLine struct {
	Owner   string           `json:"owner"`
	Refusal *cliOwnerRefusal `json:"refusal"`
}

func newOwnerRefusalCLI(t *testing.T) ownerRefusalCLI {
	t.Helper()
	cli := ownerRefusalCLI{t: t, dsn: testpg.Database(t, "ledger_owner_refusals")}
	cli.invoke("repo", "ensure", "r", "3")
	return cli
}

func (cli ownerRefusalCLI) call(args ...string) (int, string, string) {
	cli.t.Helper()
	var out, err bytes.Buffer
	code := run(context.Background(), args, func(key string) string {
		if key == "LEDGER_DSN" {
			return cli.dsn
		}
		return ""
	}, &out, &err)
	return code, out.String(), err.String()
}

func (cli ownerRefusalCLI) invoke(args ...string) string {
	cli.t.Helper()
	code, stdout, stderr := cli.call(args...)
	if code != 0 {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	return stdout
}

func (cli ownerRefusalCLI) decode(output string, value any) {
	cli.t.Helper()
	if err := json.Unmarshal([]byte(output), value); err != nil {
		cli.t.Fatalf("decode %q: %v", output, err)
	}
}

// enqueue enqueues item for owner and returns the owner's work revision, which is the id of the item's item.enqueued
// event.
func (cli ownerRefusalCLI) enqueue(item, owner string) int64 {
	cli.t.Helper()
	cli.invoke("enqueue", "r", item, owner)
	var events []struct {
		ID   int64  `json:"id"`
		Kind string `json:"kind"`
	}
	cli.decode(cli.invoke("--json", "events", "r", "0", item), &events)
	for _, event := range events {
		if event.Kind == "item.enqueued" {
			return event.ID
		}
	}
	cli.t.Fatalf("no item.enqueued event for %s: %+v", item, events)
	return 0
}

func (cli ownerRefusalCLI) refuse(owner string, revision int64, reason string) cliOwnerRefusal {
	cli.t.Helper()
	var refusal cliOwnerRefusal
	cli.decode(cli.invoke("--json", "refuse-owner", "r", owner, strconv.FormatInt(revision, 10), reason), &refusal)
	return refusal
}

// refusedEvents counts the owner.refused events of owner, each of which has its owner as its subject.
func (cli ownerRefusalCLI) refusedEvents(owner string) int {
	cli.t.Helper()
	var events []struct {
		Kind string `json:"kind"`
	}
	cli.decode(cli.invoke("--json", "events", "r", "0", owner), &events)
	count := 0
	for _, event := range events {
		if event.Kind == "owner.refused" {
			count++
		}
	}
	return count
}

// assertOwners reads `owners` and compares it with want. An owner with no refusal has no refusal field at all, and
// the command prints the same JSON with or without --json.
func (cli ownerRefusalCLI) assertOwners(want ...cliOwnerLine) {
	cli.t.Helper()
	output := cli.invoke("--json", "owners", "r")
	if plain := cli.invoke("owners", "r"); plain != output {
		cli.t.Fatalf("owners without --json printed %q, want the JSON %q", plain, output)
	}
	var got []cliOwnerLine
	cli.decode(output, &got)
	var fields []map[string]json.RawMessage
	cli.decode(output, &fields)
	if len(got) != len(want) {
		cli.t.Fatalf("owners = %s, want %+v", output, want)
	}
	for i := range want {
		if got[i].Owner != want[i].Owner || (got[i].Refusal == nil) != (want[i].Refusal == nil) {
			cli.t.Fatalf("owner %d = %s, want %+v", i, output, want[i])
		}
		if want[i].Refusal == nil {
			if _, ok := fields[i]["refusal"]; ok {
				cli.t.Fatalf("owner %s has no refusal but prints the field: %s", want[i].Owner, output)
			}
			continue
		}
		g, w := got[i].Refusal, want[i].Refusal
		if g.Owner != w.Owner || g.Revision != w.Revision || g.Reason != w.Reason || !g.At.Equal(w.At) {
			cli.t.Fatalf("owner %s refusal = %+v, want %+v", want[i].Owner, *g, *w)
		}
	}
}

// refuse-owner records one refusal per owner and revision, a repeat appends nothing, another reason at that revision
// replaces the stored one with one more event, and owners shows each owner's refusal only while its work is the refused
// work.
func TestRefuseOwnerCommandRecordsOneRefusalPerRevision(t *testing.T) {
	cli := newOwnerRefusalCLI(t)
	const refused, other = "function:get_mountpoint", "struct:mount"
	revision := cli.enqueue("item-1", refused)
	cli.enqueue("item-2", other)
	cli.assertOwners(cliOwnerLine{Owner: refused}, cliOwnerLine{Owner: other})

	first := cli.refuse(refused, revision, "no template")
	if first.Owner != refused || first.Revision != revision || first.Reason != "no template" || first.At.IsZero() {
		t.Fatalf("first refusal = %+v", first)
	}
	if repeat := cli.refuse(refused, revision, "no template"); repeat.Reason != first.Reason || !repeat.At.Equal(first.At) {
		t.Fatalf("repeated refusal = %+v, want the stored %+v", repeat, first)
	}
	if n := cli.refusedEvents(refused); n != 1 {
		t.Fatalf("%d owner.refused events after a repeat, want 1", n)
	}
	cli.assertOwners(cliOwnerLine{Owner: refused, Refusal: &first}, cliOwnerLine{Owner: other})

	latest := cli.refuse(refused, revision, "owner id outside the accepted forms")
	if latest.Revision != revision || latest.Reason != "owner id outside the accepted forms" {
		t.Fatalf("refusal with another reason = %+v", latest)
	}
	if n := cli.refusedEvents(refused); n != 2 {
		t.Fatalf("%d owner.refused events after another reason, want 2", n)
	}
	cli.assertOwners(cliOwnerLine{Owner: refused, Refusal: &latest}, cliOwnerLine{Owner: other})

	newer := cli.enqueue("item-3", refused)
	cli.assertOwners(cliOwnerLine{Owner: refused}, cliOwnerLine{Owner: other})
	second := cli.refuse(refused, newer, "owner id outside the accepted forms")
	cli.assertOwners(cliOwnerLine{Owner: refused, Refusal: &second}, cliOwnerLine{Owner: other})
	if n := cli.refusedEvents(refused); n != 3 {
		t.Fatalf("%d owner.refused events after a refusal at the new revision, want 3", n)
	}
}

// Every malformed refuse-owner exits 2 and records nothing: a wrong argument count, an empty owner or reason, and a
// revision below 1 or not an integer.
func TestRefuseOwnerCommandInvalidInputExitsTwo(t *testing.T) {
	cli := newOwnerRefusalCLI(t)
	const owner = "struct:mount"
	revision := cli.enqueue("item-1", owner)
	canonical := strconv.FormatInt(revision, 10)
	for _, test := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"too few arguments", []string{"refuse-owner", "r", owner, canonical}, "refuse-owner <repo>"},
		{"too many arguments", []string{"refuse-owner", "r", owner, canonical, "no template", "extra"}, "refuse-owner <repo>"},
		{"empty owner", []string{"refuse-owner", "r", "", canonical, "no template"}, "empty owner"},
		{"empty reason", []string{"refuse-owner", "r", owner, canonical, ""}, "empty reason"},
		{"zero revision", []string{"refuse-owner", "r", owner, "0", "no template"}, "revision 0"},
		{"negative revision", []string{"refuse-owner", "r", owner, "-1", "no template"}, "revision -1"},
		{"not a number", []string{"refuse-owner", "r", owner, canonical + "x", "no template"}, `invalid integer "` + canonical + `x"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := cli.call(test.args...)
			if code != 2 || !strings.Contains(stderr, test.stderr) {
				t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
			}
		})
	}
	if n := cli.refusedEvents(owner); n != 0 {
		t.Fatalf("invalid refusals appended %d owner.refused events", n)
	}
	cli.assertOwners(cliOwnerLine{Owner: owner})

	refusal := cli.refuse(owner, revision, "no template")
	if refusal.Revision != revision {
		t.Fatalf("canonical revision %s recorded as %d", canonical, refusal.Revision)
	}
	cli.assertOwners(cliOwnerLine{Owner: owner, Refusal: &refusal})
	if code, stdout, stderr := cli.call("refuse-owner", "missing", owner, canonical, "no template"); code != 3 {
		t.Fatalf("refusal in a missing repository: code=%d stdout=%q stderr=%q, want 3", code, stdout, stderr)
	}
}

// The revision is an integer, so every decimal form of it names the same work revision: "07" and "+7" are 7, and a
// refusal through either repeats the refusal at 7 rather than recording another.
func TestRefuseOwnerCommandReadsTheRevisionAsAnInteger(t *testing.T) {
	cli := newOwnerRefusalCLI(t)
	const owner = "struct:mount"
	revision := cli.enqueue("item-1", owner)
	canonical := strconv.FormatInt(revision, 10)
	first := cli.refuse(owner, revision, "no template")
	for _, form := range []string{"0" + canonical, "+" + canonical} {
		var repeat cliOwnerRefusal
		cli.decode(cli.invoke("--json", "refuse-owner", "r", owner, form, "no template"), &repeat)
		if repeat.Revision != revision || !repeat.At.Equal(first.At) {
			t.Fatalf("refusal at %q = %+v, want the stored refusal %+v", form, repeat, first)
		}
	}
	if n := cli.refusedEvents(owner); n != 1 {
		t.Fatalf("%d owner.refused events, want 1", n)
	}
}

// enqueue, owners, a supervised claim and release-holder round-trip through the command line: the
// owner is the exact node id, the stored item and its item.enqueued event say so, and the exit codes
// are 0 for an enqueue or a repeat, 1 for an item another owner holds, and 2 for usage errors.
func TestOwnerCommandsRoundTrip(t *testing.T) {
	call := ledgerCommand(t)
	invoke := func(args ...string) string {
		t.Helper()
		return mustRun(t, call, args...)
	}
	invoke("repo", "ensure", "r", "3")
	invoke("enqueue", "r", "item-1", "function:get_mountpoint")
	invoke("enqueue", "r", "item-1", "function:get_mountpoint")
	invoke("enqueue", "r", "item-2", "struct:mount")

	var item struct {
		Owner string `json:"owner"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &item); err != nil ||
		item.Owner != "function:get_mountpoint" || item.State != "open" {
		t.Fatalf("enqueued item: %v %+v", err, item)
	}
	var log []struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
		Data    struct {
			Owner string `json:"owner"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "events", "r", "0", "item-1")), &log); err != nil {
		t.Fatal(err)
	}
	enqueued := 0
	for _, e := range log {
		if e.Kind == "item.enqueued" {
			enqueued++
			if e.Data.Owner != "function:get_mountpoint" {
				t.Fatalf("enqueue event: %+v", e)
			}
		}
	}
	if enqueued != 1 {
		t.Fatalf("%d item.enqueued events for item-1, want 1: %+v", enqueued, log)
	}

	var owners []struct {
		Owner   string           `json:"owner"`
		Refusal *json.RawMessage `json:"refusal"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "owners", "r")), &owners); err != nil ||
		len(owners) != 2 || owners[0].Owner != "function:get_mountpoint" || owners[1].Owner != "struct:mount" ||
		owners[0].Refusal != nil || owners[1].Refusal != nil {
		t.Fatalf("open owners: %v %+v", err, owners)
	}

	if code, out, _ := call("--json", "enqueue", "r", "item-1", "struct:mount"); code != 1 || !strings.Contains(out, `"reason":"exists"`) {
		t.Fatalf("enqueue for another owner: code %d output %q", code, out)
	}
	for _, usage := range [][]string{{"enqueue", "r", "item-1"}, {"enqueue", "r", "", "struct:mount"}, {"owners"}, {"release-holder", "r", "x"}} {
		if code, _, _ := call(usage...); code != 2 {
			t.Fatalf("%v exited %d, want 2", usage, code)
		}
	}

	invoke("claim", "r", "item-1", "function:get_mountpoint", "0")
	var held struct {
		Holder    string     `json:"holder"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &held); err != nil ||
		held.Holder != "function:get_mountpoint" || held.ExpiresAt != nil {
		t.Fatalf("supervised claim: %v %+v", err, held)
	}
	var freed []struct {
		ID         string `json:"id"`
		Generation int64  `json:"generation"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "release-holder", "r", "function:get_mountpoint", "pane gone")), &freed); err != nil ||
		len(freed) != 1 || freed[0].ID != "item-1" || freed[0].Generation != 1 {
		t.Fatalf("released holder: %v %+v", err, freed)
	}
	if code, _, _ := call("complete", "r", "item-1", "function:get_mountpoint", "1", "late"); code != 1 {
		t.Fatalf("late result exited %d, want the refusal's 1", code)
	}
	var after struct {
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &after); err != nil || after.Holder != "" {
		t.Fatalf("item after release-holder: %v %+v", err, after)
	}
	invoke("claim", "r", "item-2", "struct:mount", "0")
	invoke("complete", "r", "item-2", "struct:mount", "1", "done")
	owners = nil
	if err := json.Unmarshal([]byte(invoke("--json", "owners", "r")), &owners); err != nil ||
		len(owners) != 1 || owners[0].Owner != "function:get_mountpoint" || owners[0].Refusal != nil {
		t.Fatalf("open owners after a completion: %v %+v", err, owners)
	}
	if code, _, _ := call("--json", "enqueue", "r", "item-2", "struct:mount"); code != 1 {
		t.Fatalf("enqueue of a done item exited %d, want 1", code)
	}
}
