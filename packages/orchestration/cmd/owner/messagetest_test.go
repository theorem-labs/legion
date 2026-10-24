package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"github.com/sjawhar/legion/orchestration/owner/messagetest"
	"gopkg.in/yaml.v3"
)

// heldAnswer runs `message-test answer` of item for owner under a hold of the driver's at moment, waits until the
// command says it is held, and returns what it said and the cancel that kills it, with the channel of its exit code.
func (f *commandFixture) heldAnswer(owner, item, moment string) (messagetest.Hold, context.CancelFunc, <-chan int) {
	f.t.Helper()
	if err := messagetest.PutHold(f.stateDir(), item, moment); err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	f.t.Cleanup(cancel)
	exit := make(chan int, 1)
	run := f.as(owner)
	go func() {
		_, _, code := run.runContext(ctx, run.dir(), "message-test", "answer", item)
		exit <- code
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		held, found, err := messagetest.ReadHeld(f.stateDir())
		if err != nil {
			f.t.Fatal(err)
		}
		if found {
			return held, cancel, exit
		}
		select {
		case code := <-exit:
			f.t.Fatalf("message-test answer %s exited %d without pausing at the %s hold", item, code, moment)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("message-test answer %s never paused at the %s hold", item, moment)
		}
	}
}

// enqueueCount is how many times the ledger enqueued item.
func (f *commandFixture) enqueueCount(item string) int {
	f.t.Helper()
	events, err := f.store.Events(f.ctx, "r", 0, item)
	if err != nil {
		f.t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "item.enqueued" {
			count++
		}
	}
	return count
}

// A kill after the answer lands between the record commit and the completion: the driver's hold pauses the answer
// there, with the record committed on the owner's branch and the follow-up queued, and the item still open. A killed
// command (its context cancelled) leaves the item open; once the supervisor frees its lease, the relaunched owner's
// answer completes it with the same record, once, and queues no second follow-up.
func TestMessageTestAnswerPausesAfterTheAnswerBeforeTheCompletion(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need, offer := "need:"+edge+"@r4", "offer:"+edge+"@r1"
	f.enqueue(need, state)
	held, kill, exit := f.heldAnswer(state, need, messagetest.MomentAfterAnswer)
	if held.Item != need || held.Moment != messagetest.MomentAfterAnswer || held.PID != os.Getpid() {
		t.Fatalf("held = %+v", held)
	}
	path := messagetest.AnswerRecord("skeleton", state, "need", edge)
	record := f.lastCommit(state, path)
	if record == "" || f.trailer(record, "Owner-Item") != need {
		t.Fatalf("at the hold the record of %s is %q, want a commit with its Owner-Item trailer", need, record)
	}
	if item := f.item(need); item.State != "open" || item.Holder != state {
		t.Fatalf("at the hold %s = %+v, want open and held by %s", need, item, state)
	}
	if item := f.item(offer); item.State != "open" || item.Owner != caller {
		t.Fatalf("at the hold the follow-up %s = %+v", offer, item)
	}
	kill()
	if code := <-exit; code == 0 {
		t.Fatal("a killed answer exited 0")
	}
	if item := f.item(need); item.State != "open" {
		t.Fatalf("after the kill %s = %+v, want open", need, item)
	}
	if _, err := f.store.ReleaseHolder(f.ctx, "r", state, "owner pane stopped"); err != nil {
		t.Fatal(err)
	}
	if err := messagetest.ReleaseHold(f.stateDir()); err != nil {
		t.Fatal(err)
	}
	f.as(state).ok("message-test", "answer", need)
	if item := f.item(need); item.State != "done" || item.Result != record {
		t.Fatalf("the relaunched answer: %s = %+v, want done with the record %s", need, item, record)
	}
	if n := f.commitCount(state, path); n != 1 {
		t.Fatalf("answer commits = %d, want 1", n)
	}
	if n := f.enqueueCount(offer); n != 1 {
		t.Fatalf("%s was enqueued %d times", offer, n)
	}
}

// A kill after the claim lands before the record: the hold pauses the answer with the item claimed by the owner
// (claimed by the answer itself when the owner did not run `claim` first) and nothing recorded.
func TestMessageTestAnswerPausesAfterTheClaimBeforeTheRecord(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need := "need:" + edge + "@r5"
	f.enqueue(need, state)
	held, kill, exit := f.heldAnswer(state, need, messagetest.MomentAfterClaim)
	if held.Item != need || held.Moment != messagetest.MomentAfterClaim {
		t.Fatalf("held = %+v", held)
	}
	path := messagetest.AnswerRecord("skeleton", state, "need", edge)
	if item := f.item(need); item.State != "open" || item.Holder != state {
		t.Fatalf("at the hold %s = %+v, want claimed by %s", need, item, state)
	}
	if n := f.commitCount(state, path); n != 0 {
		t.Fatalf("at the hold %d answer commits exist, want none", n)
	}
	kill()
	<-exit
	if _, err := f.store.ReleaseHolder(f.ctx, "r", state, "owner pane stopped"); err != nil {
		t.Fatal(err)
	}
	if err := messagetest.ReleaseHold(f.stateDir()); err != nil {
		t.Fatal(err)
	}
	f.as(state).ok("claim", need)
	f.as(state).ok("message-test", "answer", need)
	if item := f.item(need); item.State != "done" || item.Result != f.lastCommit(state, path) || f.commitCount(state, path) != 1 {
		t.Fatalf("the relaunched answer: %s = %+v with %d commits", need, item, f.commitCount(state, path))
	}
}

const prefix = "notifications.example."

func (f *commandFixture) messageTopics(owner string) []string {
	f.t.Helper()
	var result struct{ Topics []string }
	if err := json.Unmarshal([]byte(f.as(owner).ok("--json", "message-test", "topics")), &result); err != nil {
		f.t.Fatal(err)
	}
	return result.Topics
}

func (f *commandFixture) commitCount(owner, path string) int {
	f.t.Helper()
	out := f.git(f.root, "--git-dir="+f.bare, "log", "--format=%H", "owner/"+ownerpkg.Key(owner), "--", path)
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func TestMessageTestTopicsAddTheDriversTopicsToOwnerTopics(t *testing.T) {
	f := newCommandFixture(t)
	extra := []string{prefix + "fact.stack-alignment.needs", prefix + "concepts"}
	data, _ := json.Marshal(map[string][]string{caller: extra})
	f.writeFile(messagetest.ExtraTopicsPath(f.stateDir()), string(data))

	var plain struct{ Topics []string }
	if err := json.Unmarshal([]byte(f.as(caller).ok("--json", "topics")), &plain); err != nil {
		t.Fatal(err)
	}
	if got, want := f.messageTopics(caller), append(plain.Topics, extra...); !reflect.DeepEqual(got, want) {
		t.Fatalf("message-test topics = %v, want %v", got, want)
	}
	if err := json.Unmarshal([]byte(f.as(state).ok("--json", "topics")), &plain); err != nil {
		t.Fatal(err)
	}
	if got := f.messageTopics(state); !reflect.DeepEqual(got, plain.Topics) {
		t.Fatalf("message-test topics of an owner with no extra topics = %v, want %v", got, plain.Topics)
	}
}

func TestMessageTestSubscribedRecordsTheTopicsOnce(t *testing.T) {
	f := newCommandFixture(t)
	f.as(caller).ok("message-test", "subscribed")
	f.as(caller).ok("message-test", "subscribed")
	path := messagetest.SubscribedRecord("skeleton", caller)
	if n := f.commitCount(caller, path); n != 1 {
		t.Fatalf("subscribed commits = %d, want 1", n)
	}
	var record messagetest.Subscribed
	if err := yaml.Unmarshal(f.show(caller, path), &record); err != nil {
		t.Fatal(err)
	}
	if record.Owner != caller || !reflect.DeepEqual(record.Topics, f.messageTopics(caller)) {
		t.Fatalf("subscribed record = %+v", record)
	}
	if trailer := f.trailer(f.lastCommit(caller, path), "Owner-Item"); trailer != "" {
		t.Fatalf("subscribed commit carries Owner-Item %q", trailer)
	}
}

func TestMessageTestSawRecordsEachDeliveryOnce(t *testing.T) {
	f := newCommandFixture(t)
	topic := prefix + "all"
	f.as(state).ok("message-test", "saw", "evt-1", topic)
	f.as(state).ok("message-test", "saw", "evt-1", topic)
	f.as(state).ok("message-test", "saw", "evt-2", topic)
	path := messagetest.SawRecord("skeleton", state, "evt-1")
	if n := f.commitCount(state, path); n != 1 {
		t.Fatalf("saw commits of evt-1 = %d, want 1", n)
	}
	commit := f.lastCommit(state, path)
	if got := f.trailer(commit, messagetest.DeliveryTrailer); got != "evt-1" {
		t.Fatalf("delivery trailer = %q", got)
	}
	if got := f.trailer(commit, messagetest.TopicTrailer); got != topic {
		t.Fatalf("topic trailer = %q", got)
	}
	var record messagetest.Saw
	if err := yaml.Unmarshal(f.show(state, path), &record); err != nil {
		t.Fatal(err)
	}
	if record.Owner != state || record.Delivery != "evt-1" || record.Topic != topic || record.ReceivedAt.IsZero() {
		t.Fatalf("saw record = %+v", record)
	}
	if n := f.commitCount(state, messagetest.SawRecord("skeleton", state, "evt-2")); n != 1 {
		t.Fatalf("saw commits of evt-2 = %d, want 1", n)
	}
}

func TestMessageTestAnswerRecordsCompletesAndQueuesTheOfferOnce(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need, offer := "need:"+edge+"@r1", "offer:"+edge+"@r1"
	f.enqueue(need, state)
	f.as(state).ok("claim", need)
	f.as(state).ok("message-test", "answer", need)
	f.as(state).ok("message-test", "answer", need)

	path := messagetest.AnswerRecord("skeleton", state, "need", edge)
	if n := f.commitCount(state, path); n != 1 {
		t.Fatalf("answer commits = %d, want 1", n)
	}
	commit := f.lastCommit(state, path)
	if got := f.trailer(commit, "Owner-Item"); got != need {
		t.Fatalf("answer Owner-Item = %q, want %q", got, need)
	}
	if item := f.item(need); item.State != "done" || item.Result != commit {
		t.Fatalf("need = %+v, want done with %s", item, commit)
	}
	if item := f.item(offer); item.State != "open" || item.Owner != caller {
		t.Fatalf("offer = %+v, want open for %s", item, caller)
	}
	var record messagetest.Answer
	if err := yaml.Unmarshal(f.show(state, path), &record); err != nil {
		t.Fatal(err)
	}
	if record != (messagetest.Answer{Item: need, Type: "need", Subject: edge, Version: "r1", Revision: 1, Text: messagetest.AnswerText}) {
		t.Fatalf("answer record = %+v", record)
	}
	if posts := f.notices(); len(posts) != 0 {
		t.Fatalf("answer published %+v", posts)
	}

	f.as(caller).ok("message-test", "answer", offer)
	if item := f.item(offer); item.State != "done" || item.Result != f.lastCommit(caller, messagetest.AnswerRecord("skeleton", caller, "offer", edge)) {
		t.Fatalf("offer = %+v, want done with its record", item)
	}
	start := "start:" + state + "@1"
	f.enqueue(start, state)
	f.as(state).ok("message-test", "answer", start)
	if item := f.item(start); item.State != "done" || item.Result != f.lastCommit(state, messagetest.AnswerRecord("skeleton", state, "start", state)) {
		t.Fatalf("start = %+v, want done with its record", item)
	}
}

// An answer to a need queues the offer that production's offer queues: offer:<edge>@r<n>, where n is the revision of
// the record that answers the need, whatever the need's own version. A newer version of the need rewrites the record
// as the next revision; a second run of one answer repeats its revision.
func TestMessageTestAnswerNumbersTheOfferByItsRecordRevision(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	path := messagetest.AnswerRecord("skeleton", state, "need", edge)
	for index, version := range []string{"r3", "r5"} {
		need := "need:" + edge + "@" + version
		f.enqueue(need, state)
		f.as(state).ok("message-test", "answer", need)
		f.as(state).ok("message-test", "answer", need)
		offer := fmt.Sprintf("offer:%s@r%d", edge, index+1)
		if item := f.item(offer); item.State != "open" || item.Owner != caller {
			t.Fatalf("the answer of %s: %s = %+v, want open for %s", need, offer, item, caller)
		}
		var record map[string]any
		if err := yaml.Unmarshal(f.show(state, path), &record); err != nil {
			t.Fatal(err)
		}
		if record["revision"] != index+1 || record["version"] != version {
			t.Fatalf("the answer record of %s = %v, want revision %d", need, record, index+1)
		}
	}
	for _, version := range []string{"r3", "r5"} {
		if _, err := f.store.Item(f.ctx, "r", "offer:"+edge+"@"+version); !isNotFound(err) {
			t.Fatalf("an offer is numbered by the need version %s: %v", version, err)
		}
	}
	if n := f.commitCount(state, path); n != 2 {
		t.Fatalf("answer commits = %d, want one for each version", n)
	}
}

// An item older than the version the owner already answered completes as superseded, with no record and no follow-up.
func TestMessageTestAnswerSupersedesAnOlderVersion(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	r2, r3 := "need:"+edge+"@r2", "need:"+edge+"@r3"
	f.enqueue(r3, state)
	f.enqueue(r2, state)
	f.as(state).ok("message-test", "answer", r3)
	f.as(state).ok("message-test", "answer", r2)
	if item := f.item(r2); item.State != "done" || item.Result != "superseded:r3" {
		t.Fatalf("r2 = %+v, want done as superseded:r3", item)
	}
	out := f.git(f.root, "--git-dir="+f.bare, "log", "--format=%(trailers:key=Owner-Item,valueonly)", "owner/"+ownerpkg.Key(state))
	if strings.Contains(out, r2) {
		t.Fatalf("a commit carries Owner-Item %s:\n%s", r2, out)
	}
	if _, err := f.store.Item(f.ctx, "r", "offer:"+edge+"@r2"); !isNotFound(err) {
		t.Fatalf("offer r2 exists: %v", err)
	}
}

func TestMessageTestAnswerRefusesAnotherOwnersItem(t *testing.T) {
	f := newCommandFixture(t)
	need := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	f.enqueue(need, state)
	if code, stderr := f.as(caller).fails("message-test", "answer", need); code != exitRefused {
		t.Fatalf("answer of another owner's item exited %d: %s", code, stderr)
	}
}

func TestMessageTestUsage(t *testing.T) {
	f := newCommandFixture(t)
	for _, args := range [][]string{
		{"message-test"},
		{"message-test", "saw", "evt-1"},
		{"message-test", "answer"},
		{"message-test", "answer", "agreed:" + ownerpkg.EdgeID(caller, state) + "@1"},
		{"message-test", "topics", "extra"},
		{"message-test", "--out"},
		{"message-test", "--out", f.root + "/run", "--nodes", caller},
		{"message-test", "--out", f.root + "/run", "--kills", "-1"},
		{"message-test", "--out", f.root + "/run", "--kills", "0"},
		{"message-test", "--out", f.root + "/run", "--kills", "2"},
		{"message-test", "--out", f.root + "/run", "--frobnicate"},
	} {
		if code, stderr := f.as(caller).fails(args...); code != exitUsage {
			t.Errorf("owner %v exited %d, want 2: %s", args, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "run")); err == nil {
		t.Fatal("a refused message-test made its output directory")
	}
}

// A full run kills one owner pane at each of the three moments of a step (before the claim, after the claim, after
// the answer), so a count of kills below three, 0 included, is a usage error: such a run could not show every moment.
func TestParseDriverFlagsRefusesFewerKillsThanMoments(t *testing.T) {
	for _, kills := range []string{"0", "1", "2"} {
		_, err := parseDriverFlags([]string{"--kills", kills, "--out", "/runs/one"})
		var usage *usageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "at least 3") {
			t.Errorf("--kills %s: %v, want a usage error that asks for at least 3", kills, err)
		}
	}
	if flags, err := parseDriverFlags([]string{"--kills", "3", "--out", "/runs/one"}); err != nil || flags.kills != 3 {
		t.Fatalf("--kills 3 = %+v, %v", flags, err)
	}
}

// --allow-missing-kinds names the topic kinds the nodes may lack, comma-separated; a name that is no topic kind is a
// usage error.
func TestParseDriverFlagsReadsTheAllowedMissingKinds(t *testing.T) {
	flags, err := parseDriverFlags([]string{"--allow-missing-kinds", "linked,callees", "--out", "/runs/one"})
	if err != nil || !reflect.DeepEqual(flags.allowMissingKinds, []string{"linked", "callees"}) {
		t.Fatalf("--allow-missing-kinds linked,callees = %+v, %v", flags, err)
	}
	for _, value := range []string{"linkd", "linked,", ""} {
		_, err := parseDriverFlags([]string{"--allow-missing-kinds", value, "--out", "/runs/one"})
		var usage *usageError
		if !errors.As(err, &usage) {
			t.Errorf("--allow-missing-kinds %q: %v, want a usage error", value, err)
		}
	}
}
