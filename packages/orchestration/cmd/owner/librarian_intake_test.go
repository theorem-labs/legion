package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// The gate's own envelopes (owner/testdata/librarian, written by conceptgate.Envelope at legion 1e163669) for a proposer
// whose id holds an @.
const (
	intakeProposer  = "function:alloc_frame@kernel/frame.c"
	intakeNearID    = "near-35f57eb44edf2d11"
	intakeRecheckID = "recheck-6b1f0c2d9e8a7f3b4c5d6e7f8a9b0c1d-name.46782e5370416c69676e6564"
	conceptsTopic   = "notifications.example.concepts"
)

type intakeOutput struct {
	Item        string   `json:"item"`
	Kind        string   `json:"kind"`
	Added       bool     `json:"added"`
	Record      string   `json:"record"`
	Commit      string   `json:"commit"`
	Undelivered []string `json:"undelivered"`
	Journal     string   `json:"journal"`
}

// envelope copies the gate's envelope file name to the drafts directory, with each replacement applied, and returns its
// path: the gate gives the command a file it removes when the command exits.
func (f *commandFixture) envelope(name string, replacements ...string) string {
	f.t.Helper()
	text := string(f.read(filepath.Join("..", "..", "owner", "testdata", "librarian", name)))
	changed := strings.NewReplacer(replacements...).Replace(text)
	if len(replacements) > 0 && changed == text {
		f.t.Fatalf("the replacements %q change nothing in %s", replacements, name)
	}
	return f.write(name, changed)
}

func (f *commandFixture) dsnFile() string { return f.env["LEGION_LEDGER_DSN_FILE"] }

// intakeArgs are the gate's call of the configured command: the configured words, then --id, --proposer and --proposal.
func (f *commandFixture) intakeArgs(id, proposal string, configured ...string) []string {
	args := append([]string{"--json", "librarian-intake", "--ledger-dsn-file", f.dsnFile(), "--repository", "r"}, configured...)
	return append(args, "--id", id, "--proposer", intakeProposer, "--proposal", proposal)
}

// intake runs owner with env alone: no LEGION_OWNER, as the gate runs it. cwd is the fixture root. The user cache
// directory (os.UserCacheDir, the journal without a deployment file) is the fixture's.
func (f *commandFixture) intake(env map[string]string, args ...string) (intakeOutput, string, int) {
	f.t.Helper()
	f.t.Setenv("XDG_CACHE_HOME", filepath.Join(f.root, "cache"))
	var stdout, stderr bytes.Buffer
	code := run(f.ctx, args, func(key string) string { return env[key] }, f.root, &stdout, &stderr)
	var out intakeOutput
	if code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			f.t.Fatalf("intake output %q: %v", stdout.String(), err)
		}
	}
	return out, stderr.String(), code
}

// gateEnv is the gate's environment on a host with a listener: HOME, and the listener of the fixture.
func (f *commandFixture) gateEnv() map[string]string {
	return map[string]string{"HOME": f.root, "ENVOY_URL": f.env["ENVOY_URL"], "ENVOY_TOKEN": f.env["ENVOY_TOKEN"]}
}

// A delivery that fails after a listener refused its notice prints the notice, so that a journal that could not keep
// it does not lose it; the failure is exit 3, unable to check.
func TestAFailedDeliveryPrintsItsUndeliveredNotices(t *testing.T) {
	var stderr bytes.Buffer
	notice := ownerpkg.Notice{Topic: conceptsTopic, Message: "concept m", Key: "c-0123456789abcdef", Payload: ownerpkg.Payload{"item": "i"}}
	_, err := deliveryResult(&stderr, ownerpkg.IntakeOutcome{Item: "i", Undelivered: []ownerpkg.Notice{notice}},
		errors.New("write notice journal: no space left on device"), "/state/undelivered.jsonl")
	if err == nil || !strings.Contains(stderr.String(), "notice not delivered: "+conceptsTopic) || !strings.Contains(stderr.String(), "c-0123456789abcdef") {
		t.Fatalf("error %v, stderr %q", err, stderr.String())
	}
	if code := writeError(io.Discard, false, err); code != exitUnable {
		t.Fatalf("a failed journal write exits %d, want %d", code, exitUnable)
	}
}

func (f *commandFixture) itemEvents(id string) []string {
	f.t.Helper()
	events, err := f.store.Events(f.ctx, "r", 0, id)
	if err != nil {
		f.t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func (f *commandFixture) noItem(id string) {
	f.t.Helper()
	var notFound *ledger.NotFoundError
	if _, err := f.store.Item(f.ctx, "r", id); !errors.As(err, &notFound) {
		f.t.Fatalf("item %s exists (%v)", id, err)
	}
}

// journalLines are the notices a journal holds; a journal the command created and never wrote holds none.
func journalLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestLibrarianIntakeQueuesANearMatchOnceAndPublishesItUnderOneKey(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeNearID)
	args := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--topic-prefix", "notifications.example.")

	out, stderr, code := f.intake(f.gateEnv(), args...)
	if code != 0 {
		t.Fatalf("intake exited %d: %s", code, stderr)
	}
	if out.Item != item || out.Kind != "near" || !out.Added || len(out.Undelivered) != 0 {
		t.Fatalf("intake output = %+v", out)
	}
	if got := f.item(item); got.Owner != "librarian" || got.State != "open" || got.Holder != "" {
		t.Fatalf("item = %+v", got)
	}
	if events := f.itemEvents(item); len(events) != 1 || events[0] != "item.enqueued" {
		t.Fatalf("item events = %v, want one item.enqueued", events)
	}
	notices := f.notices()
	if len(notices) != 1 || notices[0].Topic != conceptsTopic || notices[0].IdempotencyKey != ownerpkg.NoticeKey(item) {
		t.Fatalf("notices = %+v", notices)
	}
	payload := decodePayload(t, notices[0])
	if payload.Type != "concept" || payload.Item != item || payload.From != intakeProposer || payload.To != "librarian" ||
		payload.Record != "" || payload.Commit != "" {
		t.Fatalf("payload = %+v", payload)
	}
	if notices[0].Message != "concept "+intakeProposer+" "+intakeNearID+" (epoch 0)" {
		t.Fatalf("message = %q", notices[0].Message)
	}

	// The gate runs again on the same proposal: the same id, a success that adds nothing. The item is still open, so
	// the notice goes again under the same key (the first delivery may have lost it; the listener drops a copy).
	again, stderr, code := f.intake(f.gateEnv(), args...)
	if code != 0 || again.Added || again.Item != item {
		t.Fatalf("second delivery exited %d with %+v: %s", code, again, stderr)
	}
	// The same id with another envelope (the gate's verdict changed with the registry): still the first item.
	changed := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt", `"how":"conjunct"`, `"how":"conjunct-of-a-changed-registry"`),
		"--topic-prefix", "notifications.example.")
	if out, stderr, code := f.intake(f.gateEnv(), changed...); code != 0 || out.Added {
		t.Fatalf("changed envelope exited %d with %+v: %s", code, out, stderr)
	}
	if events := f.itemEvents(item); len(events) != 1 {
		t.Fatalf("item events after repeats = %v", events)
	}
	repeats := f.notices()
	if len(repeats) != 3 {
		t.Fatalf("three deliveries of an open item published %d notices, want 3", len(repeats))
	}
	for _, notice := range repeats[1:] {
		if notice.IdempotencyKey != notices[0].IdempotencyKey || notice.Topic != notices[0].Topic ||
			notice.Message != notices[0].Message || notice.Payload != notices[0].Payload {
			t.Fatalf("a repeat notice %+v differs from the first %+v", notice, notices[0])
		}
	}
}

// A delivery after the librarian finished the item is the same success: the ledger's ReasonExists for a done item.
func TestLibrarianIntakeAfterTheItemIsDoneAddsNothing(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeNearID)
	args := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--topic-prefix", "notifications.example.")
	if _, stderr, code := f.intake(f.gateEnv(), args...); code != 0 {
		t.Fatalf("intake exited %d: %s", code, stderr)
	}
	lease, err := f.store.Claim(f.ctx, "r", item, "librarian", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(f.ctx, "r", item, "librarian", lease.Generation, "use:Fx.SpAligned"); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := f.intake(f.gateEnv(), args...)
	if code != 0 || out.Added {
		t.Fatalf("delivery after done exited %d with %+v: %s", code, out, stderr)
	}
	if got := f.item(item); got.State != "done" || got.Result != "use:Fx.SpAligned" {
		t.Fatalf("item = %+v", got)
	}
	if events := f.itemEvents(item); strings.Join(events, ",") != "item.enqueued,item.claimed,item.completed" {
		t.Fatalf("item events = %v", events)
	}
	if notices := f.notices(); len(notices) != 1 {
		t.Fatalf("%d notices, want the first delivery's only", len(notices))
	}
}

// A recheck has no proposer copy: the intake keeps its envelope on inbox/librarian before it queues the item, and the
// librarian's inbox lists the item as ready with that record. The first envelope stays.
func TestLibrarianIntakeRecordsARecheckAndTheInboxListsItReady(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeRecheckID)
	record := ownerpkg.LibrarianInboxRecord(intakeRecheckID)
	file := f.envelope("recheck-envelope.txt")
	envelope, err := ownerpkg.ParseGateEnvelope(f.read(file), intakeRecheckID, intakeProposer)
	if err != nil {
		t.Fatal(err)
	}

	out, stderr, code := f.intake(f.gateEnv(), f.intakeArgs(intakeRecheckID, file, "--git-dir", f.bare)...)
	if code != 0 {
		t.Fatalf("intake exited %d: %s", code, stderr)
	}
	tip := f.git(f.root, "--git-dir="+f.bare, "rev-parse", "refs/heads/inbox/librarian")
	if out.Item != item || out.Kind != "recheck" || !out.Added || out.Record != record || out.Commit != tip {
		t.Fatalf("intake output = %+v, tip %s", out, tip)
	}
	if got := f.git(f.root, "--git-dir="+f.bare, "show", "inbox/librarian:"+record) + "\n"; got != string(envelope.InboxRecord()) {
		t.Fatalf("inbox record =\n%s\nwant\n%s", got, envelope.InboxRecord())
	}
	if got := f.item(item); got.Owner != "librarian" || got.State != "open" {
		t.Fatalf("item = %+v", got)
	}
	notices := f.notices()
	if len(notices) != 1 || notices[0].Topic != conceptsTopic {
		t.Fatalf("notices = %+v", notices)
	}
	if payload := decodePayload(t, notices[0]); payload.Record != record || payload.Commit != tip || payload.Item != item {
		t.Fatalf("payload = %+v", payload)
	}

	var inbox []struct {
		ID     string `json:"id"`
		Record string `json:"record"`
		Branch string `json:"branch"`
		Commit string `json:"commit"`
		Ready  bool   `json:"ready"`
	}
	if err := json.Unmarshal([]byte(f.as("librarian").ok("--json", "inbox")), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != item || inbox[0].Record != record || inbox[0].Branch != "inbox/librarian" ||
		inbox[0].Commit != tip || !inbox[0].Ready {
		t.Fatalf("librarian inbox = %+v", inbox)
	}

	changed := f.envelope("recheck-envelope.txt", `"how":"conjunct"`, `"how":"conjunct-of-a-later-landing"`)
	if out, stderr, code := f.intake(f.gateEnv(), f.intakeArgs(intakeRecheckID, changed, "--git-dir", f.bare)...); code != 0 || out.Added {
		t.Fatalf("second delivery exited %d with %+v: %s", code, out, stderr)
	}
	if again := f.git(f.root, "--git-dir="+f.bare, "rev-parse", "refs/heads/inbox/librarian"); again != tip {
		t.Fatalf("inbox/librarian moved from %s to %s", tip, again)
	}
	if got := f.git(f.root, "--git-dir="+f.bare, "show", "inbox/librarian:"+record) + "\n"; got != string(envelope.InboxRecord()) {
		t.Fatalf("the second envelope rewrote the record:\n%s", got)
	}
	// The open item's notice goes again, under the first key and with the first record's commit.
	repeats := f.notices()
	if len(f.itemEvents(item)) != 1 || len(repeats) != 2 || repeats[1].IdempotencyKey != repeats[0].IdempotencyKey ||
		repeats[1].Payload != repeats[0].Payload {
		t.Fatalf("second delivery: events %v, notices %+v", f.itemEvents(item), repeats)
	}
}

// A near item is ready once the proposer's branch holds the gate's verdict record beside its proposal.
func TestLibrarianInboxListsANearItemReadyOnceTheVerdictRecordIsOnTheProposersBranch(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeNearID)
	if _, stderr, code := f.intake(f.gateEnv(), f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--git-dir", f.bare)...); code != 0 {
		t.Fatalf("intake exited %d: %s", code, stderr)
	}
	type entry struct {
		ID          string `json:"id"`
		Record      string `json:"record"`
		RecordOwner string `json:"record_owner"`
		Commit      string `json:"commit"`
		Ready       bool   `json:"ready"`
	}
	inbox := func() []entry {
		var entries []entry
		if err := json.Unmarshal([]byte(f.as("librarian").ok("--json", "inbox")), &entries); err != nil {
			t.Fatal(err)
		}
		return entries
	}
	verdict := "skeleton/nodes/" + ownerpkg.Key(intakeProposer) + "/concepts/35f57eb44edf2d11.verdict.json"
	if got := inbox(); len(got) != 1 || got[0].ID != item || got[0].Record != verdict || got[0].RecordOwner != intakeProposer ||
		got[0].Commit != "" || got[0].Ready {
		t.Fatalf("inbox before the verdict record = %+v", got)
	}
	if text := f.as("librarian").ok("inbox"); !strings.HasSuffix(strings.TrimSpace(text), "\tnot ready") {
		t.Fatalf("inbox text does not say the item is not ready: %q", text)
	}

	tree := filepath.Join(f.root, "proposer-tree")
	f.git(f.root, "--git-dir="+f.bare, "worktree", "add", "-q", "-b", "owner/"+ownerpkg.Key(intakeProposer), tree, "main")
	f.writeFile(filepath.Join(tree, verdict), `{"outcome":"near"}`+"\n")
	f.git(tree, "add", ".")
	f.git(tree, "commit", "-q", "-m", "concept: the gate's verdict")
	commit := f.git(tree, "rev-parse", "HEAD")
	if got := inbox(); len(got) != 1 || got[0].Commit != commit || !got[0].Ready {
		t.Fatalf("inbox after the verdict record = %+v, want commit %s and ready", got, commit)
	}
	// A verdict record that the branch deleted again is no input: the item is not ready.
	f.git(tree, "rm", "-q", verdict)
	f.git(tree, "commit", "-q", "-m", "concept: remove the verdict")
	if got := inbox(); len(got) != 1 || got[0].Commit != "" || got[0].Ready {
		t.Fatalf("inbox after the verdict record was deleted = %+v, want no commit and not ready", got)
	}
}

// With no listener, the notice is undelivered: the command prints it, appends it to the deployment's journal, and
// still exits 0, since the ledger item is the record.
func TestLibrarianIntakeWithoutAListenerJournalsTheNotice(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeNearID)
	args := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--topic-prefix", "notifications.example.", "--deploy", f.deploy)
	out, stderr, code := f.intake(map[string]string{"HOME": f.root}, args...)
	journal := filepath.Join(f.stateDir(), "undelivered.jsonl")
	if code != 0 || !out.Added || len(out.Undelivered) != 1 || out.Undelivered[0] != conceptsTopic || out.Journal != journal {
		t.Fatalf("intake exited %d with %+v: %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "notice not delivered: "+conceptsTopic) || !strings.Contains(stderr, item) {
		t.Fatalf("stderr does not print the notice: %s", stderr)
	}
	lines := journalLines(t, journal)
	var entry struct {
		Topic, Message, Key, Error string
		Payload                    map[string]any
	}
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &entry) != nil || entry.Topic != conceptsTopic ||
		entry.Key != ownerpkg.NoticeKey(item) || entry.Payload["item"] != item || entry.Error == "" {
		t.Fatalf("journal = %q", lines)
	}
	if got := f.item(item); got.Owner != "librarian" || got.State != "open" {
		t.Fatalf("item = %+v", got)
	}
}

// A host with no deployment file journals in the user cache directory (os.UserCacheDir): <cache>/owner/undelivered/.
func TestLibrarianIntakeWithoutADeploymentJournalsInTheUserCache(t *testing.T) {
	f := newCommandFixture(t)
	cache := filepath.Join(f.root, "cache")
	t.Setenv("XDG_CACHE_HOME", cache)
	args := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--topic-prefix", "notifications.example.")
	out, stderr, code := f.intake(map[string]string{"HOME": f.root}, args...)
	journal := filepath.Join(cache, "owner", "undelivered", "undelivered.jsonl")
	if code != 0 || out.Journal != journal || len(journalLines(t, journal)) != 1 {
		t.Fatalf("intake exited %d with %+v (journal %q): %s", code, out, journalLines(t, journal), stderr)
	}
	if lines := journalLines(t, filepath.Join(f.stateDir(), "undelivered.jsonl")); len(lines) != 0 {
		t.Fatalf("the deployment's journal got %q", lines)
	}
}

func TestLibrarianIntakeRefusesAMalformedProposalAndQueuesNothing(t *testing.T) {
	f := newCommandFixture(t)
	near := string(f.read(filepath.Join("..", "..", "owner", "testdata", "librarian", "near-envelope.txt")))
	lines := strings.Split(strings.TrimSuffix(near, "\n"), "\n")
	for name, c := range map[string]struct{ text, id string }{
		"no end marker":               {text: strings.Join(lines[:6], "\n") + "\n"},
		"a forged end marker line":    {text: strings.Join(append(append(append([]string(nil), lines[:5]...), lines[6]), lines[5:]...), "\n") + "\n"},
		"a header for another id":     {text: strings.Replace(near, "id: "+intakeNearID, "id: near-0123456789abcdef", 1)},
		"a recheck id on a near file": {text: strings.Replace(near, "id: "+intakeNearID, "id: "+intakeRecheckID, 1), id: intakeRecheckID},
		"two payload lines":           {text: strings.Join(append(append(append([]string(nil), lines[:6]...), lines[5]), lines[6]), "\n") + "\n"},
	} {
		id := c.id
		if id == "" {
			id = intakeNearID
		}
		file := f.write("malformed.txt", c.text)
		args := f.intakeArgs(id, file, "--git-dir", f.bare, "--deploy", f.deploy)
		if _, stderr, code := f.intake(f.gateEnv(), args...); code != 2 || !strings.Contains(stderr, "malformed librarian envelope") {
			t.Errorf("%s: exited %d, want 2 for a malformed envelope: %s", name, code, stderr)
		}
		f.noItem(ownerpkg.ConceptItem(intakeProposer, id))
	}
	if notices := f.notices(); len(notices) != 0 {
		t.Fatalf("malformed proposals published %d notices", len(notices))
	}
	if refs := f.git(f.root, "--git-dir="+f.bare, "for-each-ref", "refs/heads/inbox/"); refs != "" {
		t.Fatalf("a malformed proposal wrote %s", refs)
	}
}

func TestLibrarianIntakeUsageErrorsExitTwoAndQueueNothing(t *testing.T) {
	f := newCommandFixture(t)
	near, recheck := f.envelope("near-envelope.txt"), f.envelope("recheck-envelope.txt")
	noConfig := filepath.Join(f.root, "no-config.git")
	f.git(f.root, "init", "-q", "--bare", noConfig)
	// Each case names what its error must name.
	for name, c := range map[string]struct {
		args []string
		says string
	}{
		"a recheck without --git-dir":    {f.intakeArgs(intakeRecheckID, recheck, "--topic-prefix", "notifications.example."), "--git-dir"},
		"no topic prefix":                {f.intakeArgs(intakeNearID, near), "--topic-prefix"},
		"a prefix outside notifications": {f.intakeArgs(intakeNearID, near, "--topic-prefix", "example."), "--topic-prefix"},
		"an id the gate never makes":     {f.intakeArgs("near-0123", near, "--topic-prefix", "notifications.example."), "--id"},
		"a relative --envoy-config":      {f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--envoy-config", "envoy.json"), "--envoy-config"},
		"an unknown option":              {f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--owner", "librarian"), "--owner"},
		"a repeated option":              {f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--repository", "r"), "--repository"},
		"no proposal file":               {f.intakeArgs(intakeNearID, filepath.Join(f.root, "drafts", "missing.txt"), "--topic-prefix", "notifications.example."), "--proposal"},
		"no --id":                        {[]string{"--json", "librarian-intake", "--ledger-dsn-file", f.dsnFile(), "--repository", "r", "--proposer", intakeProposer, "--proposal", near}, "--id"},
		"a proposer outside the rule": {func() []string {
			args := f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.")
			args[len(args)-3] = "Function:Alloc frame"
			return args
		}(), "--proposer"},
		"a --git-dir that is not the deployment's bare_repo": {f.intakeArgs(intakeRecheckID, recheck, "--topic-prefix", "notifications.example.",
			"--git-dir", noConfig, "--deploy", f.deploy), "--git-dir"},
		"a --git-dir with no skeleton/owner.yaml": {f.intakeArgs(intakeNearID, near, "--git-dir", noConfig), "--topic-prefix"},
		"a configuration of another repository": {func() []string {
			args := f.intakeArgs(intakeNearID, near, "--git-dir", f.bare)
			args[5] = "another-repository"
			return args
		}(), "--repository"},
	} {
		_, stderr, code := f.intake(f.gateEnv(), c.args...)
		if code != 2 || !strings.Contains(stderr, c.says) || strings.Contains(stderr, "unknown command") {
			t.Errorf("%s: exited %d, want 2 naming %s: %s", name, code, c.says, stderr)
		}
	}
	f.noItem(ownerpkg.ConceptItem(intakeProposer, intakeNearID))
	f.noItem(ownerpkg.ConceptItem(intakeProposer, intakeRecheckID))
	if notices := f.notices(); len(notices) != 0 {
		t.Fatalf("usage errors published %d notices", len(notices))
	}
}

// A recheck record of that id that another proposer's delivery wrote is no record of this item: the recheck id names
// no proposer. The delivery is a protocol error, and it queues nothing.
func TestLibrarianIntakeForARecheckRecordOfAnotherProposerExitsFour(t *testing.T) {
	f := newCommandFixture(t)
	if _, stderr, code := f.intake(f.gateEnv(), f.intakeArgs(intakeRecheckID, f.envelope("recheck-envelope.txt"), "--git-dir", f.bare)...); code != 0 {
		t.Fatalf("first delivery exited %d: %s", code, stderr)
	}
	other := "function:other_frame@kernel/frame.c"
	args := f.intakeArgs(intakeRecheckID, f.envelope("recheck-envelope.txt", "proposer: "+intakeProposer, "proposer: "+other), "--git-dir", f.bare)
	args[len(args)-3] = other
	if _, stderr, code := f.intake(f.gateEnv(), args...); code != 4 || !strings.Contains(stderr, intakeProposer) {
		t.Fatalf("another proposer's recheck exited %d, want 4 naming the recorded proposer: %s", code, stderr)
	}
	f.noItem(ownerpkg.ConceptItem(other, intakeRecheckID))
	if notices := f.notices(); len(notices) != 1 {
		t.Fatalf("%d notices, want the first delivery's only", len(notices))
	}
}

// An item of that id in another owner's queue is a protocol error.
func TestLibrarianIntakeForAnItemOfAnotherOwnerExitsFour(t *testing.T) {
	f := newCommandFixture(t)
	item := ownerpkg.ConceptItem(intakeProposer, intakeNearID)
	f.enqueue(item, "function:other")
	args := f.intakeArgs(intakeNearID, f.envelope("near-envelope.txt"), "--topic-prefix", "notifications.example.")
	if _, stderr, code := f.intake(f.gateEnv(), args...); code != 4 || !strings.Contains(stderr, "function:other") {
		t.Fatalf("exited %d, want 4 naming the other owner: %s", code, stderr)
	}
	if got := f.item(item); got.Owner != "function:other" {
		t.Fatalf("item = %+v", got)
	}
	if notices := f.notices(); len(notices) != 0 {
		t.Fatalf("published %d notices", len(notices))
	}
}

// A ledger that cannot be reached exits 75, so the gate's caller retries; nothing is written first.
func TestLibrarianIntakeWithoutALedgerExitsSeventyFive(t *testing.T) {
	f := newCommandFixture(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	closed.Close()
	unreachable := filepath.Join(f.root, "unreachable-dsn")
	f.writeFile(unreachable, "postgres://ledger:secret@"+address+"/ledger?sslmode=disable&connect_timeout=5\n")
	args := f.intakeArgs(intakeRecheckID, f.envelope("recheck-envelope.txt"), "--git-dir", f.bare, "--deploy", f.deploy)
	args[3] = unreachable
	if _, stderr, code := f.intake(f.gateEnv(), args...); code != 75 {
		t.Fatalf("exited %d, want 75: %s", code, stderr)
	}
	if refs := f.git(f.root, "--git-dir="+f.bare, "for-each-ref", "refs/heads/inbox/"); refs != "" {
		t.Fatalf("an unreachable ledger left %s", refs)
	}
	if notices := f.notices(); len(notices) != 0 {
		t.Fatalf("published %d notices", len(notices))
	}
	if lines := journalLines(t, filepath.Join(f.stateDir(), "undelivered.jsonl")); len(lines) != 0 {
		t.Fatalf("journal = %q", lines)
	}
}

// An input the command needs to check the delivery that it cannot read exits 3 before anything is written.
func TestLibrarianIntakeThatCannotCheckExitsThree(t *testing.T) {
	f := newCommandFixture(t)
	badDeploy := filepath.Join(f.root, "bad-deploy.yaml")
	f.writeFile(badDeploy, "bare_repo: [\n")
	// A deployment whose state_dir is below a regular file: its journal cannot be written.
	f.writeFile(filepath.Join(f.root, "a-file"), "not a directory\n")
	unwritable := filepath.Join(f.root, "unwritable-deploy.yaml")
	f.writeFile(unwritable, strings.ReplaceAll(string(f.read(f.deploy)), f.stateDir(), filepath.Join(f.root, "a-file", "state")))
	// A librarian repository whose skeleton/owner.yaml does not load.
	seed := filepath.Join(f.root, "bad-config-seed")
	f.writeFile(filepath.Join(seed, ownerpkg.ConfigPath), "repo: [\n")
	f.git(f.root, "init", "-q", "-b", "main", seed)
	f.git(seed, "add", ".")
	f.git(seed, "commit", "-q", "-m", "skeleton: a configuration that does not load")
	badConfig := filepath.Join(f.root, "bad-config.git")
	f.git(f.root, "clone", "-q", "--bare", seed, badConfig)
	notARepository := filepath.Join(f.root, "not-a-repository")
	if err := os.MkdirAll(notARepository, 0o755); err != nil {
		t.Fatal(err)
	}
	near, recheck := f.envelope("near-envelope.txt"), f.envelope("recheck-envelope.txt")
	for name, args := range map[string][]string{
		"an unreadable DSN file": func() []string {
			args := f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.")
			args[3] = filepath.Join(f.root, "no-such-dsn")
			return args
		}(),
		"no such repository": func() []string {
			args := f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.")
			args[5] = "no-such-repository"
			return args
		}(),
		"a recheck for no such repository": func() []string {
			args := f.intakeArgs(intakeRecheckID, recheck, "--topic-prefix", "notifications.example.", "--git-dir", f.bare)
			args[5] = "no-such-repository"
			return args
		}(),
		"a deployment file that does not load":   f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--deploy", badDeploy),
		"a journal that cannot be written":       f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--deploy", unwritable),
		"a configuration that does not load":     f.intakeArgs(intakeNearID, near, "--git-dir", badConfig),
		"a --git-dir that is no repository":      f.intakeArgs(intakeRecheckID, recheck, "--topic-prefix", "notifications.example.", "--git-dir", notARepository),
		"a --git-dir that does not exist":        f.intakeArgs(intakeRecheckID, recheck, "--topic-prefix", "notifications.example.", "--git-dir", filepath.Join(f.root, "no-such.git")),
		"a named deployment file that is absent": f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--deploy", filepath.Join(f.root, "no-such-deploy.yaml")),
		"an --envoy-config that does not exist": f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--envoy-config",
			filepath.Join(f.root, "no-such-envoy.json")),
		"an --envoy-config that does not decode": f.intakeArgs(intakeNearID, near, "--topic-prefix", "notifications.example.", "--envoy-config",
			badDeploy),
	} {
		if _, stderr, code := f.intake(f.gateEnv(), args...); code != 3 {
			t.Errorf("%s: exited %d, want 3: %s", name, code, stderr)
		}
	}
	f.noItem(ownerpkg.ConceptItem(intakeProposer, intakeNearID))
	f.noItem(ownerpkg.ConceptItem(intakeProposer, intakeRecheckID))
	if notices := f.notices(); len(notices) != 0 {
		t.Fatalf("published %d notices", len(notices))
	}
	if refs := f.git(f.root, "--git-dir="+f.bare, "for-each-ref", "refs/heads/inbox/"); refs != "" {
		t.Fatalf("a delivery that could not be checked wrote %s", refs)
	}
}
