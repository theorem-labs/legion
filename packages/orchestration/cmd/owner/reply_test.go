package main

import (
	"reflect"
	"strings"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestProgramDefectReplyEnqueuesOneEscalation(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	offer := "offer:" + edge + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	f.as(state).ok("offer", f.write("offer.md", offerDraft), "--answers", "need:"+edge+"@r1")

	if code, stderr := f.as(caller).fails("reply", offer, "program-defect", f.write("bare.md", "The writer can skip the store.\n")); code != 1 || !strings.Contains(stderr, "counterexample") {
		t.Fatalf("program-defect without its evidence exited %d: %s", code, stderr)
	}
	reply := f.write("defect.md", "---\ncounterexample: state.value is read while second writes it without the lock\nbugs_row: '| caller | torn read of state.value |'\n---\nThe C code lets caller read a torn value.\n")
	f.as(caller).ok("--json", "reply", offer, "program-defect", reply)
	f.as(caller).ok("--json", "reply", offer, "program-defect", reply)

	escalations, err := f.store.OpenItems(f.ctx, "r", "escalations")
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 1 || !strings.HasPrefix(escalations[0].ID, "escalation:") {
		t.Fatalf("escalations queue = %+v", escalations)
	}
	if item := f.item(offer); item.State != "done" {
		t.Fatalf("replied offer = %+v", item)
	}
	replyPath := ownerpkg.ReplyRecord("skeleton", caller, edge, 1)
	replyBytes := f.show(caller, replyPath)
	replyID := "reply:" + edge + "@" + ownerpkg.Hex16(string(replyBytes))[:12]
	if escalations[0].ID != "escalation:"+ownerpkg.Hex16(replyID) {
		t.Fatalf("escalation id = %q, want one for %q", escalations[0].ID, replyID)
	}
	var escalation ownerpkg.Escalation
	if err := escalation.Unmarshal(f.show(caller, "skeleton/nodes/"+ownerpkg.Key(caller)+"/escalations/"+ownerpkg.Hex16(replyID)+".yaml")); err != nil {
		t.Fatal(err)
	}
	if escalation.Subject != replyID || escalation.Record != replyPath || escalation.Reason != "program-defect" || escalation.BugsRow == "" {
		t.Fatalf("escalation record = %+v", escalation)
	}
	if _, err := f.store.Item(f.ctx, "r", replyID); !isNotFound(err) {
		t.Fatalf("a program-defect reply also enqueued %s for an owner: %v", replyID, err)
	}
}

func TestMissingPremiseReplyGoesToThePremiseOwner(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need := "need:" + edge + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	if code, _ := f.as(state).fails("reply", need, "missing-premise", f.write("premise.md", "The invariant needs the cluster lock rule.\n")); code != 2 {
		t.Fatalf("missing-premise without --premise-owner exited %d, want 2", code)
	}
	f.as(state).ok("reply", need, "missing-premise", f.write("premise.md", "The invariant needs the cluster lock rule.\n"), "--premise-owner", "subsystem:cluster")

	replyBytes := f.show(state, ownerpkg.ReplyRecord("skeleton", state, edge, 1))
	replyID := "reply:" + edge + "@" + ownerpkg.Hex16(string(replyBytes))[:12]
	if item := f.item(replyID); item.State != "open" || item.Owner != "subsystem:cluster" {
		t.Fatalf("reply item = %+v", item)
	}
	posts := f.notices()
	last := posts[len(posts)-1]
	if last.Topic != "notifications.lxspec.node.struct:state.users" || decodePayload(t, last).Item != replyID {
		t.Fatalf("reply notice = %+v", last)
	}
}

// replyItem is the reply item of the reply record that replier wrote about the n-th revision it answered.
func (f *commandFixture) replyItem(replier, edge string, n int) string {
	f.t.Helper()
	return "reply:" + edge + "@" + ownerpkg.Hex16(string(f.show(replier, ownerpkg.ReplyRecord("skeleton", replier, edge, n))))[:12]
}

// The source replies to an offer, so the target answers the reply with a revised spec: the revision lists the
// reply in resolves, its commit carries the reply's Owner-Item trailer, the reply completes with that commit, and
// the source gets the revised offer to accept. The target's queue is then empty.
func TestOfferAnswersAReplyAndQueuesTheRevisedOfferForTheSource(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	f.as(state).ok("offer", f.write("offer.md", offerDraft), "--answers", "need:"+edge+"@r1")
	offer1 := "offer:" + edge + "@r1"
	f.as(caller).ok("reply", offer1, "proved-incompatible", f.write("incompatible.md", "The offer does not order the writer's store before the read.\n"))
	reply := f.replyItem(caller, edge, 1)
	if item := f.item(reply); item.State != "open" || item.Owner != state {
		t.Fatalf("reply item = %+v", item)
	}

	// The revision edits C1 in place: the claim keeps its id.
	revised := f.write("offer-2.md", "---\nauthor_model: anthropic/claude-opus\ncomplete: false\nresolves: []\n---\n1. [C1] A reader of state.value sees the last value that a writer stored before the read began. by: code include/state.h:1\n")
	f.as(state).ok("--json", "offer", revised, "--answers", reply)
	f.as(state).ok("--json", "offer", revised, "--answers", reply) // a second run repeats nothing

	specPath := ownerpkg.SpecRecord("skeleton", state)
	specFront, _, err := ownerpkg.ParseSpec(f.show(state, specPath))
	if err != nil {
		t.Fatal(err)
	}
	needHash := ownerpkg.ContentHash(f.show(caller, ownerpkg.NeedRecord("skeleton", caller, state)))
	if specFront.Revision != 2 || !reflect.DeepEqual(specFront.Resolves, []string{reply}) || !reflect.DeepEqual(specFront.Answers, []string{"need:" + edge + "@" + needHash}) {
		t.Fatalf("revised spec front = %+v", specFront)
	}
	specCommit := f.lastCommit(state, specPath)
	if trailer := f.trailer(specCommit, "Owner-Item"); trailer != reply {
		t.Fatalf("revised spec trailer = %q, want %q", trailer, reply)
	}
	if item := f.item(reply); item.State != "done" || item.Result != specCommit {
		t.Fatalf("answered reply = %+v, want done with %s", item, specCommit)
	}
	if count := f.git(f.root, "--git-dir="+f.bare, "rev-list", "--count", "owner/"+ownerpkg.Key(state), "--", specPath); count != "2" {
		t.Fatalf("spec commits = %s, want 2", count)
	}
	// The offer item's version is the spec revision that makes it.
	offer2 := "offer:" + edge + "@r2"
	if item := f.item(offer2); item.State != "open" || item.Owner != caller {
		t.Fatalf("revised offer item = %+v", item)
	}
	if open, err := f.store.OpenItems(f.ctx, "r", state); err != nil || len(open) != 0 {
		t.Fatalf("the target's queue after it answered the reply = %+v, %v", open, err)
	}
	last := f.notices()[len(f.notices())-1]
	if payload := decodePayload(t, last); last.Topic != "notifications.lxspec.node.struct:state.users" || payload.Item != offer2 || payload.Commit != specCommit {
		t.Fatalf("revised offer notice = %+v %+v", last, payload)
	}

	f.as(caller).ok("accept", offer2)
	var accept ownerpkg.Reply
	if err := accept.Unmarshal(f.show(caller, ownerpkg.ReplyRecord("skeleton", caller, edge, 2))); err != nil || accept.Kind != "accept" {
		t.Fatalf("accept of the revised offer = %+v, %v", accept, err)
	}
	if item := f.item(offer2); item.State != "done" {
		t.Fatalf("accepted revised offer = %+v", item)
	}
}

// The target replies to a need, so the source answers the reply with a revised need: the need lists the reply in
// resolves, its commit carries the reply's trailer, the reply completes with that commit, and the target gets the
// new need revision. An offer cannot answer a reply about the source's own need.
func TestNeedAnswersAReplyWithARevisedNeed(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	f.as(state).ok("reply", "need:"+edge+"@r1", "proved-incompatible", f.write("incompatible.md", "No invariant of state orders a store before a load.\n"))
	reply := f.replyItem(state, edge, 1)
	if item := f.item(reply); item.State != "open" || item.Owner != caller {
		t.Fatalf("reply item = %+v", item)
	}
	if code, stderr := f.as(caller).fails("offer", f.write("caller-spec.md", offerDraft), "--answers", reply); code != 1 || !strings.Contains(stderr, "owner need") {
		t.Fatalf("offer answering a reply about the source's need exited %d: %s", code, stderr)
	}

	revised := f.write("need-2.md", "---\nauthor_model: openai-codex/gpt-5.6-terra\ncites: []\n---\n1. [C1] caller needs the state value to stay stable while it holds the state lock. by: code kernel/caller.c:2\n")
	f.as(caller).ok("--json", "need", state, revised, "--answers", reply)
	f.as(caller).ok("--json", "need", state, revised, "--answers", reply) // a second run repeats nothing

	needPath := ownerpkg.NeedRecord("skeleton", caller, state)
	var front ownerpkg.NeedFront
	if err := front.Unmarshal(frontMatter(t, f.show(caller, needPath))); err != nil {
		t.Fatal(err)
	}
	if front.Revision != 2 || !reflect.DeepEqual(front.Resolves, []string{reply}) || front.Source != caller || front.Origin != "neighbor" {
		t.Fatalf("revised need front = %+v", front)
	}
	needCommit := f.lastCommit(caller, needPath)
	if trailer := f.trailer(needCommit, "Owner-Item"); trailer != reply {
		t.Fatalf("revised need trailer = %q, want %q", trailer, reply)
	}
	if item := f.item(reply); item.State != "done" || item.Result != needCommit {
		t.Fatalf("answered reply = %+v, want done with %s", item, needCommit)
	}
	if item := f.item("need:" + edge + "@r2"); item.State != "open" || item.Owner != state {
		t.Fatalf("revised need item = %+v", item)
	}
	if open, err := f.store.OpenItems(f.ctx, "r", caller); err != nil || len(open) != 0 {
		t.Fatalf("the source's queue after it answered the reply = %+v, %v", open, err)
	}
	if count := f.git(f.root, "--git-dir="+f.bare, "rev-list", "--count", "owner/"+ownerpkg.Key(caller), "--", needPath); count != "2" {
		t.Fatalf("need commits = %s, want 2", count)
	}
}
