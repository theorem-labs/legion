package landing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PullRequestState is GitHub's final or in-progress view of a pull request.
type PullRequestState string

const (
	PullRequestOpen    PullRequestState = "open"
	PullRequestMerged  PullRequestState = "merged"
	PullRequestRemoved PullRequestState = "removed"
	PullRequestClosed  PullRequestState = "closed"
)

// PullRequest identifies the candidate pull request and its observed outcome.
type PullRequest struct {
	Number       int
	Head         string
	MergedCommit string
	State        PullRequestState
	Reason       string
}

// GitHub is the merge-queue surface the publisher needs.
type GitHub interface {
	Push(context.Context, string, string, string) error
	FindPullRequest(context.Context, string, string) (PullRequest, bool, error)
	Open(context.Context, string, string, string, string) (PullRequest, error)
	Enqueue(context.Context, string, int) error
	Wait(context.Context, string, int, string) (PullRequest, error)
	// Withdraw takes the pull request out of the merge queue and closes it. A pull request that is already closed
	// or merged is left as it is.
	Withdraw(context.Context, string, int) error
	// Read reads the pull request once, without waiting for an outcome.
	Read(context.Context, string, int) (PullRequest, error)
}

// LandingEvent is emitted once per member after its merge is durably recorded.
// Flags are the member's flags from the receipt's check verdicts.
type LandingEvent struct {
	Batch  string
	Member string
	Commit string
	Flags  []Flag
}

// EventPublisher publishes post-landing member events.
type EventPublisher interface {
	Publish(context.Context, LandingEvent) error
}

// StopError stops the runner with the batch left for an operator: the ledger refused a batch call with a reason the
// runner has no rule for.
type StopError struct {
	Batch  string
	Reason string
	Cause  error
}

func (e *StopError) Error() string {
	return fmt.Sprintf("batch %q stopped (%s): %v", e.Batch, e.Reason, e.Cause)
}

func (e *StopError) Unwrap() error { return e.Cause }

// ErrEffectsPending says that a landed batch has a post-landing effect still to do; a later round finishes it.
var ErrEffectsPending = errors.New("post-landing effects are still to do")

// Publish puts a green candidate through the merge queue under the ledger's admission. Before its pull request is
// enqueued, a batch whose derived values are not at the current key (a new checking policy) is advanced again. The
// push, the find and the open come first; a failure in any of them is Failed, with nothing admitted. Then the ledger
// admits the batch, which fences everything its members' checks read, and only then is the pull request enqueued.
// From admission on, the batch ends only through LandBatch, DeclineBatch or LandedUnrecorded, also after a restart:
// an enqueue or wait that fails is retried up to the limit and then withdrawn; GitHub's merge of the candidate lands
// the batch; a merge of anything else is LandedUnrecorded; a pull request closed or removed unmerged declines it.
func (r Runner) Publish(ctx context.Context, batch Batch) (Batch, error) {
	if r.Store == nil || r.Admission == nil || r.GitHub == nil {
		return Batch{}, fmt.Errorf("store, batch ledger, and GitHub are required to publish")
	}
	if batch.State.terminal() {
		return batch, nil
	}
	if batch.State != Green && batch.State != Publishing {
		return Batch{}, fmt.Errorf("batch %q is %q, not publishable", batch.ID, batch.State)
	}
	if batch.Receipt == nil {
		return Batch{}, fmt.Errorf("batch %q has no receipt", batch.ID)
	}
	if batch.BaseBranch == "" {
		return Batch{}, fmt.Errorf("batch %q has no base branch", batch.ID)
	}
	if batch.Admission == Admitted {
		return r.publishAdmitted(ctx, &batch)
	}
	if !r.fresh(batch) {
		if err := r.valid(); err != nil {
			return Batch{}, err
		}
		advanced, err := r.advance(ctx, &batch)
		if err != nil || advanced.State != Green {
			return advanced, err
		}
		batch = advanced
	}
	if batch.Branch == "" {
		batch.Branch = "land/" + batch.ID
	}
	if batch.State != Publishing {
		if err := r.transition(ctx, &batch, Publishing, ""); err != nil {
			return Batch{}, err
		}
	}
	if err := r.GitHub.Push(ctx, batch.Repository, batch.Branch, batch.Candidate.Commit); err != nil {
		return r.fail(ctx, &batch, "github: push")
	}
	pull, found, err := r.GitHub.FindPullRequest(ctx, batch.Repository, batch.Branch)
	if err != nil {
		return r.fail(ctx, &batch, "github: find")
	}
	if !found {
		body, err := batch.Receipt.JSON()
		if err != nil {
			return r.fail(ctx, &batch, "receipt")
		}
		pull, err = r.GitHub.Open(ctx, batch.Repository, batch.BaseBranch, batch.Branch, string(body))
		if err != nil {
			return r.fail(ctx, &batch, "github: open")
		}
	}
	batch.PullRequest = pull
	batch.EscalateTo = r.EscalateTo
	// The hook definitions are frozen here, in the record the ledger stores with the admission, so a configuration
	// change after it (a reorder of hooks or parts, another image, score ref or escalations owner) changes nothing
	// for this batch.
	batch.PostLandEffects = r.postLandEffects(batch)
	if err := r.Admission.Admit(ctx, &batch, admissionMembers(batch.ActiveMembers)); err != nil {
		return r.admissionRefused(ctx, &batch, err)
	}
	return r.publishAdmitted(ctx, &batch)
}

// admissionRefused routes a refused admission, which recorded nothing, by its outcome: a member refusal drops the
// member it names; a batch conflict drops and declines nothing and continues from the stored batch; any other
// refusal stops the runner and leaves the batch for an operator.
func (r Runner) admissionRefused(ctx context.Context, batch *Batch, err error) (Batch, error) {
	var refusal *LedgerRefusal
	if !errors.As(err, &refusal) {
		return *batch, fmt.Errorf("admit batch %q: %w", batch.ID, err)
	}
	switch {
	case refusal.MemberRefusal():
		if err := r.dropMembers(ctx, batch, map[string]string{
			refusal.Member: droppedReason(refusal.Reason, refusal.Violations),
		}); err != nil && !r.reloaded(ctx, batch, err) {
			return *batch, err
		}
		return *batch, nil
	case refusal.BatchConflict():
		if loaded, found, loadErr := r.Store.Load(ctx, batch.ID); loadErr != nil {
			return *batch, fmt.Errorf("reload batch %q: %w", batch.ID, loadErr)
		} else if found {
			*batch = loaded
		}
		return *batch, nil
	default:
		return *batch, &StopError{Batch: batch.ID, Reason: "unclassified_refusal", Cause: err}
	}
}

// publishAdmitted takes an admitted batch to its end from its durable state. A withdraw that was started is
// finished first; then the pull request is enqueued and awaited, with each failure counted in the batch, until the
// limit starts a withdraw.
func (r Runner) publishAdmitted(ctx context.Context, batch *Batch) (Batch, error) {
	if batch.Withdraw != "" {
		return r.withdraw(ctx, batch)
	}
	number := batch.PullRequest.Number
	if err := r.GitHub.Enqueue(ctx, batch.Repository, number); err != nil {
		return r.publishFailed(ctx, batch, "github: enqueue", err)
	}
	pull, err := r.GitHub.Wait(ctx, batch.Repository, number, batch.Candidate.Commit)
	if err != nil {
		return r.publishFailed(ctx, batch, "github: wait", err)
	}
	return r.recordMerge(ctx, batch, pull)
}

// publishFailed counts a failed enqueue or wait. Below the limit the batch stays admitted in Publishing, and a later
// round tries again after the backoff; at the limit the runner withdraws the pull request.
func (r Runner) publishFailed(ctx context.Context, batch *Batch, step string, cause error) (Batch, error) {
	batch.PublishAttempts++
	if batch.PublishAttempts >= r.publishingRetryLimit() {
		batch.Withdraw = step
		if err := r.Store.Save(ctx, batch); err != nil {
			return *batch, fmt.Errorf("record withdraw of %q: %w", batch.ID, err)
		}
		return r.withdraw(ctx, batch)
	}
	batch.Reason = step
	if err := r.Store.Save(ctx, batch); err != nil {
		return *batch, fmt.Errorf("record %s failure: %w", step, err)
	}
	r.pause(ctx, batch.PublishAttempts)
	return *batch, fmt.Errorf("%s: %w", step, cause)
}

// withdraw records the withdraw effect, takes the pull request out of the queue and closes it, records the effect
// done and reads the pull request again. A merge during the withdraw lands the batch; a closed, unmerged pull request
// declines it with no returns. While GitHub cannot be reached, the batch stays admitted and the withdraw is retried:
// it never declines while the pull request could still merge.
func (r Runner) withdraw(ctx context.Context, batch *Batch) (Batch, error) {
	number := batch.PullRequest.Number
	if err := r.Admission.BeginWithdraw(ctx, batch.Repository, batch.ID); err != nil {
		return *batch, fmt.Errorf("begin withdraw of %q: %w", batch.ID, err)
	}
	if err := r.GitHub.Withdraw(ctx, batch.Repository, number); err != nil {
		return *batch, fmt.Errorf("withdraw %q: %w", batch.ID, err)
	}
	pull, err := r.GitHub.Read(ctx, batch.Repository, number)
	if err != nil {
		return *batch, fmt.Errorf("read withdrawn %q: %w", batch.ID, err)
	}
	if pull.State == PullRequestOpen {
		return *batch, fmt.Errorf("withdraw %q: the pull request is still open", batch.ID)
	}
	if err := r.Admission.EndWithdraw(ctx, batch.Repository, batch.ID); err != nil {
		return *batch, fmt.Errorf("end withdraw of %q: %w", batch.ID, err)
	}
	if pull.State == PullRequestMerged {
		return r.recordMerge(ctx, batch, pull)
	}
	return r.decline(ctx, batch, "publish_failed: "+batch.Withdraw)
}

// recordMerge ends an admitted batch from GitHub's final view of its pull request. Only a merge of the exact candidate
// commit with a merged commit lands it. A merge of anything else is on the base branch and cannot be declined, so it
// is LandedUnrecorded. A pull request closed or removed unmerged declines the batch with no returns.
func (r Runner) recordMerge(ctx context.Context, batch *Batch, pull PullRequest) (Batch, error) {
	batch.PullRequest = pull
	switch {
	case pull.State == PullRequestMerged && pull.MergedCommit == "":
		return r.landedUnrecorded(ctx, batch, "github: merged commit unavailable")
	case pull.State == PullRequestMerged && pull.Head != batch.Candidate.Commit:
		return r.landedUnrecorded(ctx, batch, "github: merged unexpected head")
	case pull.State == PullRequestMerged:
		return r.land(ctx, batch)
	}
	reason := strings.TrimSpace(pull.Reason)
	if reason == "" {
		reason = string(pull.State)
	}
	return r.decline(ctx, batch, reason)
}

// decline ends the admission with no member blamed: every member goes to the successor.
func (r Runner) decline(ctx context.Context, batch *Batch, reason string) (Batch, error) {
	if err := r.declineBatch(ctx, batch, reason, nil, nil); err != nil {
		if r.reloaded(ctx, batch, err) {
			return *batch, nil
		}
		return *batch, err
	}
	return *batch, nil
}

// land lands every member at GitHub's merged commit in one ledger transaction, which also records each effect that
// admission froze as an intent, and then does those effects. A transport error leaves the batch admitted for a
// later round; a ledger refusal is LandedUnrecorded with that refusal, and the runner stops retrying.
func (r Runner) land(ctx context.Context, batch *Batch) (Batch, error) {
	record := cloneBatch(*batch)
	record.State, record.Reason = Landed, ""
	if err := r.Admission.LandBatch(ctx, &record, batch.PullRequest.MergedCommit); err != nil {
		var refusal *LedgerRefusal
		if errors.As(err, &refusal) {
			return r.landedUnrecorded(ctx, batch, "ledger: "+refusal.Error())
		}
		return *batch, fmt.Errorf("land batch %q: %w", batch.ID, err)
	}
	*batch = record
	if err := r.finishEffects(ctx, batch); err != nil {
		return *batch, err
	}
	return *batch, nil
}

// landedUnrecorded ends a batch that GitHub merged and the ledger did not land. The ledger escalates it to the
// escalations owner in the same transaction, and the fence stays until an operator resolves it.
func (r Runner) landedUnrecorded(ctx context.Context, batch *Batch, reason string) (Batch, error) {
	batch.EscalateTo = r.EscalateTo
	if err := r.transition(ctx, batch, LandedUnrecorded, reason); err != nil {
		return *batch, err
	}
	return *batch, nil
}

// postLandEffects are the effects of a landing, frozen at admission: one event per member, when events are
// published, and one hook effect for each (hook, part) of the configuration.
func (r Runner) postLandEffects(batch Batch) []PostLandEffect {
	effects := []PostLandEffect{}
	if r.Events != nil {
		for _, member := range batch.ActiveMembers {
			effects = append(effects, PostLandEffect{Key: "event/" + member.Migration, Op: OpEvent, Member: member.Migration})
		}
	}
	return append(effects, r.hookEffects(batch)...)
}

func (r Runner) publishingRetryLimit() int {
	if r.PublishingRetryLimit > 0 {
		return r.PublishingRetryLimit
	}
	return 10
}

// pause waits the backoff after the attempt-th failed publishing step, doubling from PublishingBackoff, or until ctx
// ends.
func (r Runner) pause(ctx context.Context, attempt int) {
	backoff := r.PublishingBackoff
	if backoff <= 0 {
		backoff = 30 * time.Second
	}
	for range attempt - 1 {
		if backoff >= time.Hour {
			break
		}
		backoff *= 2
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// memberFlags is a member's flags in the receipt's check verdicts, never nil.
func memberFlags(receipt *Receipt, member string) []Flag {
	flags := []Flag{}
	if receipt == nil {
		return flags
	}
	for _, verdict := range receipt.Checks {
		for _, flag := range verdict.Flags {
			if flag.Member == member {
				flags = append(flags, flag)
			}
		}
	}
	return flags
}

func operationComplete(ctx context.Context, dependency any, operation Operation) (bool, error) {
	if operation.State == OperationComplete {
		return true, nil
	}
	status, supported := dependency.(OperationStatus)
	if !supported {
		return false, nil
	}
	return status.OperationComplete(ctx, operation)
}

func batchMemberNames(members []Member) []string {
	names := make([]string, len(members))
	for index, member := range members {
		names[index] = member.Migration
	}
	return names
}
