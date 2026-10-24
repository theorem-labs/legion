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
// exits 1 with refusal_conflict, and owners shows each owner's refusal only while its work is the refused work.
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

	code, stdout, stderr := cli.call("--json", "refuse-owner", "r", refused, strconv.FormatInt(revision, 10), "owner id outside the accepted forms")
	if code != 1 || !strings.Contains(stdout, `"reason":"refusal_conflict"`) {
		t.Fatalf("conflicting refusal: code=%d stdout=%q stderr=%q, want exit 1 with refusal_conflict", code, stdout, stderr)
	}
	if n := cli.refusedEvents(refused); n != 1 {
		t.Fatalf("%d owner.refused events after a conflicting refusal, want 1", n)
	}
	cli.assertOwners(cliOwnerLine{Owner: refused, Refusal: &first}, cliOwnerLine{Owner: other})

	newer := cli.enqueue("item-3", refused)
	cli.assertOwners(cliOwnerLine{Owner: refused}, cliOwnerLine{Owner: other})
	second := cli.refuse(refused, newer, "owner id outside the accepted forms")
	cli.assertOwners(cliOwnerLine{Owner: refused, Refusal: &second}, cliOwnerLine{Owner: other})
	if n := cli.refusedEvents(refused); n != 2 {
		t.Fatalf("%d owner.refused events after a refusal at the new revision, want 2", n)
	}
}

// Every malformed refuse-owner exits 2 and records nothing: a wrong argument count, an empty owner or reason, a
// revision below 1 or not an integer, and a revision newer than the owner's work revision, whose error names the current
// one.
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
		{"not a number", []string{"refuse-owner", "r", owner, canonical + "x", "no template"}, "revision"},
		{"newer revision", []string{"refuse-owner", "r", owner, strconv.FormatInt(revision+1, 10), "no template"}, "work revision " + canonical},
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
