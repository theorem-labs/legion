package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const supervisedLease = 0

var (
	// ErrIDOwner reports a follow-up identifier that has already been assigned to another owner.
	ErrIDOwner = errors.New("work item belongs to another owner")
	// ErrUnattributed reports a record whose commit does not carry the Owner-Item trailer of the item a step
	// answers, so the commit is no proof that the step answered it.
	ErrUnattributed = errors.New("record commit does not carry the answered item's Owner-Item trailer")
	// ErrListenerConfig reports an Envoy listener configuration that cannot be read: the owner process cannot check
	// where its notices go, which is no delivery failure to retry and journal.
	ErrListenerConfig = errors.New("the Envoy listener configuration cannot be read")

	noticeRetryDelays = [...]time.Duration{time.Second, 4 * time.Second, 16 * time.Second}
)

// Ledger is the subset of the work-item ledger a step needs.
type Ledger interface {
	Item(ctx context.Context, repo, id string) (ledger.Item, error)
	Claim(ctx context.Context, repo, id, holder string, ttl time.Duration) (ledger.Lease, error)
	Enqueue(ctx context.Context, repo, id, owner string) error
	Complete(ctx context.Context, repo, id, holder string, gen int64, result string) error
}

// Payload is the structured content of an Envoy notice.
type Payload map[string]any

// FollowUp is one work item a record makes ready for another owner.
type FollowUp struct{ Item, Owner string }

// Notice is one Envoy delivery that reports a committed record.
type Notice struct {
	Topic, Message string
	Payload        Payload
	Key            string
}

// Step is one record mutation of an owner: the record it writes, the items that record answers, and what follows
// from it.
type Step struct {
	// Path is the record's path in the owner's worktree.
	Path string
	// Answers are the items the record answers. Run claims each open one (or adopts its owner's own live lease),
	// commits the record with an Owner-Item trailer for each, and completes each with the record's commit once the
	// follow-ups are queued and the notices sent. A done answer was answered by an earlier record: it gets neither
	// trailer nor completion.
	Answers []string
	// Build makes the record from the bytes HEAD holds at Path (found is false when HEAD holds none). Run calls it
	// once, with the worktree's commit lock held for the whole step, so the revision Build picks from those bytes is
	// the one Run commits, and no other step of the owner writes in between. An error from Build stops the step
	// before it claims or writes anything, and Run returns it as it is.
	Build func(previous []byte, found bool) (Record, error)
}

// Record is what a step's Build makes: the record's bytes and commit subject, the files its commit writes beside it,
// the items it makes ready, and its notices, which Notices builds once the record's commit is known.
type Record struct {
	Data      []byte
	Subject   string
	Also      []File // other files of the record's commit: an escalation beside its reply, an edge beside its accept
	FollowUps []FollowUp
	Notices   func(commit string) []Notice
}

// Runner executes owner steps against one worktree, ledger repository, and Envoy publisher.
type Runner struct {
	Ledger  Ledger
	Tree    Worktree
	Envoy   Publisher
	Owner   string // the owner the command acts for (--owner)
	Repo    string // the ledger repository: skeleton/owner.yaml's repo
	Journal string // absolute: the deployment's <state_dir>/undelivered.jsonl
}

// Outcome records the durable result of a step. AlreadyDone is a step whose every answer was done and whose record
// HEAD held already: the repeat of the step that answered them, which changed nothing.
type Outcome struct {
	AlreadyDone bool
	Commit      string
	Undelivered []string
}

// Run completes a step in the protocol order: the record, its follow-ups, its notices, then the completion of its
// answers. The worktree's commit lock covers what reads or changes the owner's state: reading the answers, building
// the record from HEAD (where its revision is picked), claiming the open answers, the commit and the follow-ups; and,
// taken again, the completion. Two steps of one owner therefore pick their revisions one after the other, and each
// builds from the record the other committed. The notices are fixed once the commit is known, so they are sent,
// retried and journaled between the two with the lock released: another command of the owner does not wait on a
// listener that refuses them. They are sent before the completion, so a step killed in between leaves its answers
// open, and its re-run sends them again. Repeating a step after any failure reuses the record, the idempotent
// follow-ups, the notice keys and the already-completed answers.
func (r Runner) Run(ctx context.Context, s Step) (Outcome, error) {
	var (
		outcome  Outcome
		notices  []Notice
		answered []claimed
	)
	err := r.Tree.locked(ctx, func(git func(args ...string) (string, error)) error {
		var err error
		outcome, notices, answered, err = r.record(ctx, s, git)
		return err
	})
	if err != nil || outcome.AlreadyDone {
		return outcome, err
	}
	undelivered, err := Deliver(ctx, r.Envoy, r.Journal, notices)
	for _, notice := range undelivered {
		outcome.Undelivered = append(outcome.Undelivered, notice.Topic)
	}
	if err != nil || len(answered) == 0 {
		return outcome, err
	}
	err = r.Tree.locked(ctx, func(func(args ...string) (string, error)) error {
		return r.complete(ctx, answered, outcome.Commit)
	})
	return outcome, err
}

// claimed is an answer that a step claimed, or whose lease of its owner it adopted, at generation.
type claimed struct {
	id         string
	generation int64
}

// record is the part of a step under the commit lock before its notices: it reads the answers, builds the record
// from HEAD, claims the open answers, commits, and queues the follow-ups. It returns the notices to send and the
// answers to complete.
func (r Runner) record(ctx context.Context, s Step, git func(args ...string) (string, error)) (Outcome, []Notice, []claimed, error) {
	open, err := r.openAnswers(ctx, s.Answers)
	if err != nil {
		return Outcome{}, nil, nil, err
	}
	previous, found, err := r.Tree.BlobAtHead(s.Path)
	if err != nil {
		return Outcome{}, nil, nil, fmt.Errorf("read record %s: %w", s.Path, err)
	}
	record, err := s.Build(previous, found)
	if err != nil {
		return Outcome{}, nil, nil, err
	}
	if len(s.Answers) > 0 && len(open) == 0 && found && bytes.Equal(previous, record.Data) {
		commit, err := r.Tree.LastCommitTouching(s.Path)
		return Outcome{AlreadyDone: true, Commit: commit}, nil, nil, err
	}

	answered := make([]claimed, 0, len(open))
	ids := make([]string, 0, len(open))
	for _, item := range open {
		generation, err := claimOrAdopt(ctx, r.Ledger, r.Repo, r.Owner, item.ID, item)
		if err != nil {
			return Outcome{}, nil, nil, err
		}
		answered, ids = append(answered, claimed{id: item.ID, generation: generation}), append(ids, item.ID)
	}
	files := append([]File{{Path: s.Path, Data: record.Data}}, record.Also...)
	if err := r.Tree.commitFiles(git, files, record.Subject, map[string][]string{"Owner-Item": ids}); err != nil {
		return Outcome{}, nil, nil, fmt.Errorf("commit record %s: %w", s.Path, err)
	}
	commit, err := r.Tree.LastCommitTouching(s.Path)
	if err != nil {
		return Outcome{}, nil, nil, fmt.Errorf("find the commit of record %s: %w", s.Path, err)
	}
	if commit == "" {
		return Outcome{}, nil, nil, fmt.Errorf("record %q has no commit", s.Path)
	}
	if err := r.checkAttributed(s.Path, ids, commit); err != nil {
		return Outcome{Commit: commit}, nil, nil, err
	}
	for _, followUp := range record.FollowUps {
		if err := r.enqueue(ctx, followUp); err != nil {
			return Outcome{Commit: commit}, nil, nil, err
		}
	}
	var notices []Notice
	if record.Notices != nil {
		notices = record.Notices(commit)
	}
	return Outcome{Commit: commit}, notices, answered, nil
}

// complete completes each answer with the record's commit, under the commit lock. An answer that is done already was
// completed by another command of the owner, which adopted the same lease while this one sent its notices.
func (r Runner) complete(ctx context.Context, answered []claimed, commit string) error {
	for _, answer := range answered {
		item, err := r.Ledger.Item(ctx, r.Repo, answer.id)
		if err != nil {
			return fmt.Errorf("re-read answer item %q: %w", answer.id, err)
		}
		if item.State == "done" {
			continue
		}
		if err := r.Ledger.Complete(ctx, r.Repo, answer.id, r.Owner, answer.generation, commit); err != nil {
			return fmt.Errorf("complete answer item %q: %w", answer.id, err)
		}
	}
	return nil
}

// openAnswers reads each answer and returns the open ones, in order. An open answer that another holder's live
// lease holds is refused here, before the step claims any answer or writes anything.
func (r Runner) openAnswers(ctx context.Context, answers []string) ([]ledger.Item, error) {
	var open []ledger.Item
	for _, id := range answers {
		item, err := r.Ledger.Item(ctx, r.Repo, id)
		if err != nil {
			return nil, fmt.Errorf("read answer item %q: %w", id, err)
		}
		if item.State == "done" {
			continue
		}
		if item.Holder != r.Owner && LeaseLive(item) {
			return nil, fmt.Errorf("answer item %q: %w", id, &ledger.Refusal{
				Reason: ledger.ReasonLeaseHeld, Message: fmt.Sprintf("item %s is held by %s", id, item.Holder),
			})
		}
		item.ID = id
		open = append(open, item)
	}
	return open, nil
}

// checkAttributed refuses to answer items with a record commit that does not carry each one's Owner-Item trailer:
// bytes that another step committed are no proof that this step answered them.
func (r Runner) checkAttributed(path string, answered []string, commit string) error {
	if len(answered) == 0 {
		return nil
	}
	trailers, err := r.Tree.Repo.CommitTrailers(commit)
	if err != nil {
		return fmt.Errorf("read the trailers of record commit %s: %w", commit, err)
	}
	for _, id := range answered {
		if !slices.Contains(trailers["Owner-Item"], id) {
			return fmt.Errorf("%w: %s was last committed at %s, which does not carry Owner-Item: %s", ErrUnattributed, path, commit, id)
		}
	}
	return nil
}

// claimOrAdopt takes the lease a step needs on item: a live lease of owner is its own earlier command's, which a killed
// command left held, so its generation is adopted; any other item is claimed, which refuses another holder's live
// lease.
func claimOrAdopt(ctx context.Context, l Ledger, repo, owner, id string, item ledger.Item) (int64, error) {
	if item.Holder == owner && LeaseLive(item) {
		return item.Generation, nil
	}

	lease, err := l.Claim(ctx, repo, id, owner, supervisedLease)
	if err == nil {
		return lease.Generation, nil
	}
	var refusal *ledger.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonLeaseHeld {
		return 0, fmt.Errorf("claim answer item %q: %w", id, err)
	}

	item, itemErr := l.Item(ctx, repo, id)
	if itemErr != nil {
		return 0, fmt.Errorf("re-read answer item %q after held lease: %w", id, itemErr)
	}
	if item.Holder == owner && LeaseLive(item) {
		return item.Generation, nil
	}
	return 0, fmt.Errorf("claim answer item %q: %w", id, err)
}

// LeaseLive reports whether item is held by a lease that has not expired: a holder, and no end time or one still to
// come. The step runner adopts its owner's own live lease and refuses anyone else's.
func LeaseLive(item ledger.Item) bool {
	return item.Holder != "" && (item.ExpiresAt == nil || item.ExpiresAt.After(time.Now()))
}

// Supersede completes owner's item as superseded:<version>, with no record: the step of an item older than the
// version on the branch or on main. Its caller has checked that version exists. It takes the lease as Run does,
// and an item that is done already is left as it is.
func Supersede(ctx context.Context, l Ledger, repo, owner, id, version string) error {
	item, err := l.Item(ctx, repo, id)
	if err != nil {
		return fmt.Errorf("read item %q: %w", id, err)
	}
	if item.Owner != owner {
		return fmt.Errorf("%w: %q is in the queue of %q, not %q", ErrIDOwner, id, item.Owner, owner)
	}
	if item.State == "done" {
		return nil
	}
	generation, err := claimOrAdopt(ctx, l, repo, owner, id, item)
	if err != nil {
		return err
	}
	if err := l.Complete(ctx, repo, id, owner, generation, "superseded:"+version); err != nil {
		return fmt.Errorf("complete item %q: %w", id, err)
	}
	return nil
}

func (r Runner) enqueue(ctx context.Context, followUp FollowUp) error {
	if err := r.Ledger.Enqueue(ctx, r.Repo, followUp.Item, followUp.Owner); err == nil {
		return nil
	} else {
		var refusal *ledger.Refusal
		if !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonExists {
			return fmt.Errorf("enqueue follow-up %q: %w", followUp.Item, err)
		}
	}

	existing, err := r.Ledger.Item(ctx, r.Repo, followUp.Item)
	if err != nil {
		return fmt.Errorf("read existing follow-up %q: %w", followUp.Item, err)
	}
	if existing.Owner != followUp.Owner {
		return fmt.Errorf("%w: %q is assigned to %q, not %q", ErrIDOwner, followUp.Item, existing.Owner, followUp.Owner)
	}
	return nil
}

// Deliver sends each notice through envoy, retrying a refused one at 1 s, 4 s and 16 s, and appends each one that
// envoy still refuses to the journal of undelivered notices at journal: an absolute path, a deployment's
// <state_dir>/undelivered.jsonl. It returns the notices envoy refused, in order. Its error is a journal it could not
// write, and the notices it returns then end with the one that journal lost.
func Deliver(ctx context.Context, envoy Publisher, journal string, notices []Notice) ([]Notice, error) {
	var undelivered []Notice
	for _, notice := range notices {
		publishErr := publish(ctx, envoy, notice)
		if publishErr == nil {
			continue
		}
		undelivered = append(undelivered, notice)
		if err := appendJournal(journal, notice, publishErr); err != nil {
			return undelivered, err
		}
	}
	return undelivered, nil
}

func publish(ctx context.Context, envoy Publisher, notice Notice) error {
	if strings.TrimSpace(notice.Key) == "" {
		return fmt.Errorf("publish notice on %q: empty idempotency key", notice.Topic)
	}

	var err error
	for attempt := 0; attempt <= len(noticeRetryDelays); attempt++ {
		err = envoy.Publish(ctx, notice.Topic, notice.Message, notice.Payload, notice.Key)
		if err == nil {
			return nil
		}
		if attempt == len(noticeRetryDelays) {
			break
		}
		if err := wait(ctx, noticeRetryDelays[attempt]); err != nil {
			return fmt.Errorf("retry notice on %q: %w", notice.Topic, err)
		}
	}
	return fmt.Errorf("publish notice on %q after %d attempts: %w", notice.Topic, len(noticeRetryDelays)+1, err)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func appendJournal(path string, notice Notice, publishErr error) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the notice journal %q is not an absolute path: it is the deployment's <state_dir>/undelivered.jsonl", path)
	}
	entry, err := json.Marshal(struct {
		Topic   string  `json:"topic"`
		Message string  `json:"message"`
		Payload Payload `json:"payload"`
		Key     string  `json:"key"`
		Error   string  `json:"error"`
	}{
		Topic: notice.Topic, Message: notice.Message, Payload: notice.Payload, Key: notice.Key, Error: publishErr.Error(),
	})
	if err != nil {
		return fmt.Errorf("encode undelivered notice on %q: %w", notice.Topic, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make notice journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open notice journal: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(entry, '\n')); err != nil {
		return fmt.Errorf("write notice journal: %w", err)
	}
	return nil
}

// OpenLedgerFile opens the ledger whose DSN the file dsnFile holds. source names where dsnFile came from (a flag, or
// the deployment's ledger_dsn_file), so an error says which setting to fix.
func OpenLedgerFile(ctx context.Context, source, dsnFile string) (*ledger.Store, error) {
	dsn, err := os.ReadFile(dsnFile)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", source, dsnFile, err)
	}
	if strings.TrimSpace(string(dsn)) == "" {
		return nil, fmt.Errorf("%s %q is empty", source, dsnFile)
	}
	store, err := ledger.Open(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return nil, fmt.Errorf("open ledger from %s: %w", source, err)
	}
	return store, nil
}

// NewRunner is the one way to build an owner's step runner: for owner, in ledger repository repo, with the ledger l,
// the journal of undelivered notices in the deployment's state_dir, and the publisher of the listener that
// ResolveListener names from getenv. A listener configuration that cannot be resolved is ErrListenerConfig. It writes
// nothing, so a command builds its runner before it creates the owner's worktree, and then sets the runner's Tree to
// that worktree.
func NewRunner(owner, repo string, l Ledger, stateDir string, getenv func(string) string) (Runner, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return Runner{}, fmt.Errorf("a step runner needs an owner and a ledger repository, not %q and %q", owner, repo)
	}
	if !filepath.IsAbs(stateDir) {
		return Runner{}, fmt.Errorf("the deployment's state_dir %q is not an absolute path", stateDir)
	}
	url, token, err := ResolveListener(getenv)
	if err != nil {
		return Runner{}, fmt.Errorf("%w: %v", ErrListenerConfig, err)
	}
	return Runner{
		Ledger:  l,
		Envoy:   NewHTTPPublisher(url, token),
		Owner:   owner,
		Repo:    repo,
		Journal: filepath.Join(stateDir, "undelivered.jsonl"),
	}, nil
}
