package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestNeedOfferAcceptRoundTripUsesPostgresAndListener(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	need := "need:" + edge + "@r1"
	offer := "offer:" + edge + "@r1"

	f.as(caller).ok("--json", "worktree")
	needFile := f.write("need.md", needDraft)
	f.as(caller).ok("--json", "need", state, needFile)
	f.as(caller).ok("--json", "need", state, needFile) // a second run repeats nothing

	needItem := f.item(need)
	if needItem.State != "open" || needItem.Owner != state {
		t.Fatalf("need item = %+v", needItem)
	}
	if _, err := f.store.Item(f.ctx, "r", "need:"+edge+"@r2"); !isNotFound(err) {
		t.Fatalf("a repeated need made revision 2: %v", err)
	}
	needPath := ownerpkg.NeedRecord("skeleton", caller, state)
	needBytes := f.show(caller, needPath)
	var needFront ownerpkg.NeedFront
	if err := needFront.Unmarshal(frontMatter(t, needBytes)); err != nil {
		t.Fatalf("committed need: %v\n%s", err, needBytes)
	}
	if needFront.Source != caller || needFront.Target != state || needFront.Origin != "neighbor" || needFront.Revision != 1 || needFront.Epoch != 1 {
		t.Fatalf("need front = %+v", needFront)
	}
	needCommit := f.lastCommit(caller, needPath)

	f.as(state).ok("--json", "worktree")
	f.as(state).ok("--json", "offer", f.write("offer.md", offerDraft), "--answers", need)

	if item := f.item(need); item.State != "done" || item.Result == "" {
		t.Fatalf("answered need = %+v", item)
	}
	offerItem := f.item(offer)
	if offerItem.State != "open" || offerItem.Owner != caller {
		t.Fatalf("offer item = %+v", offerItem)
	}
	specPath := ownerpkg.SpecRecord("skeleton", state)
	specBytes := f.show(state, specPath)
	specFront, _, err := ownerpkg.ParseSpec(specBytes)
	if err != nil {
		t.Fatal(err)
	}
	needHash := ownerpkg.ContentHash(needBytes)
	if specFront.Owner != state || specFront.Revision != 1 || !reflect.DeepEqual(specFront.Answers, []string{"need:" + edge + "@" + needHash}) {
		t.Fatalf("spec front = %+v", specFront)
	}
	specCommit := f.lastCommit(state, specPath)
	if trailer := f.trailer(specCommit, "Owner-Item"); trailer != need {
		t.Fatalf("offer commit trailer = %q, want %q", trailer, need)
	}

	f.as(caller).ok("--json", "accept", offer)
	if item := f.item(offer); item.State != "done" {
		t.Fatalf("accepted offer = %+v", item)
	}
	replyPath := ownerpkg.ReplyRecord("skeleton", caller, edge, 1)
	var accept ownerpkg.Reply
	if err := accept.Unmarshal(f.show(caller, replyPath)); err != nil {
		t.Fatal(err)
	}
	specHash := ownerpkg.ContentHash(specBytes)
	if accept.Kind != "accept" || accept.Answers["need"] != needHash || accept.Answers["offer"] != specHash {
		t.Fatalf("accept record = %+v", accept)
	}
	var edgeRecord ownerpkg.EdgeRecord
	if err := edgeRecord.Unmarshal(f.show(caller, "skeleton/nodes/"+ownerpkg.Key(caller)+"/edges/E"+ownerpkg.Hex16(state)+".yaml")); err != nil {
		t.Fatal(err)
	}
	if edgeRecord.Status != "accepted" || edgeRecord.Need.Hash != needHash || edgeRecord.Offer.SpecHash != specHash {
		t.Fatalf("edge record = %+v", edgeRecord)
	}

	posts := f.notices()
	if len(posts) != 4 {
		t.Fatalf("listener got %d posts, want need twice, offer and accept: %+v", len(posts), posts)
	}
	if posts[0].IdempotencyKey != posts[1].IdempotencyKey || posts[0].IdempotencyKey != "c-"+ownerpkg.Hex16(need) {
		t.Fatalf("repeated need keys = %q and %q", posts[0].IdempotencyKey, posts[1].IdempotencyKey)
	}
	wantTopics := []string{
		"notifications.example.node.struct:state.needs",
		"notifications.example.node.struct:state.needs",
		"notifications.example.node.struct:state.users",
		"notifications.example.node.struct:state.needs",
	}
	for index, post := range posts {
		if post.Topic != wantTopics[index] {
			t.Errorf("post %d topic = %q, want %q", index, post.Topic, wantTopics[index])
		}
	}
	needNotice := decodePayload(t, posts[0])
	if needNotice.Type != "need" || needNotice.From != caller || needNotice.To != state || needNotice.Item != need ||
		needNotice.Record != needPath || needNotice.Commit != needCommit || needNotice.Epoch != 1 ||
		needNotice.Versions["need"] != needHash || needNotice.Models["author"] != "openai-codex/gpt-5.6-terra" {
		t.Fatalf("need notice = %+v", needNotice)
	}
	offerNotice := decodePayload(t, posts[2])
	if offerNotice.Type != "offer" || offerNotice.From != state || offerNotice.To != caller || offerNotice.Item != offer ||
		offerNotice.Commit != specCommit || offerNotice.Versions["need"] != needHash || offerNotice.Versions["offer"] != specHash ||
		posts[2].IdempotencyKey != "c-"+ownerpkg.Hex16(offer) {
		t.Fatalf("offer notice = %+v key %q", offerNotice, posts[2].IdempotencyKey)
	}
	acceptNotice := decodePayload(t, posts[3])
	if acceptNotice.Type != "accept" || acceptNotice.From != caller || acceptNotice.To != state || !strings.HasPrefix(acceptNotice.Item, "reply:"+edge+"@") ||
		acceptNotice.Record != replyPath || acceptNotice.Versions["offer"] != specHash {
		t.Fatalf("accept notice = %+v", acceptNotice)
	}
}

// One spec revision that answers two needs and the owner's open start item is the record of all three, so its
// commit carries an Owner-Item trailer for each, and each item completes with that commit.
func TestOfferAnsweringSeveralItemsCommitsATrailerForEach(t *testing.T) {
	f := newCommandFixture(t)
	fromCaller := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	fromSecond := "need:" + ownerpkg.EdgeID(second, state) + "@r1"
	start := "start:" + state + "@1"
	f.as(caller).ok("need", state, f.write("need-caller.md", needDraft))
	f.as(second).ok("need", state, f.write("need-second.md", needDraft))
	f.enqueue(start, state)

	offerFile := f.write("offer.md", offerDraft)
	f.as(state).ok("--json", "offer", offerFile, "--answers", fromCaller, fromSecond)
	f.as(state).ok("--json", "offer", offerFile, "--answers", fromCaller, fromSecond) // a second run repeats nothing

	specPath := ownerpkg.SpecRecord("skeleton", state)
	specCommit := f.lastCommit(state, specPath)
	if got, want := strings.Fields(f.trailer(specCommit, "Owner-Item")), []string{fromCaller, fromSecond, start}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spec commit Owner-Item trailers = %q, want %q", got, want)
	}
	for _, answered := range []string{fromCaller, fromSecond, start} {
		if item := f.item(answered); item.State != "done" || item.Result != specCommit {
			t.Errorf("%s = %+v, want done with result %s", answered, item, specCommit)
		}
	}
	for source, offer := range map[string]string{caller: "offer:" + ownerpkg.EdgeID(caller, state) + "@r1", second: "offer:" + ownerpkg.EdgeID(second, state) + "@r1"} {
		if item := f.item(offer); item.State != "open" || item.Owner != source {
			t.Errorf("%s = %+v, want open for %s", offer, item, source)
		}
	}
	if count := f.git(f.root, "--git-dir="+f.bare, "rev-list", "--count", "owner/"+ownerpkg.Key(state), "--", specPath); count != "1" {
		t.Fatalf("spec commits = %s, want 1", count)
	}
}

// When every --answers item is done, the command repeats the revision that answered them, with that revision's
// number and commit. A changed draft is a new revision, which answers nothing that is done, so the command refuses
// it and writes nothing.
func TestOfferWithEveryAnswerDoneRepeatsItsRevisionOrRefusesAChangedDraft(t *testing.T) {
	f := newCommandFixture(t)
	need := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	offerFile := f.write("offer.md", offerDraft)
	f.as(state).ok("offer", offerFile, "--answers", need)
	specPath := ownerpkg.SpecRecord("skeleton", state)
	specCommit := f.lastCommit(state, specPath)

	var again stepResult
	if err := json.Unmarshal([]byte(f.as(state).ok("--json", "offer", offerFile, "--answers", need)), &again); err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyDone || again.Revision != 1 || again.Commit != specCommit {
		t.Fatalf("the repeated offer = %+v, want already done at r1 with %s", again, specCommit)
	}

	changed := f.write("offer-2.md", strings.Replace(offerDraft, "the last value that a writer stored", "the value that the last writer stored", 1))
	if code, stderr := f.as(state).fails("offer", changed, "--answers", need); code != 1 || !strings.Contains(stderr, need) {
		t.Fatalf("a changed draft answering only done items exited %d: %s", code, stderr)
	}
	if got := f.lastCommit(state, specPath); got != specCommit {
		t.Fatalf("the refused draft moved the spec to %s", got)
	}
	if posts := f.notices(); len(posts) != 2 {
		t.Fatalf("listener got %d posts, want the need's and the first offer's only: %+v", len(posts), posts)
	}
}

// A spec revision that offers no edge notifies the users of the owner's spec. It queues no item for them, so the
// notice names none; it carries its own key for each revision, which a re-run repeats, and its message names the
// revision.
func TestSpecRevisionWithNoEdgeNamesNoItem(t *testing.T) {
	f := newCommandFixture(t)
	f.as(state).ok("offer", f.write("spec-1.md", offerDraft))
	revised := f.write("spec-2.md", strings.Replace(offerDraft, "the last value that a writer stored", "the value that the last writer stored", 1))
	f.as(state).ok("offer", revised)
	f.as(state).ok("offer", revised) // a second run repeats the notice under the same key

	posts := f.notices()
	if len(posts) != 3 {
		t.Fatalf("listener got %d posts, want r1, r2 and r2 again: %+v", len(posts), posts)
	}
	specPath := ownerpkg.SpecRecord("skeleton", state)
	for index, post := range posts {
		var payload map[string]any
		if err := json.Unmarshal([]byte(post.Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if item, ok := payload["item"]; ok {
			t.Errorf("post %d names the item %v", index, item)
		}
		if post.Topic != "notifications.example.node.struct:state.users" || payload["record"] != specPath || payload["type"] != "offer" {
			t.Errorf("post %d = %+v", index, post)
		}
		if want := map[int]string{0: " r1 ", 1: " r2 ", 2: " r2 "}[index]; !strings.Contains(post.Message, state+want) {
			t.Errorf("post %d message %q does not name %s%s", index, post.Message, state, want)
		}
	}
	if posts[0].IdempotencyKey == posts[1].IdempotencyKey || posts[1].IdempotencyKey != posts[2].IdempotencyKey || !strings.HasPrefix(posts[0].IdempotencyKey, "c-") {
		t.Fatalf("keys = %q, %q, %q; want one key for each revision, repeated by the re-run", posts[0].IdempotencyKey, posts[1].IdempotencyKey, posts[2].IdempotencyKey)
	}
}

// A claim id removed from a spec is retired for good: the command records it, the draft cannot, and a later
// revision that uses it again is refused. A claim edited under its id keeps that id.
func TestSpecRevisionsRetireRemovedClaimIDs(t *testing.T) {
	f := newCommandFixture(t)
	twoClaims := "---\nauthor_model: anthropic/claude-opus\ncomplete: false\nresolves: []\n---\n1. [C1] A reader of state.value sees the last value that a writer stored. by: code include/state.h:1\n    1. [C2] A writer holds the state lock. by: code include/state.h:2\n"
	f.as(state).ok("offer", f.write("spec-1.md", twoClaims))
	if code, stderr := f.as(state).fails("offer", f.write("spec-retired.md", strings.Replace(offerDraft, "resolves: []\n", "resolves: []\nretired: [C2]\n", 1))); code != 1 || !strings.Contains(stderr, "retired") {
		t.Fatalf("a spec draft that sets retired exited %d: %s", code, stderr)
	}
	f.as(state).ok("offer", f.write("spec-2.md", offerDraft)) // C2 removed
	edited := strings.Replace(offerDraft, "the last value that a writer stored", "the value that the last writer stored", 1)
	f.as(state).ok("offer", f.write("spec-3.md", edited)) // C1 edited in place
	if code, stderr := f.as(state).fails("offer", f.write("spec-4.md", twoClaims)); code != 1 || !strings.Contains(stderr, "retired") {
		t.Fatalf("a spec reusing a retired claim id exited %d: %s", code, stderr)
	}
	front, claims, err := ownerpkg.ParseSpec(f.show(state, ownerpkg.SpecRecord("skeleton", state)))
	if err != nil {
		t.Fatal(err)
	}
	if front.Revision != 3 || !reflect.DeepEqual(front.Retired, []string{"C2"}) || claims[0].Text != "A reader of state.value sees the value that the last writer stored." {
		t.Fatalf("spec r%d retired %v claims %+v", front.Revision, front.Retired, claims)
	}
}
