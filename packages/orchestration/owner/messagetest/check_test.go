package messagetest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const (
	fn  = "function:get_mountpoint@fs/namespace.c"
	st  = "struct:mountpoint"
	all = "notifications.example-message-test.all"
)

var (
	edge   = fn + "->" + st
	needR1 = "need:" + edge + "@r1"
	needR2 = "need:" + edge + "@r2"
	needR3 = "need:" + edge + "@r3"
	offrR1 = "offer:" + edge + "@r1"
	offrR2 = "offer:" + edge + "@r2"
	offrR3 = "offer:" + edge + "@r3"
	needs  = "notifications.example-message-test.node.struct:mountpoint.needs"
	users  = "notifications.example-message-test.node.struct:mountpoint.users"
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

// saw is the owner model's record of a delivery, which decides nothing.
func (r *recording) saw(owner, id, topic string) {
	r.commit(owner, map[string][]string{DeliveryTrailer: {id}, TopicTrailer: {topic}})
}

// arrive is the owner session's transcript recording one of the driver's notices.
func (r *recording) arrive(owner, id, topic string) {
	r.recorded.Arrivals = append(r.recorded.Arrivals, Arrival{Owner: owner, ID: id, Type: noticeType, Run: "r", Topic: topic, Subject: topic,
		Transcript: "/sessions/" + owner + ".jsonl", Line: len(r.recorded.Arrivals) + 1})
}

// ownerNotice is the source owner's session recording the target owner's own notice of an offer: published by the
// target's answer through the routing table's offer row, on the data structure's users topic.
func (r *recording) ownerNotice(id, offer, topic string) {
	r.recorded.Arrivals = append(r.recorded.Arrivals, Arrival{Owner: fn, ID: id, Type: "offer", From: st, Item: offer, Topic: topic, Subject: topic,
		DedupeKey: "publish.k-" + offer, Transcript: "/sessions/" + fn + ".jsonl", Line: len(r.recorded.Arrivals) + 1})
}

// passingRun is the recorded state of a run where every step happened once: an arrival notice on all that both
// owners' sessions recorded, the need r1 enqueued twice and its notice published twice under two keys (Envoy's
// at-least-once delivery), both in the session and the need answered once, r3 answered and r2 superseded. The notice
// of a killed owner's need is optional: the kill may end the session before the notice reaches it. Each answer of a
// need published its offer's notice itself, and the source's session recorded it.
func passingRun() (Expected, *recording) {
	expected := Expected{
		Deliveries: []ExpectedDelivery{
			{Owner: fn, Topic: all, ID: "evt-all", Required: true},
			{Owner: st, Topic: all, ID: "evt-all", Required: true},
			{Owner: st, Topic: needs, ID: "evt-dup-1", Required: true},
			{Owner: st, Topic: needs, ID: "evt-dup-2", Required: true},
			{Owner: st, Topic: needs, ID: "evt-kill"},
		},
		OwnerNotices: []ExpectedOwnerNotice{
			{Step: "duplicates", Receiver: fn, From: st, Type: "offer", Item: offrR1, Topic: users},
			{Step: "out of order", Receiver: fn, From: st, Type: "offer", Item: offrR3, Topic: users},
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
		r.arrive(owner, "evt-all", all)
	}
	r.enqueue(needR1, st)
	r.arrive(st, "evt-dup-1", needs)
	r.enqueue(offrR1, fn)
	r.answer(needR1, st)
	r.ownerNotice("evt-offer-1", offrR1, users)
	r.arrive(st, "evt-dup-2", needs)
	r.answer(offrR1, fn)
	r.enqueue(needR3, st)
	r.enqueue(needR2, st)
	r.enqueue(offrR3, fn)
	r.answer(needR3, st)
	r.ownerNotice("evt-offer-3", offrR3, users)
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
	r.arrive(st, "evt-all", all)
	requireOnly(t, Check(expected, r.recorded), FailDuplicate, "evt-all")
}

// A notice of another run, or one the driver never published, reaching an owner's session fails the run; a delivery
// that is not the driver's notice is not the run's to judge.
func TestCheckFailsADeliveryThatWasNeverPublished(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Arrivals = append(r.recorded.Arrivals, Arrival{Owner: fn, ID: "evt-role", Topic: "notifications.role.x"})
	r.recorded.Arrivals = append(r.recorded.Arrivals, Arrival{Owner: fn, ID: "evt-stranger", Type: noticeType, Run: "another run", Topic: all})
	failures := Check(expected, r.recorded)
	requireOnly(t, failures, FailUnexpected, "evt-stranger")
	if !strings.Contains(failures[0].Detail, "another run") {
		t.Fatalf("the failure %+v does not name the run that published it", failures[0])
	}
}

// A delivery's topic is Envoy's record of it in the session's transcript: the envelope's topic, and the subject the
// session's subscription took it on. Either one that is not the topic the driver published on is a wrong-topic
// delivery, and fails the run.
func TestCheckFailsADeliveryRecordedOnTheWrongTopic(t *testing.T) {
	expected, r := passingRun()
	retopic(r.recorded.Arrivals, fn, "evt-all", needs)
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, "evt-all on "+needs)

	expected, r = passingRun()
	for index, arrival := range r.recorded.Arrivals {
		if arrival.Owner == fn && arrival.ID == "evt-all" {
			r.recorded.Arrivals[index].Subject = needs
		}
	}
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, "evt-all on "+needs)
}

// A notice delivered a second time under another key is a second delivery: both reach the session, and the owner
// still answers the need once.
func TestCheckFailsADuplicateNoticeRecordedOnlyOnce(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Arrivals = without(r.recorded.Arrivals, "evt-dup-2")
	requireOnly(t, Check(expected, r.recorded), FailMissed, "evt-dup-2")
}

func TestCheckAcceptsAnOptionalNoticeRecordedOnce(t *testing.T) {
	expected, r := passingRun()
	r.arrive(st, "evt-kill", needs)
	if failures := Check(expected, r.recorded); len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}
	r.arrive(st, "evt-kill", needs)
	requireOnly(t, Check(expected, r.recorded), FailDuplicate, "evt-kill")
}

// The message test requires a notice that one owner session publishes itself to reach the other's: the source's
// transcript must record the target's notice of each offer its answers queued. A run where one never arrived fails.
func TestCheckFailsAnOwnersNoticeThatNeverReachedTheOtherOwner(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Arrivals = without(r.recorded.Arrivals, "evt-offer-3")
	requireOnly(t, Check(expected, r.recorded), FailMissed, offrR3)
}

// An owner's notice is told from the driver's by its payload's publisher and type and the item it names, and it counts
// only on the topic the routing table gives it: a driver's notice naming the offer, one the source published itself,
// and the target's notice delivered on another topic are none of them the target's notice of the offer.
func TestCheckTellsAnOwnersNoticeByItsPublisherAndTopic(t *testing.T) {
	for name, change := range map[string]func(*Arrival){
		"the driver's notice":   func(a *Arrival) { a.Type, a.From = noticeType, "" },
		"the source's own":      func(a *Arrival) { a.From = fn },
		"an accept, not offer":  func(a *Arrival) { a.Type = "accept" },
		"another offer's item":  func(a *Arrival) { a.Item = offrR2 },
		"in the target session": func(a *Arrival) { a.Owner = st },
	} {
		expected, r := passingRun()
		for index := range r.recorded.Arrivals {
			if r.recorded.Arrivals[index].ID == "evt-offer-1" {
				change(&r.recorded.Arrivals[index])
			}
		}
		failures := Check(expected, r.recorded)
		if !slices.ContainsFunc(failures, func(f Failure) bool { return f.Kind == FailMissed && strings.Contains(f.Subject, offrR1) }) {
			t.Errorf("%s: failures = %+v, want %s missed", name, failures, offrR1)
		}
	}
	expected, r := passingRun()
	retopic(r.recorded.Arrivals, fn, "evt-offer-1", needs)
	requireOnly(t, Check(expected, r.recorded), FailUnexpected, offrR1+" on "+needs)
}

// The verdict reads the sessions' transcripts, never the owner model's records: a delivery the model recorded and the
// session did not is missed, and one the session recorded and the model did not is no failure (acceptance run 1 failed,
// and run 7 stalled, on a delivery that reached the session and that the model did not record).
func TestCheckJudgesDeliveriesByTheSessionNotByTheOwnersRecord(t *testing.T) {
	expected, r := passingRun()
	r.recorded.Arrivals = without(r.recorded.Arrivals, "evt-dup-1")
	r.saw(st, "evt-dup-1", needs)
	requireOnly(t, Check(expected, r.recorded), FailMissed, "evt-dup-1")

	expected, r = passingRun()
	r.saw(st, "evt-dup-2", needs)
	r.saw(st, "evt-dup-2", needs)
	r.saw(fn, "evt-stranger", needs)
	if failures := Check(expected, r.recorded); len(failures) != 0 {
		t.Fatalf("failures = %+v; the owner's own records decide nothing", failures)
	}
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

func without(arrivals []Arrival, delivery string) []Arrival {
	return slices.DeleteFunc(slices.Clone(arrivals), func(arrival Arrival) bool { return arrival.ID == delivery })
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

func retopic(arrivals []Arrival, owner, delivery, topic string) {
	for index, arrival := range arrivals {
		if arrival.Owner == owner && arrival.ID == delivery {
			arrivals[index].Topic = topic
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
