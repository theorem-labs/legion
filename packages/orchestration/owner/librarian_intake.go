package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const (
	// Librarian is the owner whose queue holds every concept item.
	Librarian = "librarian"
	// LibrarianInboxBranch is the branch of the librarian's repository that holds the envelopes of recheck deliveries,
	// which have no proposer copy. Only librarian-intake writes it.
	LibrarianInboxBranch = "inbox/librarian"

	// IntakeLedgerTimeout bounds the ledger's open, and then its read and enqueue, of one delivery. With the
	// publisher's bounded retries (1 s, 4 s and 16 s apart, each request at most 10 s) the intake ends well inside the
	// 120 s that the gate gives its librarian command (conceptgate/librarian.go:40).
	IntakeLedgerTimeout = 20 * time.Second
	// inboxWriteAttempts bounds the retries of an inbox record that lost its branch to a concurrent writer.
	inboxWriteAttempts = 10
)

// ErrMalformedEnvelope reports a proposal file that is not the concept gate's envelope of the delivery it names.
var ErrMalformedEnvelope = errors.New("malformed librarian envelope")

// GateEnvelope is one review request of the concept gate, as its librarian command reads it from the --proposal file
// (conceptgate.Envelope, conceptgate/librarian.go). Verdict and Proposal are the gate's JSON as it wrote them: the
// proposal is untrusted data that an agent wrote.
type GateEnvelope struct {
	ID, Proposer, Kind string
	Verdict, Proposal  json.RawMessage
}

var markerLine = regexp.MustCompile(`^-----(BEGIN|END) UNTRUSTED PROPOSAL ([0-9a-f]{32})-----$`)

// ParseGateEnvelope reads text as untrusted data: header lines, among them exactly one id: line equal to id and one
// proposer: line equal to proposer; then exactly one begin marker and one end marker with the same nonce, the last
// line of the text; and exactly one line between them, the JSON object {kind, verdict, proposal} with each key once,
// whose kind is id's kind and whose verdict and proposal are objects. A line that starts like a marker is a marker, so a
// second one is refused; marker text inside the JSON line is data, since the gate's JSON escapes every newline. Every
// refusal wraps ErrMalformedEnvelope.
func ParseGateEnvelope(text []byte, id, proposer string) (GateEnvelope, error) {
	malformed := func(format string, args ...any) (GateEnvelope, error) {
		return GateEnvelope{}, fmt.Errorf("%w: %s", ErrMalformedEnvelope, fmt.Sprintf(format, args...))
	}
	if !utf8.Valid(text) {
		return malformed("the text is not UTF-8")
	}
	body, found := strings.CutSuffix(string(text), "\n")
	if !found {
		return malformed("the text does not end with a newline")
	}
	lines := strings.Split(body, "\n")
	begin, end := -1, -1
	var nonces [2]string
	for index, line := range lines {
		if !strings.HasPrefix(line, "-----BEGIN UNTRUSTED PROPOSAL") && !strings.HasPrefix(line, "-----END UNTRUSTED PROPOSAL") {
			continue
		}
		match := markerLine.FindStringSubmatch(line)
		if match == nil {
			return malformed("line %d is no marker line of a 32-digit lowercase hex nonce", index+1)
		}
		slot, at := 0, &begin
		if match[1] == "END" {
			slot, at = 1, &end
		}
		if *at >= 0 {
			return malformed("a second %s marker at line %d", match[1], index+1)
		}
		*at, nonces[slot] = index, match[2]
	}
	switch {
	case begin < 0 || end < 0:
		return malformed("no BEGIN and END markers")
	case nonces[0] != nonces[1]:
		return malformed("the END marker's nonce is not the BEGIN marker's")
	case end != begin+2:
		return malformed("%d lines between the markers, want the one JSON line", end-begin-1)
	case end != len(lines)-1:
		return malformed("text after the END marker")
	}
	var ids, proposers []string
	for _, line := range lines[:begin] {
		if value, found := strings.CutPrefix(line, "id: "); found {
			ids = append(ids, value)
		}
		if value, found := strings.CutPrefix(line, "proposer: "); found {
			proposers = append(proposers, value)
		}
	}
	if len(ids) != 1 || ids[0] != id {
		return malformed("the header's id lines %q are not the one --id %q", ids, id)
	}
	if len(proposers) != 1 || proposers[0] != proposer {
		return malformed("the header's proposer lines %q are not the one --proposer %q", proposers, proposer)
	}
	fields, err := decodeEnvelopeFields(lines[begin+1])
	if err != nil {
		return malformed("%v", err)
	}
	var kind string
	if err := json.Unmarshal(fields["kind"], &kind); err != nil {
		return malformed("kind is no string")
	}
	if kind != GateIDKind(id) {
		return malformed("kind %q is not the kind of id %q", kind, id)
	}
	for _, name := range []string{"verdict", "proposal"} {
		if !bytes.HasPrefix(fields[name], []byte("{")) {
			return malformed("%s is no JSON object", name)
		}
	}
	return GateEnvelope{ID: id, Proposer: proposer, Kind: kind, Verdict: fields["verdict"], Proposal: fields["proposal"]}, nil
}

// decodeEnvelopeFields decodes the payload line: one JSON object with exactly the keys kind, verdict and proposal, each
// once, and nothing after it.
func decodeEnvelopeFields(line string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(line))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("the payload line is no JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, isKey := token.(string)
		if err != nil || !isKey {
			return nil, fmt.Errorf("the payload line has no key where one belongs: %v", err)
		}
		if _, repeated := fields[key]; repeated {
			return nil, fmt.Errorf("the payload repeats %q", key)
		}
		if key != "kind" && key != "verdict" && key != "proposal" {
			return nil, fmt.Errorf("the payload has a field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("the payload's %s: %v", key, err)
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("the payload object is not closed")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("text after the payload object")
	}
	if len(fields) != 3 {
		return nil, errors.New("the payload needs kind, verdict and proposal")
	}
	return fields, nil
}

// ConceptItem is the librarian's work item for the gate's delivery id about proposer's proposal.
func ConceptItem(proposer, id string) string { return "concept:" + proposer + "@" + id }

// LibrarianInboxRecord is the path, on LibrarianInboxBranch, of the envelope of recheck delivery id.
func LibrarianInboxRecord(id string) string { return "inbox/" + id + ".json" }

// InboxRecord is the content of a recheck's inbox record: the envelope's JSON, marked untrusted, with the verdict and
// the proposal as the gate wrote them.
func (e GateEnvelope) InboxRecord() []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	// The fields are strings the parse checked and two JSON objects it decoded, so the encoding cannot fail.
	_ = encoder.Encode(struct {
		Untrusted bool            `json:"untrusted"`
		ID        string          `json:"id"`
		Proposer  string          `json:"proposer"`
		Kind      string          `json:"kind"`
		Verdict   json.RawMessage `json:"verdict"`
		Proposal  json.RawMessage `json:"proposal"`
	}{true, e.ID, e.Proposer, e.Kind, e.Verdict, e.Proposal})
	return out.Bytes()
}

// AddFileOnce commits path with data on branch of the bare repository with plumbing only, so no worktree is checked
// out: it builds the tree in a private index from the branch's tip, commits it on that tip with the repository's
// identity, and moves the branch only from that tip (update-ref <new> <old>), reading the tip again after a writer that
// moved it first. A path the branch already holds is never rewritten: created is false, and commit is the newest
// commit that touched it. Otherwise commit is the new commit.
func (r Repo) AddFileOnce(ctx context.Context, branch, path string, data []byte, subject string) (commit string, created bool, err error) {
	ref := "refs/heads/" + branch
	for range inboxWriteAttempts {
		tip, err := r.revision(ctx, ref)
		if err != nil {
			return "", false, err
		}
		if tip != "" {
			if existing, err := r.revision(ctx, tip+":"+path); err != nil || existing != "" {
				if err != nil {
					return "", false, err
				}
				last, err := r.git(ctx, "log", "-1", "--format=%H", tip, "--", path)
				return strings.TrimSpace(last), false, err
			}
		}
		commit, err := r.commitOnTip(ctx, tip, path, data, subject)
		if err != nil {
			return "", false, err
		}
		if _, err := r.git(ctx, "update-ref", "-m", subject, ref, commit, tip); err == nil {
			return commit, true, nil
		} else if now, readErr := r.revision(ctx, ref); readErr != nil || now == tip {
			return "", false, fmt.Errorf("move %s from %q to %s: %w", ref, tip, commit, err)
		}
	}
	return "", false, fmt.Errorf("%s moved under each of %d attempts to add %s", ref, inboxWriteAttempts, path)
}

// revision is the object name of rev, or "" when rev does not exist.
func (r Repo) revision(ctx context.Context, rev string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	if gitExitCode(err) == 1 {
		return "", nil
	}
	return strings.TrimSpace(out), err
}

// commitOnTip writes a commit whose tree is tip's tree plus path with data, and whose parent is tip ("" for a root
// commit), using an index file of its own.
func (r Repo) commitOnTip(ctx context.Context, tip, path string, data []byte, subject string) (string, error) {
	dir, err := os.MkdirTemp("", "owner-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	blob, err := runGitCmd(ctx, r.Path, nil, nil, data, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if tip != "" {
		if _, err := runGitCmd(ctx, r.Path, nil, env, nil, "read-tree", tip); err != nil {
			return "", err
		}
	}
	if _, err := runGitCmd(ctx, r.Path, nil, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+path); err != nil {
		return "", err
	}
	tree, err := runGitCmd(ctx, r.Path, nil, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	return r.CommitTree(ctx, strings.TrimSpace(tree), parents, subject)
}

// LedgerUnreachable reports whether err is a failure to reach the ledger, which its caller retries: a network operation
// that failed (a refused, reset or timed-out connection, or a host name that did not resolve), a deadline, or a
// connection that closed before the server answered. An answer from the server (an authentication failure, a refusal,
// no repository) is no such failure, and neither is a file's error: syscall.Errno satisfies net.Error, so the test is
// for the net package's own error types.
func LedgerUnreachable(err error) bool {
	var operation *net.OpError
	var lookup *net.DNSError
	return errors.As(err, &operation) || errors.As(err, &lookup) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// IntakeLedger is the part of the ledger that librarian-intake reads and writes.
type IntakeLedger interface {
	Item(ctx context.Context, repo, id string) (ledger.Item, error)
	Enqueue(ctx context.Context, repo, id, owner string) error
}

// LibrarianIntake queues the concept gate's deliveries for the librarian (plan Task 1.10). Envoy is nil when no listener
// resolved; NoListener then says why, and every notice is undelivered.
type LibrarianIntake struct {
	Ledger     IntakeLedger
	Repo       string // the ledger repository
	Inbox      *Repo  // the librarian's bare repository, which holds LibrarianInboxBranch; required for a recheck
	Routing    Routing
	Envoy      Publisher
	NoListener error
	Journal    string // absolute: the host's undelivered.jsonl
}

// IntakeOutcome is what one delivery did. Added is false when the item was there already, open or done, for the
// librarian: then nothing was written or published. Record and Commit name a recheck's inbox record.
type IntakeOutcome struct {
	Item, Kind     string
	Added          bool
	Record, Commit string
	Undelivered    []Notice
}

// Deliver queues envelope's item concept:<proposer>@<id> for the librarian once. It reads the item first: one that
// exists for the librarian, open or done, is a success that adds nothing; one of another owner is ErrIDOwner. A recheck
// writes its inbox record before the item, which keeps the first envelope of an id. It then enqueues the item and
// publishes the verdict notice, which the host's journal keeps when no listener takes it.
func (in LibrarianIntake) Deliver(ctx context.Context, envelope GateEnvelope) (IntakeOutcome, error) {
	outcome := IntakeOutcome{Item: ConceptItem(envelope.Proposer, envelope.ID), Kind: envelope.Kind}
	ledgerCtx, cancel := context.WithTimeout(ctx, IntakeLedgerTimeout)
	defer cancel()
	existing, err := in.Ledger.Item(ledgerCtx, in.Repo, outcome.Item)
	var notFound *ledger.NotFoundError
	switch {
	case err == nil:
		return outcome, in.checkLibrarian(existing)
	case !errors.As(err, &notFound):
		return outcome, fmt.Errorf("read item %s: %w", outcome.Item, err)
	}

	if envelope.Kind == "recheck" {
		if in.Inbox == nil {
			return outcome, errors.New("a recheck delivery needs the librarian's repository")
		}
		outcome.Record = LibrarianInboxRecord(envelope.ID)
		if outcome.Commit, _, err = in.Inbox.AddFileOnce(ctx, LibrarianInboxBranch, outcome.Record, envelope.InboxRecord(),
			"inbox: "+outcome.Item); err != nil {
			return outcome, fmt.Errorf("record %s on %s: %w", outcome.Record, LibrarianInboxBranch, err)
		}
	}

	if err := in.Ledger.Enqueue(ledgerCtx, in.Repo, outcome.Item, Librarian); err != nil {
		var refusal *ledger.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonExists {
			return outcome, fmt.Errorf("enqueue %s: %w", outcome.Item, err)
		}
		if existing, err = in.Ledger.Item(ledgerCtx, in.Repo, outcome.Item); err != nil {
			return outcome, fmt.Errorf("read existing item %s: %w", outcome.Item, err)
		}
		return outcome, in.checkLibrarian(existing)
	}
	outcome.Added = true

	runner := Runner{Envoy: in.Envoy, Journal: in.Journal}
	for _, notice := range Notices(in.Routing.Topics(Route{Step: RouteConcept}), NoticeFields{
		Type: "concept", From: envelope.Proposer, To: Librarian, Item: outcome.Item, Record: outcome.Record, Commit: outcome.Commit,
	}) {
		publishErr := in.NoListener
		if in.Envoy != nil {
			publishErr = runner.publish(ctx, notice)
		}
		if publishErr == nil {
			continue
		}
		if err := runner.journal(notice, publishErr); err != nil {
			return outcome, err
		}
		outcome.Undelivered = append(outcome.Undelivered, notice)
	}
	return outcome, nil
}

func (in LibrarianIntake) checkLibrarian(item ledger.Item) error {
	if item.Owner != Librarian {
		return fmt.Errorf("%w: %s is in the queue of %s, not of %s", ErrIDOwner, item.ID, item.Owner, Librarian)
	}
	return nil
}
