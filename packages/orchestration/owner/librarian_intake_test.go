package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// The envelopes in testdata/librarian are the gate's own text: conceptgate.Envelope (conceptgate/librarian.go) at
// legion 1e163669 wrote near-envelope.txt for the proposal file near-proposal.json, and recheck-envelope.txt for a near
// pair that a landing added (conceptgate/recheck.go:105-112), each with a fresh random nonce. The proposal's Lean source
// holds a line that looks like the gate's end marker.
const (
	fixtureNearID    = "near-35f57eb44edf2d11"
	fixtureRecheckID = "recheck-6b1f0c2d9e8a7f3b4c5d6e7f8a9b0c1d-name.46782e5370416c69676e6564"
	fixtureProposer  = "function:alloc_frame@kernel/frame.c"
)

func readLibrarianFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "librarian", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGateIDKindReadsTheGatesTwoIDForms(t *testing.T) {
	for id, want := range map[string]string{
		fixtureNearID:           "near",
		fixtureRecheckID:        "recheck",
		"near-0123456789abcdef": "near",
		"recheck-0123456789abcdef0123456789abcdef-fedcba9876543210fedcba9876543210": "recheck",
		"recheck-name.6162-name.63": "recheck",
		"":                          "",
		"near-":                     "",
		"near-0123456789ABCDEF":     "",
		"near-0123456789abcdef0":    "",
		"near-0123456789abcdef@1":   "",
		"recheck-0123456789abcdef0123456789abcdef":                                   "",
		"recheck-0123456789abcdef0123456789abcdef-":                                  "",
		"recheck-0123456789abcdef0123456789abcdef-name.":                             "",
		"recheck-0123456789abcdef0123456789abcdef-name.616":                          "",
		"recheck-0123456789abcdef0123456789abcdef-name.6G":                           "",
		"recheck-0123456789abcdef0123456789abcdef-0123456789abcdef":                  "",
		"recheck-0123456789abcdef0123456789abcdef-fedcba9876543210fedcba9876543210-": "",
		"recheck-0123456789abcdef0123456789abcdef-name.61-name.62":                   "",
		"near-0123456789abcdef\n":                                                    "",
	} {
		if got := GateIDKind(id); got != want {
			t.Errorf("GateIDKind(%q) = %q, want %q", id, got, want)
		}
	}
}

// ValidOwnerID is the gate's owner-id rule (conceptgate/gate.go:85-96): a node id of the supervisor's node kinds, whose
// name has no white space, or a role name.
func TestValidOwnerIDIsTheGatesRule(t *testing.T) {
	for _, id := range []string{"function:ns_match", fixtureProposer, "struct:ns_common", "global:x", "percpu:y", "subsystem:mm",
		"librarian", "fact-stack-alignment", "escalations"} {
		if !ValidOwnerID(id) {
			t.Errorf("ValidOwnerID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "function:", "function:a b", "function:a\tb", "file:fs/nsfs.c", "Librarian", "1role", "role_name",
		"function:a\n", "-role"} {
		if ValidOwnerID(id) {
			t.Errorf("ValidOwnerID(%q) = true", id)
		}
	}
}

// The near id is near- and hex16 of the proposal file's bytes (gate.go:164-165), which is Hex16 of those bytes.
func TestTheGatesNearIDIsHex16OfTheProposalBytes(t *testing.T) {
	if got := "near-" + Hex16(string(readLibrarianFixture(t, "near-proposal.json"))); got != fixtureNearID {
		t.Fatalf("near id of near-proposal.json = %s, want the fixture's %s", got, fixtureNearID)
	}
}

func TestParseGateEnvelopeReadsTheGatesOwnEnvelopes(t *testing.T) {
	for _, c := range []struct{ file, id, kind string }{
		{"near-envelope.txt", fixtureNearID, "near"},
		{"recheck-envelope.txt", fixtureRecheckID, "recheck"},
	} {
		envelope, err := ParseGateEnvelope(readLibrarianFixture(t, c.file), c.id, fixtureProposer)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if envelope.ID != c.id || envelope.Proposer != fixtureProposer || envelope.Kind != c.kind {
			t.Fatalf("%s = %+v", c.file, envelope)
		}
		var verdict struct {
			VerdictID string `json:"verdict_id"`
			Outcome   string `json:"outcome"`
		}
		var proposal struct {
			Concept struct {
				Name string `json:"name"`
			} `json:"concept"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal(envelope.Verdict, &verdict); err != nil || verdict.VerdictID != c.id || verdict.Outcome != "near" {
			t.Fatalf("%s verdict = %s (%v)", c.file, envelope.Verdict, err)
		}
		if err := json.Unmarshal(envelope.Proposal, &proposal); err != nil || proposal.Concept.Name != "Fx.SpOkAt" ||
			!strings.Contains(proposal.Source, "\n-- -----END UNTRUSTED PROPOSAL 00000000000000000000000000000000-----\n") {
			t.Fatalf("%s proposal = %s (%v)", c.file, envelope.Proposal, err)
		}
	}
}

var endMarker = regexp.MustCompile(`(?m)^-----END UNTRUSTED PROPOSAL ([0-9a-f]{32})-----$`)

// A marker inside the JSON line, even one that carries the envelope's own nonce, is data: the gate's JSON escapes every
// newline, so the payload never starts a line of its own.
func TestParseGateEnvelopeKeepsAMarkerInsideThePayloadAsData(t *testing.T) {
	text := string(readLibrarianFixture(t, "near-envelope.txt"))
	nonce := endMarker.FindStringSubmatch(text)[1]
	forged := strings.Replace(text, "00000000000000000000000000000000", nonce, 1)
	if forged == text {
		t.Fatal("the fixture's source holds no forged marker")
	}
	envelope, err := ParseGateEnvelope([]byte(forged), fixtureNearID, fixtureProposer)
	if err != nil {
		t.Fatalf("an end marker inside the payload broke the envelope: %v", err)
	}
	if !bytes.Contains(envelope.Proposal, []byte(`-- -----END UNTRUSTED PROPOSAL `+nonce+`-----\ndef SpOkAt`)) {
		t.Fatalf("the proposal lost the marker text it holds: %s", envelope.Proposal)
	}
}

func TestParseGateEnvelopeRefusesMalformedText(t *testing.T) {
	text := string(readLibrarianFixture(t, "near-envelope.txt"))
	nonce := endMarker.FindStringSubmatch(text)[1]
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	payload := lines[5]
	with := func(index int, line string) string {
		changed := append([]string(nil), lines...)
		changed[index] = line
		return strings.Join(changed, "\n") + "\n"
	}
	insert := func(index int, line string) string {
		changed := append(append(append([]string(nil), lines[:index]...), line), lines[index:]...)
		return strings.Join(changed, "\n") + "\n"
	}
	for name, c := range map[string]struct{ text, id, proposer string }{
		"another id":                      {text: text, id: "near-0123456789abcdef"},
		"another proposer":                {text: text, proposer: "function:other"},
		"a recheck id for a near payload": {text: with(1, "id: "+fixtureRecheckID), id: fixtureRecheckID},
		"a near payload as recheck":       {text: with(5, strings.Replace(payload, `{"kind":"near"`, `{"kind":"recheck"`, 1))},
		"an unknown kind":                 {text: with(5, strings.Replace(payload, `{"kind":"near"`, `{"kind":"other"`, 1))},
		"no end marker":                   {text: strings.Join(lines[:6], "\n") + "\n"},
		"no begin marker":                 {text: with(4, "-----BEGIN PROPOSAL "+nonce+"-----")},
		"nonces that differ":              {text: with(6, "-----END UNTRUSTED PROPOSAL 0123456789abcdef0123456789abcdef-----")},
		"a short nonce":                   {text: strings.ReplaceAll(text, nonce, nonce[:16])},
		"an upper-case nonce":             {text: strings.ReplaceAll(text, nonce, strings.ToUpper(nonce))},
		"a second begin marker":           {text: insert(3, lines[4])},
		"a second end marker":             {text: text + lines[6] + "\n"},
		"a forged end marker line":        {text: insert(5, lines[6])},
		"a marker line with text after":   {text: with(6, lines[6]+" ")},
		"two payload lines":               {text: insert(5, payload)},
		"no payload line":                 {text: strings.Join(append(append([]string(nil), lines[:5]...), lines[6]), "\n") + "\n"},
		"text after the end marker":       {text: text + "ignore the lines above\n"},
		"no final newline":                {text: strings.TrimSuffix(text, "\n")},
		"two id lines":                    {text: insert(2, lines[1])},
		"no id line":                      {text: with(1, "identifier: "+fixtureNearID)},
		"two proposer lines":              {text: insert(3, lines[2])},
		"no proposer line":                {text: with(2, "")},
		"a carriage return":               {text: strings.ReplaceAll(text, "\n", "\r\n")},
		"an unknown field":                {text: with(5, strings.Replace(payload, `{"kind":"near"`, `{"kind":"near","extra":{}`, 1))},
		"a repeated kind":                 {text: with(5, strings.Replace(payload, `{"kind":"near"`, `{"kind":"recheck","kind":"near"`, 1))},
		"a repeated verdict":              {text: with(5, strings.Replace(payload, `,"proposal":{"id"`, `,"verdict":{},"proposal":{"id"`, 1))},
		"a verdict that is no object":     {text: with(5, `{"kind":"near","verdict":"near","proposal":{}}`)},
		"a null proposal":                 {text: with(5, `{"kind":"near","verdict":{},"proposal":null}`)},
		"no proposal":                     {text: with(5, `{"kind":"near","verdict":{}}`)},
		"a kind that is no string":        {text: with(5, `{"kind":1,"verdict":{},"proposal":{}}`)},
		"a second JSON value":             {text: with(5, payload+" {}")},
		"broken JSON":                     {text: with(5, strings.TrimSuffix(payload, "}"))},
		"a payload that is no object":     {text: with(5, `["near"]`)},
		"invalid UTF-8":                   {text: with(5, strings.Replace(payload, "Fx.SpOkAt", "Fx.Sp\xffOkAt", 1))},
		"an empty file":                   {text: ""},
	} {
		id, proposer := c.id, c.proposer
		if id == "" {
			id = fixtureNearID
		}
		if proposer == "" {
			proposer = fixtureProposer
		}
		if _, err := ParseGateEnvelope([]byte(c.text), id, proposer); !errors.Is(err, ErrMalformedEnvelope) {
			t.Errorf("%s: ParseGateEnvelope error = %v, want ErrMalformedEnvelope", name, err)
		}
	}
}

// The record the intake keeps of a recheck is the envelope's JSON, marked untrusted, with the verdict and the proposal
// as the gate wrote them.
func TestInboxRecordIsTheUntrustedEnvelope(t *testing.T) {
	envelope := parseFixture(t, "recheck-envelope.txt", fixtureRecheckID)
	want := `{"untrusted":true,"id":"` + fixtureRecheckID + `","proposer":"` + fixtureProposer + `","kind":"recheck","verdict":` +
		string(envelope.Verdict) + `,"proposal":` + string(envelope.Proposal) + "}\n"
	if got := string(envelope.InboxRecord()); got != want {
		t.Fatalf("InboxRecord =\n%s\nwant\n%s", got, want)
	}
	if LibrarianInboxRecord(fixtureRecheckID) != "inbox/"+fixtureRecheckID+".json" || ConceptItem(fixtureProposer, fixtureRecheckID) !=
		"concept:"+fixtureProposer+"@"+fixtureRecheckID {
		t.Fatalf("record %s, item %s", LibrarianInboxRecord(fixtureRecheckID), ConceptItem(fixtureProposer, fixtureRecheckID))
	}
}

func TestAddFileOnceWritesABranchWithPlumbingAndNeverRewritesAFile(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	first, created, err := repo.AddFileOnce(ctx, "inbox/librarian", "inbox/a.json", []byte("a\n"), "inbox: a")
	if err != nil || !created {
		t.Fatalf("first file: %s %t %v", first, created, err)
	}
	if parents := gitOutput(t, repo.Path, nil, "log", "-1", "--format=%P", first); parents != "" {
		t.Fatalf("the branch's first commit has parents %q", parents)
	}
	second, created, err := repo.AddFileOnce(ctx, "inbox/librarian", "inbox/b.json", []byte("b\n"), "inbox: b")
	if err != nil || !created {
		t.Fatalf("second file: %s %t %v", second, created, err)
	}
	if parent := gitOutput(t, repo.Path, nil, "log", "-1", "--format=%P", second); parent != first {
		t.Fatalf("the second commit's parent is %q, want %s", parent, first)
	}
	again, created, err := repo.AddFileOnce(ctx, "inbox/librarian", "inbox/a.json", []byte("changed\n"), "inbox: a again")
	if err != nil || created || again != first {
		t.Fatalf("rewrite of a: %s %t %v, want the first commit %s and nothing written", again, created, err, first)
	}
	if tip := gitOutput(t, repo.Path, nil, "rev-parse", "refs/heads/inbox/librarian"); tip != second {
		t.Fatalf("branch tip = %s, want %s", tip, second)
	}
	if got := gitOutput(t, repo.Path, nil, "show", "inbox/librarian:inbox/a.json"); got != "a" {
		t.Fatalf("inbox/a.json = %q, want the first bytes", got)
	}
	if got := gitOutput(t, repo.Path, nil, "ls-tree", "-r", "--name-only", "inbox/librarian"); got != "inbox/a.json\ninbox/b.json" {
		t.Fatalf("the branch holds %q, want only the two inbox files", got)
	}
	if worktrees := gitOutput(t, repo.Path, nil, "worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("AddFileOnce made a worktree: %s", worktrees)
	}
	if main := gitOutput(t, repo.Path, nil, "ls-tree", "--name-only", "main"); main != "shared.txt" {
		t.Fatalf("main changed: %q", main)
	}
}

// Writers that race for the branch each land their file: a writer whose update-ref lost the race reads the new tip and
// commits again.
func TestAddFileOnceKeepsEveryFileOfConcurrentWriters(t *testing.T) {
	repo := newTestRepo(t)
	const writers = 8
	var wait sync.WaitGroup
	errs := make(chan error, writers)
	start := make(chan struct{})
	for index := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			name := fmt.Sprintf("inbox/%d.json", index)
			if _, created, err := repo.AddFileOnce(context.Background(), "inbox/librarian", name, []byte(name+"\n"), "inbox: "+name); err != nil || !created {
				errs <- fmt.Errorf("%s: created %t, %v", name, created, err)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if files := strings.Fields(gitOutput(t, repo.Path, nil, "ls-tree", "-r", "--name-only", "inbox/librarian")); len(files) != writers {
		t.Fatalf("the branch holds %v, want %d files", files, writers)
	}
	if count := gitOutput(t, repo.Path, nil, "rev-list", "--count", "inbox/librarian"); count != fmt.Sprint(writers) {
		t.Fatalf("the branch has %s commits, want one for each writer", count)
	}
}

// fakeIntakeLedger is a ledger of one repository, r, for Deliver's own order of steps. itemDelay slows each Item read;
// a positive enqueueNeeds refuses an Enqueue whose context has less time left than that.
type fakeIntakeLedger struct {
	items        map[string]ledger.Item
	itemDelay    time.Duration
	enqueueNeeds time.Duration
}

func (l *fakeIntakeLedger) HasOpenWork(_ context.Context, repo, _ string) (bool, error) {
	if repo != "r" {
		return false, &ledger.NotFoundError{What: "repository", ID: repo}
	}
	return len(l.items) > 0, nil
}

func (l *fakeIntakeLedger) Item(_ context.Context, _, id string) (ledger.Item, error) {
	time.Sleep(l.itemDelay)
	if item, found := l.items[id]; found {
		return item, nil
	}
	return ledger.Item{}, &ledger.NotFoundError{What: "item", ID: id}
}

func (l *fakeIntakeLedger) Enqueue(ctx context.Context, _, id, owner string) error {
	if deadline, ok := ctx.Deadline(); l.enqueueNeeds > 0 && (!ok || time.Until(deadline) < l.enqueueNeeds) {
		return fmt.Errorf("enqueue with %v left: %w", time.Until(deadline), context.DeadlineExceeded)
	}
	l.items[id] = ledger.Item{ID: id, Owner: owner, State: "open"}
	return nil
}

func newTestIntake(t *testing.T, l *fakeIntakeLedger, inbox *Repo) LibrarianIntake {
	t.Helper()
	routing, err := NewRouting("notifications.example.", nil)
	if err != nil {
		t.Fatal(err)
	}
	return LibrarianIntake{Ledger: l, Repo: "r", Inbox: inbox, Routing: routing, Envoy: NoListener{Reason: errors.New("no listener")},
		Journal: filepath.Join(t.TempDir(), "undelivered.jsonl")}
}

func parseFixture(t *testing.T, name, id string) GateEnvelope {
	t.Helper()
	envelope, err := ParseGateEnvelope(readLibrarianFixture(t, name), id, fixtureProposer)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// A delivery whose item is still open for the librarian publishes its notice again, under the same key: the first
// delivery may have queued the item and then lost its notice (a failed journal, a lost reply, a kill), and the
// listener drops a copy it already has. A done item publishes nothing, and another owner's item is ErrIDOwner.
func TestDeliverPublishesAgainOnlyForAnItemStillOpenForTheLibrarian(t *testing.T) {
	envelope := parseFixture(t, "near-envelope.txt", fixtureNearID)
	item := ConceptItem(fixtureProposer, fixtureNearID)
	for name, c := range map[string]struct {
		existing ledger.Item
		notices  int
		idOwner  bool
	}{
		"open for the librarian": {existing: ledger.Item{ID: item, Owner: Librarian, State: "open"}, notices: 1},
		"done":                   {existing: ledger.Item{ID: item, Owner: Librarian, State: "done"}},
		"another owner's":        {existing: ledger.Item{ID: item, Owner: "function:other", State: "open"}, idOwner: true},
	} {
		l := &fakeIntakeLedger{items: map[string]ledger.Item{item: c.existing}}
		outcome, err := newTestIntake(t, l, nil).Deliver(context.Background(), envelope)
		if c.idOwner != errors.Is(err, ErrIDOwner) || (!c.idOwner && err != nil) {
			t.Errorf("%s: error %v", name, err)
		}
		if outcome.Added || len(outcome.Undelivered) != c.notices {
			t.Errorf("%s: outcome %+v, want %d notices and nothing added", name, outcome, c.notices)
		}
		if c.notices > 0 && len(outcome.Undelivered) > 0 && outcome.Undelivered[0].Key != NoticeKey(item) {
			t.Errorf("%s: key %s, want the first delivery's %s", name, outcome.Undelivered[0].Key, NoticeKey(item))
		}
	}
}

// The recheck id names no proposer, so a record of that id that another proposer's delivery wrote is no record of this
// item: the delivery is a protocol error, and it queues nothing.
func TestDeliverRefusesARecheckWhoseRecordNamesAnotherProposer(t *testing.T) {
	repo := newTestRepo(t)
	l := &fakeIntakeLedger{items: map[string]ledger.Item{}}
	if _, err := newTestIntake(t, l, &repo).Deliver(context.Background(), parseFixture(t, "recheck-envelope.txt", fixtureRecheckID)); err != nil {
		t.Fatal(err)
	}
	other := "function:other_frame@kernel/frame.c"
	text := strings.Replace(string(readLibrarianFixture(t, "recheck-envelope.txt")), "proposer: "+fixtureProposer, "proposer: "+other, 1)
	envelope, err := ParseGateEnvelope([]byte(text), fixtureRecheckID, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestIntake(t, l, &repo).Deliver(context.Background(), envelope); !errors.Is(err, ErrRecordProposer) {
		t.Fatalf("a recheck of another proposer: %v, want ErrRecordProposer", err)
	}
	if _, queued := l.items[ConceptItem(other, fixtureRecheckID)]; queued {
		t.Fatal("the other proposer's item was queued")
	}
}

// Each ledger call has its own deadline: time the delivery spends before it (a slow read, the record's git write)
// does not shorten the enqueue's.
func TestDeliverGivesEachLedgerCallItsOwnDeadline(t *testing.T) {
	repo := newTestRepo(t)
	l := &fakeIntakeLedger{items: map[string]ledger.Item{}, itemDelay: 300 * time.Millisecond, enqueueNeeds: 300 * time.Millisecond}
	intake := newTestIntake(t, l, &repo)
	intake.LedgerTimeout = 400 * time.Millisecond
	if _, err := intake.Deliver(context.Background(), parseFixture(t, "recheck-envelope.txt", fixtureRecheckID)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// A ref lock that another writer holds, or that a killed git left, does not move the tip: the write waits for it, and
// names the lock when it stays.
func TestAddFileOnceWaitsForAHeldRefLockAndNamesAStaleOne(t *testing.T) {
	repo := newTestRepo(t)
	lock := filepath.Join(repo.Path, "refs", "heads", "inbox", "librarian.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Remove(lock)
		close(released)
	}()
	if _, created, err := repo.AddFileOnce(context.Background(), "inbox/librarian", "inbox/a.json", []byte("a\n"), "inbox: a"); err != nil || !created {
		t.Fatalf("a lock released after 400 ms: created %t, %v", created, err)
	}
	<-released
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := repo.AddFileOnce(context.Background(), "inbox/librarian", "inbox/b.json", []byte("b\n"), "inbox: b")
	if err == nil || !strings.Contains(err.Error(), lock) {
		t.Fatalf("a stale lock: %v, want an error naming %s", err, lock)
	}
}

// The inbox branch is a branch: a symbolic ref there is refused, so no record lands on the ref it points to.
func TestAddFileOnceRefusesASymbolicRef(t *testing.T) {
	repo := newTestRepo(t)
	main := gitOutput(t, repo.Path, nil, "rev-parse", "main")
	gitOutput(t, repo.Path, nil, "symbolic-ref", "refs/heads/inbox/librarian", "refs/heads/main")
	if _, _, err := repo.AddFileOnce(context.Background(), "inbox/librarian", "inbox/a.json", []byte("a\n"), "inbox: a"); err == nil {
		t.Fatal("AddFileOnce wrote through a symbolic ref")
	}
	if got := gitOutput(t, repo.Path, nil, "rev-parse", "main"); got != main {
		t.Fatalf("main moved from %s to %s", main, got)
	}
}

// A notice the journal could not keep is still in the outcome, so that the command prints it: the item is queued, and
// a later delivery of the open item publishes it again.
func TestDeliverKeepsTheNoticeThatTheJournalCouldNotKeep(t *testing.T) {
	intake := newTestIntake(t, &fakeIntakeLedger{items: map[string]ledger.Item{}}, nil)
	intake.Journal = "relative/undelivered.jsonl"
	outcome, err := intake.Deliver(context.Background(), parseFixture(t, "near-envelope.txt", fixtureNearID))
	if err == nil || !outcome.Added || len(outcome.Undelivered) != 1 || outcome.Undelivered[0].Topic != "notifications.example.concepts" {
		t.Fatalf("Deliver with an unwritable journal = %+v, %v", outcome, err)
	}
}

// A recheck checks its ledger repository before it writes its record, so a delivery to a repository that does not
// exist leaves no record behind.
func TestDeliverChecksTheRepositoryBeforeItRecordsARecheck(t *testing.T) {
	repo := newTestRepo(t)
	intake := newTestIntake(t, &fakeIntakeLedger{items: map[string]ledger.Item{}}, &repo)
	intake.Repo = "no-such-repository"
	envelope := parseFixture(t, "recheck-envelope.txt", fixtureRecheckID)
	var notFound *ledger.NotFoundError
	if _, err := intake.Deliver(context.Background(), envelope); !errors.As(err, &notFound) || notFound.What != "repository" {
		t.Fatalf("Deliver to no repository: %v", err)
	}
	if refs := gitOutput(t, repo.Path, nil, "for-each-ref", "refs/heads/inbox/"); refs != "" {
		t.Fatalf("Deliver wrote %s before it found no repository", refs)
	}
}
