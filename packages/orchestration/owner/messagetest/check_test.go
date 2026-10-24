package messagetest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const (
	fn  = "function:get_mountpoint@fs/namespace.c"
	st  = "struct:mountpoint"
	all = "notifications.lxspec-message-test.all"
)

var (
	edge   = fn + "->" + st
	needR1 = "need:" + edge + "@r1"
	needR2 = "need:" + edge + "@r2"
	needR3 = "need:" + edge + "@r3"
	offrR1 = "offer:" + edge + "@r1"
	offrR2 = "offer:" + edge + "@r2"
	offrR3 = "offer:" + edge + "@r3"
	needs  = "notifications.lxspec-message-test.node.struct:mountpoint.needs"
)

// recording builds the ledger rows and git logs a run leaves, one call per row or commit.
type recording struct {
	recorded Recorded
	commits  int
}

func (r *recording) event(kind, subject, actor string, data map[string]any) {
	encoded, _ := json.Marshal(data)
	r.recorded.Events = append(r.recorded.Events, ledger.Event{ID: int64(len(r.recorded.Events) + 1), Kind: kind, Subject: subject, Actor: actor, Data: encoded})
}

func (r *recording) commit(owner string, trailers map[string][]string) string {
	r.commits++
	sha := fmt.Sprintf("%040x", r.commits)
	r.recorded.Commits = append(r.recorded.Commits, Commit{Owner: owner, SHA: sha, Trailers: trailers})
	return sha
}

func (r *recording) enqueue(item, owner string) {
	r.event("item.enqueued", item, owner, map[string]any{"owner": owner})
}

func (r *recording) complete(item, owner, result string) {
	r.event("item.completed", item, owner, map[string]any{"generation": 1, "result": result})
}

// answer is an owner's record commit for item, then the completion with that commit, as `answer` makes them.
func (r *recording) answer(item, owner string) string {
	sha := r.commit(owner, map[string][]string{"Owner-Item": {item}})
	r.complete(item, owner, sha)
	return sha
}

func (r *recording) saw(owner, id, topic string) {
	r.commit(owner, map[string][]string{DeliveryTrailer: {id}, TopicTrailer: {topic}})
}

// passingRun is the recorded state of a run where every step happened once: an arrival notice on all that both
// owners recorded, the need r1 enqueued twice and its notice published twice under two keys (Envoy's at-least-once
// delivery), both recorded and the need answered once, r3 answered and r2 superseded. The notice of a killed
// owner's need is optional: the kill may end the session before it records it.
func passingRun() (Expected, *recording) {
	expected := Expected{
		Deliveries: []ExpectedDelivery{
			{Owner: fn, Topic: all, ID: "evt-all", Required: true},
			{Owner: st, Topic: all, ID: "evt-all", Required: true},
			{Owner: st, Topic: needs, ID: "evt-dup-1", Required: true},
			{Owner: st, Topic: needs, ID: "evt-dup-2", Required: true},
			{Owner: st, Topic: needs, ID: "evt-kill"},
		},
		Outcomes: []Outcome{
			{Item: "start:" + fn + "@1", Owner: fn},
			{Item: "start:" + st + "@1", Owner: st},
			{Item: needR1, Owner: st, FollowUp: offrR1, FollowUpOwner: fn},
			{Item: offrR1, Owner: fn, Optional: true},
			{Item: needR3, Owner: st, FollowUp: offrR3, FollowUpOwner: fn},
			{Item: offrR3, Owner: fn, Optional: true},
			{Item: needR2, Owner: st, Superseded: "r3"},
		},
	}
	r := &recording{}
	for _, owner := range []string{fn, st} {
		r.enqueue("start:"+owner+"@1", owner)
		r.answer("start:"+owner+"@1", owner)
		r.saw(owner, "evt-all", all)
	}
	r.enqueue(needR1, st)
	r.saw(st, "evt-dup-1", needs)
	r.enqueue(offrR1, fn)
	r.answer(needR1, st)
	r.saw(st, "evt-dup-2", needs)
	r.answer(offrR1, fn)
	r.enqueue(needR3, st)
	r.enqueue(needR2, st)
	r.enqueue(offrR3, fn)
	r.answer(needR3, st)
	r.complete(needR2, st, "superseded:r3")
	return expected, r
}

func requireOnly(t *testing.T, failures []Failure, kind, subject string) {
	t.Helper()
	if len(failures) != 1 || failures[0].Kind != kind || !strings.Contains(failures[0].Subject, subject) {
		t.Fatalf("failures = %+v, want one %s failure about %q", failures, kind, subject)
	}
}

func TestCheckPassesARunWhereEveryStepHappenedOnce(t *testing.T) {
	expected, r := passingRun()
	if failures := Check(expected, r.recorded); len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}
}

func TestCheckFailsAMissedDelivery(t *testing.T) {
	expected, r := passingRun()
	expected.Deliveries = append(expected.Deliveries, ExpectedDelivery{Owner: fn, Topic: needs, ID: "evt-never", Required: true})
	requireOnly(t, Check(expected, r.recorded), FailMissed, "evt-never")
}

func TestCheckFailsADeliveryRecordedTwice(t *testing.T) {
	expected, r := passingRun()
	r.saw(st, "evt-all", all)
	requireOnly(t, Check(expected, r.recorded), FailDuplicate, "evt-all")
}

func TestCheckFailsADeliveryThatWasNeverPublished(t *testing.T) {
	expected, r := passingRun()
	r.saw(fn, "evt-stranger", all)
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, "evt-stranger")
}

func TestCheckFailsADeliveryRecordedOnTheWrongTopic(t *testing.T) {
	expected, r := passingRun()
	retopic(r.recorded.Commits, fn, "evt-all", needs)
	failures := Check(expected, r.recorded)
	if len(failures) != 2 || failures[0].Kind != FailMissed || failures[1].Kind != FailUnexpected {
		t.Fatalf("failures = %+v, want the all notice missed and a delivery on the wrong topic", failures)
	}
}

// A notice delivered a second time under another key is a second delivery: the owner records both, and still
// answers the need once.
func TestCheckFailsADuplicateNoticeRecordedOnlyOnce(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Commits = without(r.recorded.Commits, "evt-dup-2")
	requireOnly(t, Check(expected, r.recorded), FailMissed, "evt-dup-2")
}

func TestCheckAcceptsAnOptionalNoticeRecordedOnce(t *testing.T) {
	expected, r := passingRun()
	r.saw(st, "evt-kill", needs)
	if failures := Check(expected, r.recorded); len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}
	r.saw(st, "evt-kill", needs)
	requireOnly(t, Check(expected, r.recorded), FailDuplicate, "evt-kill")
}

func TestCheckFailsADuplicatedRecord(t *testing.T) {
	expected, r := passingRun()
	r.commit(st, map[string][]string{"Owner-Item": {needR1}})
	requireOnly(t, Check(expected, r.recorded), FailDuplicateRecord, needR1)
}

func TestCheckFailsAnItemCompletedTwice(t *testing.T) {
	expected, r := passingRun()
	r.complete(needR1, st, recordOf(r.recorded.Commits, needR1))
	failures := Check(expected, r.recorded)
	if len(failures) == 0 || failures[0].Kind != FailCompletedTwice || failures[0].Subject != needR1 {
		t.Fatalf("failures = %+v, want %s completed twice", failures, needR1)
	}
}

func TestCheckFailsAnOptionalFollowUpCompletedTwice(t *testing.T) {
	expected, r := passingRun()
	r.complete(offrR1, fn, "again")
	failures := Check(expected, r.recorded)
	if len(failures) == 0 || failures[0].Kind != FailCompletedTwice || failures[0].Subject != offrR1 {
		t.Fatalf("failures = %+v, want %s completed twice", failures, offrR1)
	}
}

func TestCheckFailsAnItemThatNeverCompleted(t *testing.T) {
	expected, r := passingRun()
	expected.Outcomes = append(expected.Outcomes, Outcome{Item: "need:" + edge + "@r4", Owner: st})
	r.enqueue("need:"+edge+"@r4", st)
	requireOnly(t, Check(expected, r.recorded), FailNotCompleted, "@r4")
}

func TestCheckFailsACompletionWithoutItsRecord(t *testing.T) {
	expected, r := passingRun()
	expected.Outcomes = append(expected.Outcomes, Outcome{Item: "need:" + edge + "@r4", Owner: st})
	r.enqueue("need:"+edge+"@r4", st)
	r.complete("need:"+edge+"@r4", st, "0000000000000000000000000000000000000abc")
	requireOnly(t, Check(expected, r.recorded), FailMissingRecord, "@r4")
}

func TestCheckFailsACompletionThatIsNotItsRecordCommit(t *testing.T) {
	expected, r := passingRun()
	expected.Outcomes = append(expected.Outcomes, Outcome{Item: "need:" + edge + "@r4", Owner: st})
	r.enqueue("need:"+edge+"@r4", st)
	r.commit(st, map[string][]string{"Owner-Item": {"need:" + edge + "@r4"}})
	r.complete("need:"+edge+"@r4", st, "some other commit")
	requireOnly(t, Check(expected, r.recorded), FailWrongResult, "@r4")
}

// An item that arrives after a newer version is completed as superseded, with no record and no follow-up.
func TestCheckFailsAnOutOfOrderItemAnsweredAsIfItWereCurrent(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Events = r.recorded.Events[:len(r.recorded.Events)-1]
	r.enqueue(offrR2, fn)
	r.answer(needR2, st)
	failures := Check(expected, r.recorded)
	kinds := map[string]bool{}
	for _, failure := range failures {
		kinds[failure.Kind+" "+failure.Subject] = true
	}
	for _, want := range []string{FailOutOfOrder + " " + needR2, FailUnexpected + " " + offrR2} {
		if !kinds[want] {
			t.Fatalf("failures = %+v, want %s", failures, want)
		}
	}
}

func TestCheckFailsAFollowUpEnqueuedTwiceOrNotAtAll(t *testing.T) {
	expected, r := passingRun()
	r.enqueue(offrR1, fn)
	requireOnly(t, Check(expected, r.recorded), FailFollowUp, offrR1)

	expected, r = passingRun()
	r.recorded.Events = withoutEvent(r.recorded.Events, "item.enqueued", offrR3)
	requireOnly(t, Check(expected, r.recorded), FailFollowUp, offrR3)
}

func TestCheckFailsAFollowUpForAnotherOwner(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Events = withoutEvent(r.recorded.Events, "item.enqueued", offrR3)
	r.enqueue(offrR3, st)
	requireOnly(t, Check(expected, r.recorded), FailFollowUp, offrR3)
}

func TestCheckFailsARecordOrCompletionOfAnUnexpectedItem(t *testing.T) {
	expected, r := passingRun()
	r.commit(fn, map[string][]string{"Owner-Item": {"need:" + edge + "@r9"}})
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, "@r9")

	expected, r = passingRun()
	r.complete("need:"+edge+"@r9", st, "x")
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, "@r9")
}

func TestCheckFailsARecordOnAnotherOwnersBranch(t *testing.T) {
	expected, r := passingRun()
	r.commit(fn, map[string][]string{"Owner-Item": {needR3}})
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, needR3)
}

func without(commits []Commit, delivery string) []Commit {
	var kept []Commit
	for _, commit := range commits {
		if len(commit.Trailers[DeliveryTrailer]) == 1 && commit.Trailers[DeliveryTrailer][0] == delivery {
			continue
		}
		kept = append(kept, commit)
	}
	return kept
}

func withoutEvent(events []ledger.Event, kind, subject string) []ledger.Event {
	var kept []ledger.Event
	for _, event := range events {
		if event.Kind == kind && event.Subject == subject {
			continue
		}
		kept = append(kept, event)
	}
	return kept
}

func retopic(commits []Commit, owner, delivery, topic string) {
	for index, commit := range commits {
		if commit.Owner == owner && len(commit.Trailers[DeliveryTrailer]) == 1 && commit.Trailers[DeliveryTrailer][0] == delivery {
			commits[index].Trailers = map[string][]string{DeliveryTrailer: {delivery}, TopicTrailer: {topic}}
		}
	}
}

func recordOf(commits []Commit, item string) string {
	for _, commit := range commits {
		if len(commit.Trailers["Owner-Item"]) == 1 && commit.Trailers["Owner-Item"][0] == item {
			return commit.SHA
		}
	}
	return ""
}
