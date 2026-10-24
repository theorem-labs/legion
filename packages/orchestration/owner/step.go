package owner

import (
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

// MissingEnvError names a launch variable an owner process needs and does not have.
type MissingEnvError struct{ Name string }

func (e *MissingEnvError) Error() string { return "missing " + e.Name }

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

// Step writes one record, queues its consequences, reports them, and answers at most one work item.
type Step struct {
	// Answers is the item the step answers, or "" for a step that answers none (a need, a spec revision with no
	// open item): Run then writes the record, queues the follow-ups and publishes the notices, and reads, claims
	// and completes no item.
	Answers   string
	Path      string
	Data      []byte
	Subject   string
	FollowUps []FollowUp
	Notices   []Notice
	Result    string // "" means the record's commit
}

// Runner executes owner steps against one worktree, ledger repository, and Envoy publisher.
type Runner struct {
	Ledger  Ledger
	Tree    Worktree
	Envoy   Publisher
	Owner   string // $LEGION_OWNER
	Repo    string // $LEGION_LEDGER_REPOSITORY
	Journal string // absolute: the deployment's <state_dir>/undelivered.jsonl
}

// Outcome records the durable result of a step.
type Outcome struct {
	AlreadyDone    bool
	Commit, Result string
	Undelivered    []string
}

// Run completes a step in the protocol order. Repeating it after any failure reuses the record,
// idempotent follow-ups, notice keys, and the already-completed answer.
func (r Runner) Run(ctx context.Context, s Step) (Outcome, error) {
	var generation int64
	trailers := map[string][]string{}
	if s.Answers != "" {
		item, err := r.Ledger.Item(ctx, r.Repo, s.Answers)
		if err != nil {
			return Outcome{}, fmt.Errorf("read answer item %q: %w", s.Answers, err)
		}
		if item.State == "done" {
			return Outcome{AlreadyDone: true, Result: item.Result}, nil
		}
		if generation, err = claimOrAdopt(ctx, r.Ledger, r.Repo, r.Owner, s.Answers, item); err != nil {
			return Outcome{}, err
		}
		trailers["Owner-Item"] = []string{s.Answers}
	}

	commit, err := r.Tree.CommitFile(ctx, s.Path, s.Data, s.Subject, trailers)
	if err != nil {
		return Outcome{}, fmt.Errorf("commit record %s: %w", s.Path, err)
	}
	commit, err = r.Tree.LastCommitTouching(s.Path)
	if err != nil {
		return Outcome{}, fmt.Errorf("find the commit of record %s: %w", s.Path, err)
	}
	if commit == "" {
		return Outcome{}, fmt.Errorf("record %q has no commit", s.Path)
	}
	if err := r.checkAttributed(s, commit); err != nil {
		return Outcome{Commit: commit}, err
	}

	for _, followUp := range s.FollowUps {
		if err := r.enqueue(ctx, followUp); err != nil {
			return Outcome{Commit: commit}, err
		}
	}

	outcome := Outcome{Commit: commit}
	for _, notice := range s.Notices {
		if err := r.publish(ctx, notice); err != nil {
			if journalErr := r.journal(notice, err); journalErr != nil {
				return outcome, journalErr
			}
			outcome.Undelivered = append(outcome.Undelivered, notice.Topic)
		}
	}

	outcome.Result = s.Result
	if outcome.Result == "" {
		outcome.Result = commit
	}
	if s.Answers == "" {
		return outcome, nil
	}
	if err := r.Ledger.Complete(ctx, r.Repo, s.Answers, r.Owner, generation, outcome.Result); err != nil {
		return outcome, fmt.Errorf("complete answer item %q: %w", s.Answers, err)
	}
	return outcome, nil
}

// checkAttributed refuses to answer an item with a record commit that does not carry the item's Owner-Item
// trailer: bytes that another step committed are no proof that this step answered it.
func (r Runner) checkAttributed(s Step, commit string) error {
	if s.Answers == "" {
		return nil
	}
	trailers, err := r.Tree.Repo.CommitTrailers(commit)
	if err != nil {
		return fmt.Errorf("read the trailers of record commit %s: %w", commit, err)
	}
	if !slices.Contains(trailers["Owner-Item"], s.Answers) {
		return fmt.Errorf("%w: %s was last committed at %s, which does not carry Owner-Item: %s", ErrUnattributed, s.Path, commit, s.Answers)
	}
	return nil
}

// claimOrAdopt takes the lease a step needs on item: a live lease of owner is its own earlier process, whose release
// the supervisor has not sent yet, so its generation is adopted; any other item is claimed, which refuses another
// holder's live lease.
func claimOrAdopt(ctx context.Context, l Ledger, repo, owner, id string, item ledger.Item) (int64, error) {
	if item.Holder == owner && leaseLive(item) {
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
	if item.Holder == owner && leaseLive(item) {
		return item.Generation, nil
	}
	return 0, fmt.Errorf("claim answer item %q: %w", id, err)
}

func leaseLive(item ledger.Item) bool {
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

func (r Runner) publish(ctx context.Context, notice Notice) error {
	if strings.TrimSpace(notice.Key) == "" {
		return fmt.Errorf("publish notice on %q: empty idempotency key", notice.Topic)
	}

	var err error
	for attempt := 0; attempt <= len(noticeRetryDelays); attempt++ {
		err = r.Envoy.Publish(ctx, notice.Topic, notice.Message, notice.Payload, notice.Key)
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

func (r Runner) journal(notice Notice, publishErr error) error {
	path := r.Journal
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

// OpenLedger opens the ledger of an owner process from LEGION_LEDGER_DSN_FILE, the launch variable of C8: the one
// source of the ledger for every owner command.
func OpenLedger(ctx context.Context, env func(string) string) (*ledger.Store, error) {
	dsnFile, err := requiredEnv(env, "LEGION_LEDGER_DSN_FILE")
	if err != nil {
		return nil, err
	}
	dsn, err := os.ReadFile(dsnFile)
	if err != nil {
		return nil, fmt.Errorf("read LEGION_LEDGER_DSN_FILE %q: %w", dsnFile, err)
	}
	if strings.TrimSpace(string(dsn)) == "" {
		return nil, fmt.Errorf("LEGION_LEDGER_DSN_FILE %q is empty", dsnFile)
	}
	store, err := ledger.Open(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return nil, fmt.Errorf("open ledger from LEGION_LEDGER_DSN_FILE: %w", err)
	}
	return store, nil
}

// RunnerFromEnv is the one way to build an owner process's step runner: the owner and the ledger repository from
// its launch variables LEGION_OWNER and LEGION_LEDGER_REPOSITORY (C8), the ledger l (OpenLedger's), the owner's
// worktree, the journal of undelivered notices in the deployment's state_dir, and the publisher of the listener
// that ResolveListener names. A listener configuration that cannot be resolved is ErrListenerConfig.
func RunnerFromEnv(env func(string) string, l Ledger, tree Worktree, stateDir string) (Runner, error) {
	owner, err := requiredEnv(env, "LEGION_OWNER")
	if err != nil {
		return Runner{}, err
	}
	repo, err := requiredEnv(env, "LEGION_LEDGER_REPOSITORY")
	if err != nil {
		return Runner{}, err
	}
	if !filepath.IsAbs(stateDir) {
		return Runner{}, fmt.Errorf("the deployment's state_dir %q is not an absolute path", stateDir)
	}
	url, token, err := ResolveListener(env)
	if err != nil {
		return Runner{}, fmt.Errorf("%w: %v", ErrListenerConfig, err)
	}
	return Runner{
		Ledger:  l,
		Tree:    tree,
		Envoy:   NewHTTPPublisher(url, token),
		Owner:   owner,
		Repo:    repo,
		Journal: filepath.Join(stateDir, "undelivered.jsonl"),
	}, nil
}

func requiredEnv(env func(string) string, name string) (string, error) {
	value := ""
	if env != nil {
		value = strings.TrimSpace(env(name))
	}
	if value == "" {
		return "", &MissingEnvError{Name: name}
	}
	return value, nil
}
