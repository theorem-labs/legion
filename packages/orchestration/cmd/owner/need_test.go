package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// mintedDedupeKeyPattern is Envoy's rule for a dedupe key that names its message, so that a host drops a second copy:
// MINTED_DEDUPE_KEY_PATTERN of packages/contracts, read from the constant that it generates into the Envoy module, so
// that this test cannot drift from it.
func mintedDedupeKeyPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	path := filepath.Join("..", "..", "..", "envoy", "internal", "contracts", "generated.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var pattern string
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "MintedDedupeKeyPattern" || len(spec.Values) != 1 {
			return true
		}
		if literal, ok := spec.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			pattern, err = strconv.Unquote(literal.Value)
		}
		return false
	})
	if err != nil || pattern == "" {
		t.Fatalf("%s holds no string constant MintedDedupeKeyPattern: %v", path, err)
	}
	return regexp.MustCompile(pattern)
}

// A repeated `owner need` with the same draft publishes its notice again, under the same idempotency key, and the
// dedupe key the listener makes of it, publish.<key> (packages/envoy/cmd/listener/api.go), is one that Envoy's hosts
// drop a second copy of: a key of Envoy's minted shape. A key of any other shape is no dedupe key to pi-envoy
// (dedupeKeyNamesItsEvent), so the subscribed owner got the notice twice, under two event ids.
func TestRepeatedNeedPublishesOneKeyThatEnvoyHostsDrop(t *testing.T) {
	f := newCommandFixture(t)
	needFile := f.write("need.md", needDraft)
	f.as(caller).ok("--json", "need", state, needFile)
	f.as(caller).ok("--json", "need", state, needFile)
	posts := f.notices()
	if len(posts) != 2 || posts[0].IdempotencyKey != posts[1].IdempotencyKey {
		t.Fatalf("a repeated need published %+v, want its notice twice under one key", posts)
	}
	if minted, dedupe := mintedDedupeKeyPattern(t), "publish."+posts[0].IdempotencyKey; !minted.MatchString(dedupe) {
		t.Fatalf("the need's dedupe key %q is not one Envoy's hosts drop a repeat of (%s)", dedupe, minted)
	}
}

func TestNeedOutsideTheActiveSetIsRefusedOrRecordedOutsidePart(t *testing.T) {
	f := newCommandFixture(t)
	draft := f.write("need.md", needDraft)
	if code, stderr := f.as(caller).fails("need", callee, draft); code != 1 || !strings.Contains(stderr, "active") {
		t.Fatalf("need outside the active set exited %d: %s", code, stderr)
	}
	f.as(caller).ok("need", callee, draft, "--outside-part")
	var edgeRecord ownerpkg.EdgeRecord
	if err := edgeRecord.Unmarshal(f.show(caller, "skeleton/nodes/"+ownerpkg.Key(caller)+"/edges/E"+ownerpkg.Hex16(callee)+".yaml")); err != nil {
		t.Fatal(err)
	}
	needPath := ownerpkg.NeedRecord("skeleton", caller, callee)
	if edgeRecord.Status != "outside_part" || edgeRecord.Need.Path != needPath || edgeRecord.Need.Hash != ownerpkg.ContentHash(f.show(caller, needPath)) {
		t.Fatalf("outside-part edge = %+v", edgeRecord)
	}
	if _, err := f.store.Item(f.ctx, "r", "need:"+ownerpkg.EdgeID(caller, callee)+"@r1"); !isNotFound(err) {
		t.Fatalf("outside-part need enqueued an item: %v", err)
	}
	if len(f.notices()) != 0 {
		t.Fatalf("outside-part need published %+v", f.notices())
	}
}

// A need answers no item, but once its record and its item are durable a refused notice no longer fails it: the
// command retries, journals the notice in the deployment's state directory, names it and exits 0.
func TestNeedJournalsANoticeTheListenerRefuses(t *testing.T) {
	f := newCommandFixture(t)
	f.refuseNotices()
	need := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	topic := "notifications.example.node.struct:state.needs"

	stdout, stderr, code := f.as(caller).call("--json", "need", state, f.write("need.md", needDraft))
	if code != 0 {
		t.Fatalf("need with a refusing listener exited %d: %s", code, stderr)
	}
	var result struct {
		Commit      string   `json:"commit"`
		Undelivered []string `json:"undelivered"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("need output %q: %v", stdout, err)
	}
	if !reflect.DeepEqual(result.Undelivered, []string{topic}) || result.Commit != f.lastCommit(caller, ownerpkg.NeedRecord("skeleton", caller, state)) {
		t.Fatalf("need result = %+v", result)
	}
	if !strings.Contains(stderr, "notice not delivered: "+topic) {
		t.Fatalf("stderr %q does not name the undelivered notice", stderr)
	}
	if item := f.item(need); item.State != "open" || item.Owner != state {
		t.Fatalf("need item = %+v", item)
	}
	journal, err := os.ReadFile(filepath.Join(f.stateDir(), "undelivered.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct{ Topic, Key string }
	if err := json.Unmarshal(journal, &entry); err != nil || entry.Topic != topic || entry.Key != ownerpkg.NoticeKey(need) {
		t.Fatalf("journal %q: %+v, %v", journal, entry, err)
	}
}

// needWritten reports whether caller's branch holds a need record to target.
func (f *commandFixture) needWritten(target string) bool {
	err := exec.Command("git", "--git-dir="+f.bare, "cat-file", "-e", "owner/"+ownerpkg.Key(caller)+":"+ownerpkg.NeedRecord("skeleton", caller, target)).Run()
	return err == nil
}

// A listener configuration that cannot be read is exit 3, unable to check, before the command writes anything: no
// worktree or branch, no record, no item and no journal line that would pass for a delivery failure.
func TestNeedStopsBeforeWritingWhenTheListenerConfigurationCannotBeRead(t *testing.T) {
	f := newCommandFixture(t)
	delete(f.env, "ENVOY_URL")
	delete(f.env, "ENVOY_TOKEN")
	_, stderr, code := f.as(caller).call("need", state, f.write("need.md", needDraft))
	if code != 3 || !strings.Contains(stderr, "envoy.json") {
		t.Fatalf("need with no listener configuration exited %d: %s", code, stderr)
	}
	if _, err := f.store.Item(f.ctx, "r", "need:"+ownerpkg.EdgeID(caller, state)+"@r1"); !isNotFound(err) {
		t.Fatalf("the need was enqueued: %v", err)
	}
	if f.needWritten(state) {
		t.Fatal("the need record was committed")
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "undelivered.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("a notice was journaled: %v", err)
	}
	if _, err := os.Stat(f.wt(caller)); !os.IsNotExist(err) {
		t.Fatalf("the worktree was created: %v", err)
	}
	if err := exec.Command("git", "--git-dir="+f.bare, "rev-parse", "--verify", "--quiet", "refs/heads/owner/"+ownerpkg.Key(caller)).Run(); err == nil {
		t.Fatal("the owner's branch was created")
	}
}

// A step killed in the middle of its commit leaves git's index lock behind, with no process that holds it. The step's
// next run commits its record: a killed step does not wedge its owner.
func TestNeedRunsAgainAfterAStepKilledInItsCommit(t *testing.T) {
	f := newCommandFixture(t)
	f.as(caller).ok("worktree")
	wt := f.wt(caller)
	lock := f.git(wt, "rev-parse", "--git-path", "index.lock")
	if !filepath.IsAbs(lock) {
		lock = filepath.Join(wt, lock)
	}
	f.writeFile(lock, "")
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	if !f.needWritten(state) {
		t.Fatal("the need record was not committed")
	}
	if item := f.item("need:" + ownerpkg.EdgeID(caller, state) + "@r1"); item.State != "open" || item.Owner != state {
		t.Fatalf("need item = %+v", item)
	}
}

// A reviews path that git cannot list is an input the command cannot read, never "no reviewers".
func TestNeedRefusesAReviewsPathItCannotRead(t *testing.T) {
	f := newCommandFixture(t)
	f.as(caller).ok("worktree")
	wt := f.wt(caller)
	f.writeFile(filepath.Join(wt, "skeleton", "nodes", ownerpkg.Key(caller), "reviews"), "not a directory\n")
	f.git(wt, "add", ".")
	f.git(wt, "commit", "-q", "-m", "a file where the reviews directory belongs")
	code, stderr := f.as(caller).fails("need", state, f.write("need.md", needDraft))
	if code != 3 || !strings.Contains(stderr, "reviews") {
		t.Fatalf("need with an unreadable reviews path exited %d: %s", code, stderr)
	}
	if _, err := f.store.Item(f.ctx, "r", "need:"+ownerpkg.EdgeID(caller, state)+"@r1"); !isNotFound(err) {
		t.Fatalf("the need was enqueued: %v", err)
	}
}

// The command sets a need's source, target and origin: the source is the edge's source owner, and a need always
// comes from a neighbor. A draft that sets them, or the retired ids, is refused.
func TestNeedDraftCannotSetTheFieldsTheCommandSets(t *testing.T) {
	f := newCommandFixture(t)
	for _, field := range []string{"source: review", "origin: review", "node: struct:state", "retired: [C9]"} {
		draft := strings.Replace(needDraft, "cites: []\n", "cites: []\n"+field+"\n", 1)
		if code, stderr := f.as(caller).fails("need", state, f.write("need.md", draft)); code != 1 || !strings.Contains(stderr, "author_model, cites, resolves") {
			t.Errorf("need draft with %q exited %d: %s", field, code, stderr)
		}
	}
}

// A claim id removed from a need is retired for good: the next revision records it, and a later revision that
// uses it again is refused.
func TestNeedRevisionsRetireRemovedClaimIDs(t *testing.T) {
	f := newCommandFixture(t)
	f.as(caller).ok("need", state, f.write("need-1.md", needDraftR2))                                                                            // C1, C2
	f.as(caller).ok("need", state, f.write("need-2.md", needDraft))                                                                              // C1
	if code, stderr := f.as(caller).fails("need", state, f.write("need-3.md", needDraftR2)); code != 1 || !strings.Contains(stderr, "retired") { // C2 again
		t.Fatalf("need reusing a retired claim id exited %d: %s", code, stderr)
	}
	var front ownerpkg.NeedFront
	if err := front.Unmarshal(frontMatter(t, f.show(caller, ownerpkg.NeedRecord("skeleton", caller, state)))); err != nil {
		t.Fatal(err)
	}
	if front.Revision != 2 || !reflect.DeepEqual(front.Retired, []string{"C2"}) {
		t.Fatalf("need front = %+v, want r2 with C2 retired", front)
	}
}
