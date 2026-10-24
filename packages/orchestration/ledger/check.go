package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Check outcomes classify a complete pass, a refusal, or a pre-build check with deferred reads.
const (
	CheckOutcomePass       = "pass"
	CheckOutcomeRefused    = "refused"
	CheckOutcomeIncomplete = "incomplete"
)

// StatementRecords are the records a candidate carries for its statement change, the same in a ready record, a check
// and a landing: the changed contracts with their new statement hashes, the head the landing program built, the
// records that clear the change, the pinned contracts a person approved, and an upstream commit as provenance.
type StatementRecords struct {
	ChangedContracts map[string]string `json:"changed_contracts"`
	// Head is the member's branch head that the landing program built. A statement change requires it.
	Head string `json:"head"`
	// CompatibilityProof is the fully qualified name of a declaration that proves that each changed contract gives
	// its callers all that the old one gave.
	CompatibilityProof string `json:"compatibility_proof"`
	// ReviewedHead is the commit at which an agent's review of the change passed.
	ReviewedHead string   `json:"reviewed_head"`
	Reviews      []Review `json:"reviews"`
	// ApprovedPinned names pinned contracts with a person's approval to change.
	ApprovedPinned []string `json:"approved_pinned"`
	Upstream       string   `json:"upstream"`
}

// validate checks the records' shape, which ready, check and land share: a changed contract has a name and a hash, a
// review has all three fields, an approved pinned contract has a name, and no value has leading or trailing
// whitespace.
func (r StatementRecords) validate() error {
	for name, hash := range r.ChangedContracts {
		if err := checkRecord("changed contract name", name, true); err != nil {
			return err
		}
		if err := checkRecord("changed contract hash", hash, true); err != nil {
			return err
		}
	}
	for _, record := range []struct{ label, value string }{
		{"head", r.Head}, {"compatibility proof", r.CompatibilityProof}, {"reviewed head", r.ReviewedHead}, {"upstream", r.Upstream},
	} {
		if err := checkRecord(record.label, record.value, false); err != nil {
			return err
		}
	}
	for _, review := range r.Reviews {
		for _, field := range []struct{ label, value string }{
			{"review contract", review.Contract}, {"reviewer", review.Reviewer}, {"review head", review.Head},
		} {
			if err := checkRecord(field.label, field.value, true); err != nil {
				return err
			}
		}
	}
	for _, name := range r.ApprovedPinned {
		if err := checkRecord("approved pinned name", name, true); err != nil {
			return err
		}
	}
	return nil
}

func checkRecord(label, value string, required bool) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return invalid("%s is empty", label)
	}
	if trimmed != value {
		return invalid("%s %q has leading or trailing whitespace", label, value)
	}
	return nil
}

// withEmptyLists gives absent lists and maps their empty value, so a stored record has one shape whether its request
// came from flags or from JSON.
func (r StatementRecords) withEmptyLists() StatementRecords {
	if r.ChangedContracts == nil {
		r.ChangedContracts = map[string]string{}
	}
	if r.Reviews == nil {
		r.Reviews = []Review{}
	}
	if r.ApprovedPinned == nil {
		r.ApprovedPinned = []string{}
	}
	return r
}

// LandRequest is the candidate that Land validates and closes: a migration at its generation, the commit that lands,
// the paths it changes, the current hash of each declared read, and its statement records.
type LandRequest struct {
	Repo       string `json:"repo"`
	Migration  string `json:"migration"`
	Generation int64  `json:"generation"`
	Commit     string `json:"commit"`
	// ChangedPaths is every path the candidate adds, modifies or deletes, relative to the repository root.
	ChangedPaths []string `json:"changed_paths"`
	// Current maps a read's name to its statement hash in the candidate's base. An empty hash means the statement
	// no longer exists; a name missing from the map was not hashed, so that read cannot be decided.
	Current map[string]string `json:"current"`
	StatementRecords
}

// CheckRequest asks whether a candidate may land. It is the LandRequest that Land would receive, so one request file
// serves both commands.
type CheckRequest struct {
	LandRequest
	// DeferReads omits the comparison of the candidate's read hashes and the landed commit, for a check before the
	// build. It can only produce a refused or an incomplete result.
	DeferReads bool `json:"defer_reads"`
}

// Violation is one reason a candidate must not land. Kind is the refusal reason (a Reason constant) that Land gives for
// it.
type Violation struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Name     string `json:"name,omitempty"`
	Recorded string `json:"recorded,omitempty"`
	Current  string `json:"current,omitempty"`
	Other    string `json:"other,omitempty"`
	Message  string `json:"message"`
}

// CheckResult is the landing decision. Outcome is pass, refused, or incomplete. Pass is true only for pass.
// Deferred lists declared reads whose hashes a deferred check intentionally did not compare.
type CheckResult struct {
	Migration  string      `json:"migration"`
	Generation int64       `json:"generation"`
	Outcome    string      `json:"outcome"`
	Pass       bool        `json:"pass"`
	Deferred   []string    `json:"deferred"`
	Violations []Violation `json:"violations"`
}

// refusalForViolation is the refusal Land gives for a violation: a violation's kind is its refusal reason.
func refusalForViolation(v Violation) error {
	return &Refusal{Reason: v.Kind, Message: v.Message}
}

// cleanCandidate validates a candidate's shape and normalizes its paths before they are compared to declared writes.
// A malformed candidate is invalid input for check and land alike, before either reads the ledger. An absent path
// list stays absent, for the precondition to refuse.
func cleanCandidate(req CheckRequest) (CheckRequest, error) {
	if err := req.validate(); err != nil {
		return CheckRequest{}, err
	}
	if req.ChangedPaths == nil {
		return req, nil
	}
	changed := make([]string, 0, len(req.ChangedPaths))
	for _, path := range req.ChangedPaths {
		cleaned, err := cleanRepoPath(path)
		if err != nil {
			return CheckRequest{}, err
		}
		changed = append(changed, cleaned)
	}
	sort.Strings(changed)
	req.ChangedPaths = changed
	return req, nil
}

// candidatePrecondition reports the candidate metadata that cannot describe a complete landing.
func candidatePrecondition(req CheckRequest) (Violation, bool) {
	var message string
	switch {
	case req.ChangedPaths == nil:
		message = "missing changed paths"
	case len(req.ChangedPaths) == 0 && len(req.ChangedContracts) > 0:
		message = "changed contracts need at least one changed path"
	case len(req.ChangedContracts) > 0 && req.Head == "":
		message = "changed contracts need the head that the landing program built"
	case !req.DeferReads && strings.TrimSpace(req.Commit) == "":
		message = "empty landed commit"
	}
	if message == "" {
		return Violation{}, false
	}
	return Violation{Kind: ReasonInvalidCandidate, Message: message}, true
}

// verdict is the landing decision for a candidate: every reason it must not land, in order, its changed registered
// contracts under the caller's lock, in name order, and the record that clears each one it clears.
type verdict struct {
	violations []Violation
	changed    []Contract
	cleared    map[string]LastChange
}

// evaluate runs every landing rule on a cleaned candidate, for a migration and repository the caller has locked.
// Check and Land both call it, so one candidate gets the same violations, in the same order, from both. lock is the row
// lock it takes on each contract the migration read: shareLock for a check, updateLock for a landing. DeferReads skips
// only the comparison of the caller's read hashes and the landed commit; every rule that needs only the ledger runs.
func evaluate(ctx context.Context, tx pgx.Tx, m Migration, req CheckRequest, lock rowLock) (verdict, error) {
	var v verdict
	if precondition, ok := candidatePrecondition(req); ok {
		v.violations = append(v.violations, precondition)
	}
	violations, err := checkViolations(ctx, tx, m, req)
	if err != nil {
		return verdict{}, err
	}
	v.violations = append(v.violations, violations...)
	contracts, stale, err := readFence(ctx, tx, req.Repo, m, lock)
	if err != nil {
		return verdict{}, err
	}
	v.violations = append(v.violations, stale...)
	reported := make(map[string]bool, len(stale))
	for _, violation := range stale {
		reported[violation.Name] = true
	}
	approved := make(map[string]bool, len(req.ApprovedPinned))
	for _, name := range req.ApprovedPinned {
		approved[name] = true
	}
	for _, name := range sortedNames(req.ChangedContracts) {
		_, declared := m.ReadVersions[name]
		current, read := contracts[name]
		switch {
		case !declared:
			v.violations = append(v.violations, Violation{Kind: ReasonUndeclaredContract, Name: name,
				Message: fmt.Sprintf("migration %s did not declare contract %s as a read", m.ID, name)})
		case reported[name]:
		case !read:
			// The migration declared the read before the contract was registered, and it is not registered now.
			v.violations = append(v.violations, Violation{Kind: ReasonStaleRead, Name: name,
				Message: fmt.Sprintf("migration %s did not record a contract version for %s", m.ID, name)})
		default:
			if current.Pinned && !approved[name] {
				v.violations = append(v.violations, Violation{Kind: ReasonPinned, Name: name,
					Message: fmt.Sprintf("contract %s is pinned; a person must approve its change", name)})
			}
			v.changed = append(v.changed, current)
		}
	}
	v.cleared = clearStatements(v.changed, req.StatementRecords)
	var missing []string
	for _, contract := range v.changed {
		if _, ok := v.cleared[contract.Name]; !ok {
			missing = append(missing, contract.Name)
		}
	}
	if len(missing) > 0 {
		v.violations = append(v.violations, Violation{Kind: ReasonStatementUnreviewed, Name: missing[0],
			Message: unreviewedMessage(v.changed, missing, req.StatementRecords)})
	}
	return v, nil
}

// readFence checks each contract the migration read against the ledger: the contract must still be at the version and
// hash the migration recorded. It needs no build, so it runs for a deferred check too. It returns the contracts it
// read under lock, and one stale_read violation for each read that moved, vanished, or was declared without a
// version although the contract is registered.
func readFence(ctx context.Context, tx pgx.Tx, repo string, m Migration, lock rowLock) (map[string]Contract, []Violation, error) {
	contracts := map[string]Contract{}
	var stale []Violation
	for _, name := range sortedNames(m.ReadVersions) {
		recorded := m.ReadVersions[name]
		current, err := loadContract(ctx, tx, repo, name, lock)
		var missing *NotFoundError
		switch {
		case errors.As(err, &missing):
			if recorded != nil {
				stale = append(stale, Violation{Kind: ReasonStaleRead, Name: name,
					Message: fmt.Sprintf("recorded contract %s no longer exists", name)})
			}
		case err != nil:
			return nil, nil, err
		case recorded == nil:
			stale = append(stale, Violation{Kind: ReasonStaleRead, Name: name,
				Message: fmt.Sprintf("migration %s did not record a version for contract %s", m.ID, name)})
		case current.Version != *recorded || current.StatementHash != m.Reads[name]:
			stale = append(stale, Violation{Kind: ReasonStaleRead, Name: name, Message: fmt.Sprintf(
				"migration %s read contract %s at version %d hash %s, current version is %d hash %s",
				m.ID, name, *recorded, m.Reads[name], current.Version, current.StatementHash)})
		default:
			contracts[name] = current
		}
	}
	return contracts, stale, nil
}

// clearStatements gives the record that clears each changed contract's statement change, in priority order: the
// compatibility proof clears every one; a review that names the contract, by its owner in the ledger, at the head
// clears that one; and a reviewed head equal to the head clears every one, as their owner's review, when they all
// have one owner. A contract with no entry is not cleared. Each record carries the upstream commit as provenance.
func clearStatements(contracts []Contract, r StatementRecords) map[string]LastChange {
	reviewedHead := r.Head != "" && r.ReviewedHead == r.Head && oneOwner(contracts)
	cleared := make(map[string]LastChange, len(contracts))
	for _, contract := range contracts {
		change := LastChange{Upstream: r.Upstream}
		review, reviewed := ownerReview(contract, r)
		switch {
		case r.CompatibilityProof != "":
			change.Proof = r.CompatibilityProof
		case reviewed:
			change.Review = &review
		case reviewedHead:
			change.Review = &Review{Contract: contract.Name, Reviewer: contract.Owner, Head: r.ReviewedHead}
		default:
			continue
		}
		cleared[contract.Name] = change
	}
	return cleared
}

// ownerReview finds a review that names the contract, by its owner, at the candidate's head.
func ownerReview(contract Contract, r StatementRecords) (Review, bool) {
	for _, review := range r.Reviews {
		if review.Contract == contract.Name && review.Reviewer == contract.Owner && review.Head == r.Head {
			return review, true
		}
	}
	return Review{}, false
}

func oneOwner(contracts []Contract) bool {
	for _, contract := range contracts {
		if contract.Owner != contracts[0].Owner {
			return false
		}
	}
	return true
}

// unreviewedMessage explains why the missing contracts' statement changes are not cleared.
func unreviewedMessage(contracts []Contract, missing []string, r StatementRecords) string {
	subject := "contract " + missing[0] + " changes its statement"
	if len(missing) > 1 {
		subject = "contracts " + strings.Join(missing, ", ") + " change their statements"
	}
	at := "the head that lands"
	if r.Head != "" {
		at = r.Head
	}
	review := "no review"
	switch {
	case r.ReviewedHead != "" && r.Head != "" && r.ReviewedHead != r.Head:
		review = "a review that passed at " + r.ReviewedHead + ", not at " + r.Head
	case r.ReviewedHead != "" && r.Head != "" && !oneOwner(contracts):
		review = "a reviewed head at " + r.ReviewedHead + ", but it clears every changed contract only when they have the same owner"
	case r.ReviewedHead != "":
		review = "a review that passed at " + r.ReviewedHead + ", but no head to compare it with"
	}
	return fmt.Sprintf("%s; a statement change lands only with a compatibility proof or an agent's review at %s, "+
		"and this one has no compatibility proof and %s", subject, at, review)
}

// checkViolations evaluates the migration's state, the candidate's paths and its read hashes, for a migration locked
// by the caller.
func checkViolations(ctx context.Context, q querier, m Migration, req CheckRequest) ([]Violation, error) {
	var violations []Violation
	add := func(v Violation) { violations = append(violations, v) }
	if m.State != StateOpen {
		add(Violation{Kind: ReasonNotOpen, Message: fmt.Sprintf("migration %s is %s", m.ID, m.State)})
	}
	if req.Generation != m.Generation {
		add(Violation{Kind: ReasonStaleGeneration, Message: fmt.Sprintf(
			"candidate is from generation %d; migration %s is at generation %d (owner %s)",
			req.Generation, m.ID, m.Generation, m.Owner)})
	}
	if m.EscalatedAt != nil {
		add(Violation{Kind: ReasonEscalated, Message: escalatedMessage(m)})
	}
	ours, err := migrationWrites(ctx, q, req.Repo, req.Migration)
	if err != nil {
		return nil, err
	}
	sets, err := openWriteSets(ctx, q, req.Repo, req.Migration)
	if err != nil {
		return nil, err
	}
	for _, path := range req.ChangedPaths {
		if coveredBy(ours, path) {
			continue
		}
		v := Violation{Kind: ReasonUndeclaredWrite, Path: path,
			Message: fmt.Sprintf("%s is outside migration %s's declared writes", path, m.ID)}
		if other := holderOf(sets, path); other != "" {
			v.Other = other
			v.Message += "; open migration " + other + " declares it"
		}
		add(v)
	}
	if !req.DeferReads {
		for _, name := range sortedNames(m.Reads) {
			recorded := m.Reads[name]
			current, ok := req.Current[name]
			switch {
			case !ok:
				add(Violation{Kind: ReasonUnverifiedRead, Name: name, Recorded: recorded,
					Message: fmt.Sprintf("no current hash for read %s, so it cannot be checked", name)})
			case current == "":
				add(Violation{Kind: ReasonStaleRead, Name: name, Recorded: recorded,
					Message: fmt.Sprintf("read %s no longer exists (recorded hash %s)", name, recorded)})
			case current != recorded:
				add(Violation{Kind: ReasonStaleRead, Name: name, Recorded: recorded, Current: current,
					Message: fmt.Sprintf("read %s changed: recorded %s, now %s", name, recorded, current)})
			}
		}
	}
	if m.State == StateOpen {
		ids := make([]string, 0, len(sets))
		for id := range sets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			for _, clash := range clashes(ours, sets[id]) {
				add(Violation{Kind: ReasonOverlap, Path: clash.Ours, Other: id,
					Message: fmt.Sprintf("declared %s overlaps open migration %s's %s", clash.Ours, id, clash.Theirs)})
			}
		}
	}
	return violations, nil
}

// Check decides whether a candidate change may land for a migration, by the rules Land applies. It writes nothing but
// the event that records its decision. A refusal of the request itself (an unknown migration, a malformed candidate)
// is an error; every reason the candidate must not land is a violation in the result.
func (s *Store) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	cleaned, err := cleanCandidate(req)
	if err != nil {
		return CheckResult{}, err
	}
	var res CheckResult
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// A retried attempt starts from an empty result, so the result holds only what the committed attempt found.
		res = CheckResult{Migration: cleaned.Migration, Generation: cleaned.Generation, Deferred: []string{}, Violations: []Violation{}}
		if _, err := lockRepo(ctx, tx, cleaned.Repo, false); err != nil {
			return err
		}
		migration, err := loadMigration(ctx, tx, cleaned.Repo, cleaned.Migration, true)
		if err != nil {
			return err
		}
		v, err := evaluate(ctx, tx, migration, cleaned, shareLock)
		if err != nil {
			return err
		}
		res.Violations = append(res.Violations, v.violations...)
		if cleaned.DeferReads {
			res.Deferred = sortedNames(migration.Reads)
		}
		switch {
		case len(res.Violations) > 0:
			res.Outcome = CheckOutcomeRefused
		case cleaned.DeferReads:
			res.Outcome = CheckOutcomeIncomplete
		default:
			res.Outcome = CheckOutcomePass
		}
		res.Pass = res.Outcome == CheckOutcomePass
		kinds := make([]string, 0, len(res.Violations))
		for _, violation := range res.Violations {
			kinds = append(kinds, violation.Kind)
		}
		return event(ctx, tx, cleaned.Repo, "migration.checked", cleaned.Migration, "", map[string]any{
			"generation": cleaned.Generation,
			"pass":       res.Pass,
			"outcome":    res.Outcome,
			"changed":    len(cleaned.ChangedPaths),
			"deferred":   res.Deferred,
			"violations": kinds,
		})
	})
	return res, err
}

func coveredBy(ws []writePath, p string) bool {
	for _, w := range ws {
		if w.covers(p) {
			return true
		}
	}
	return false
}

func holderOf(sets map[string][]writePath, p string) string {
	ids := make([]string, 0, len(sets))
	for id := range sets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if coveredBy(sets[id], p) {
			return id
		}
	}
	return ""
}
