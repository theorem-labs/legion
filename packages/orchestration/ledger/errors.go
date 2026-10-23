package ledger

import "fmt"

// Refusal reasons. A refusal means the ledger is consistent and the request was declined; the caller decides what
// to do next.
const (
	ReasonOverlap            = "overlap"             // the write set overlaps another open migration's
	ReasonStaleGeneration    = "stale_generation"    // the caller's generation is not the current one
	ReasonNotHolder          = "not_holder"          // the item is held by someone else
	ReasonLeaseHeld          = "lease_held"          // the item has a live lease
	ReasonLeaseExpired       = "lease_expired"       // the caller's lease ended
	ReasonSupervisedLease    = "supervised_lease"    // the lease has no end: its supervisor frees it
	ReasonDone               = "done"                // the item already has a result
	ReasonNotOpen            = "not_open"            // the migration has landed or been abandoned
	ReasonExists             = "exists"              // a migration with that name already exists
	ReasonBelowLimit         = "below_limit"         // the migration has not used up its landing rounds
	ReasonNotEscalated       = "not_escalated"       // retry of a migration that is not escalated
	ReasonImportConflict     = "import_conflict"     // a pilot record disagrees with the ledger's copy
	ReasonNotOwner           = "not_owner"           // the caller does not own the contract
	ReasonStaleRead          = "stale_read"          // a recorded contract hash is no longer current
	ReasonStaleVersion       = "stale_version"       // an agreement did not read the current contract version
	ReasonPinned             = "pinned"              // a person must approve the pinned contract's change
	ReasonEscalated          = "escalated"           // the migration awaits a steward's decision
	ReasonUndeclaredWrite    = "undeclared_write"    // the candidate writes a path outside the migration lock
	ReasonUndeclaredContract = "undeclared_contract" // the migration did not declare the changed contract as a read
)

// Refusal is a declined request.
type Refusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// For an overlap: the other migration and the paths that overlap (ours -> theirs).
	Other    string      `json:"other,omitempty"`
	Overlaps []PathClash `json:"overlaps,omitempty"`
}

func (r *Refusal) Error() string { return fmt.Sprintf("refused (%s): %s", r.Reason, r.Message) }

func refuse(reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// NotFoundError names a repository, item, migration, contract or agreement that does not exist.
type NotFoundError struct {
	What string `json:"what"`
	ID   string `json:"id"`
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("no %s %q", e.What, e.ID) }

// InvalidError is input the ledger cannot accept as given (a bad path, a non-positive lease length).
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return "invalid: " + e.Message }

func invalid(format string, args ...any) *InvalidError {
	return &InvalidError{Message: fmt.Sprintf(format, args...)}
}
