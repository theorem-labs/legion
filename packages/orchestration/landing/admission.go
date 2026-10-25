package landing

import (
	"context"
	"fmt"
	"strings"
)

// Admission states of a batch in the ledger, as the batch store reads them.
const (
	// NotAdmitted is a batch whose members the ledger does not hold.
	NotAdmitted = ""
	// Admitted is a batch whose members the ledger fences until it lands or declines it.
	Admitted = "admitted"
	// AdmissionLanded is the admission of a batch that landed.
	AdmissionLanded = "landed"
	// AdmissionDeclined is the admission of a batch that was declined.
	AdmissionDeclined = "declined"
)

// BatchLedger is the batch half of the ledger: the calls that hold a batch's admission from the moment its pull
// request is open until the ledger lands or declines it. Each one writes the batch's record at the version the batch
// carries and replaces it with the version its write produced, so the runner's next save names it.
type BatchLedger interface {
	// Admit admits the batch's members, so the ledger fences everything their checks read, and stores the batch's
	// record.
	Admit(ctx context.Context, batch *Batch, members []AdmitMember) error
	// LandBatch lands every admitted member at GitHub's merged commit, in one transaction, and releases the
	// admission.
	LandBatch(ctx context.Context, batch *Batch, mergedCommit string) error
	// DeclineBatch ends the admission without a landing: it records each return, names the successor that the other
	// members go to, and stores the batch's record.
	DeclineBatch(ctx context.Context, batch *Batch, reason string, returns []Return, successor string) error
	// BeginWithdraw records the withdraw effect of an admitted batch, so a crash during a withdraw is finished after
	// the restart.
	BeginWithdraw(ctx context.Context, repo, batch string) error
	// EndWithdraw records the withdraw effect done, after GitHub removed and closed the pull request. A repeat is a
	// success, so a restart that finds the effect already done finishes nothing twice.
	EndWithdraw(ctx context.Context, repo, batch string) error
	// ReadyMembers lists the repository's ready migrations, each as the member a batch freezes from it. A successor
	// batch forms from them, so a returned member joins one only after its owner readies it again.
	ReadyMembers(ctx context.Context, repo string) ([]Member, error)
}

// AdmitMember is one member of an admission: what the program computed from the candidate. Everything else (the
// generation, the head, the changed contracts, the reviews, the proof, the approvals and the upstream record) comes
// from the ready record that ReadyEvent names, which is the ledger's own copy.
type AdmitMember struct {
	Migration     string            `json:"migration"`
	ReadyEvent    int64             `json:"ready_event"`
	CurrentHashes map[string]string `json:"current_hashes"`
	ChangedPaths  []string          `json:"changed_paths"`
}

// Return sends one member of a declined batch back to its owner. Key is `<batch>/<member>/<generation>`, so a retry
// after a crash replays, and ReadyEvent is the ready record that this batch froze, so the return clears that record
// and never one the owner sent later.
type Return struct {
	Migration  string `json:"migration"`
	Generation int64  `json:"generation"`
	ReadyEvent int64  `json:"ready_event"`
	Reason     string `json:"reason"`
	Key        string `json:"key"`
}

// Refusal is a ledger refusal of a batch call, which the runner routes by its outcome.
type Refusal struct {
	// Reason is the ledger's refusal reason.
	Reason string
	// Message is the ledger's message.
	Message string
	// Member is the member the refusal names, and empty for a refusal of the batch.
	Member string
	// Violations are the kinds that a member's Check reported, when the refusal carries them.
	Violations []string
}

func (r *Refusal) Error() string {
	if r.Member != "" {
		return fmt.Sprintf("refused (%s): member %s: %s", r.Reason, r.Member, r.Message)
	}
	return fmt.Sprintf("refused (%s): %s", r.Reason, r.Message)
}

// batchConflictReasons are the refusals that say another runner has admitted or changed this durable batch, or that
// another admitted batch fences one of its members or contracts. The runner drops nothing and declines nothing for
// them: it reloads and continues from the stored state.
var batchConflictReasons = []string{"batch_admitted", "stale_batch"}

// memberRefusalReasons are the refusals that name a member: every refusal reason of the full Check, which mirrors
// Land's, and admission's own reasons. The named member is dropped.
var memberRefusalReasons = []string{
	"invalid_candidate", "not_open", "stale_generation", "escalated", "undeclared_write", "stale_read",
	"unverified_read", "overlap", "undeclared_contract", "pinned", "statement_unreviewed", "upstream_not_allowed",
	"not_ready", "ready_changed", "batch_conflict",
}

// BatchConflict reports whether the refusal is another runner's or another batch's claim on this one.
func (r *Refusal) BatchConflict() bool { return contains(batchConflictReasons, r.Reason) }

// MemberRefusal reports whether the refusal names a member that the runner drops.
func (r *Refusal) MemberRefusal() bool {
	return r.Member != "" && contains(memberRefusalReasons, r.Reason)
}

// Unclassified reports whether the refusal is neither of the two: the runner stops and leaves the batch for an
// operator.
func (r *Refusal) Unclassified() bool { return !r.BatchConflict() && !r.MemberRefusal() }

// admissionMembers is the admission request's member list, built from the active members of the candidate that the
// program checked: each member's frozen ready event, the current hash of each read it computed, and its changed
// paths.
func admissionMembers(members []Member) []AdmitMember {
	admitted := make([]AdmitMember, 0, len(members))
	for _, member := range members {
		admitted = append(admitted, AdmitMember{
			Migration:     member.Migration,
			ReadyEvent:    member.ReadyEvent,
			CurrentHashes: cloneHashes(member.CurrentHashes),
			ChangedPaths:  append([]string(nil), member.Changed...),
		})
	}
	return admitted
}

// returnKey is decision 6's idempotency key of a member's return: a member leaves one batch at most once at one
// generation, so a retry after a crash replays.
func returnKey(batch, migration string, generation int64) string {
	return fmt.Sprintf("%s/%s/%d", batch, migration, generation)
}

// droppedReason is the reason a drop records, with the ledger's violation kinds when its refusal carried them.
func droppedReason(reason string, violations []string) string {
	if len(violations) == 0 {
		return reason
	}
	return reason + ": " + strings.Join(violations, ", ")
}
