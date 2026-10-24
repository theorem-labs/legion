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

// CheckRequest asks whether a candidate change may land for a migration.
type CheckRequest struct {
	Repo       string `json:"repo"`
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Commit     string `json:"commit"`
	// Changed is every path the candidate adds, modifies or deletes, relative to the repository root.
	Changed []string `json:"changed"`
	// Current maps a read's name to its statement hash in the candidate's base. An empty hash means the statement
	// no longer exists; a name missing from the map was not hashed, so that read cannot be decided.
	Current map[string]string `json:"current"`
	// ApprovedPinned names pinned contracts with a person's approval to change.
	ApprovedPinned []string `json:"approved_pinned"`
	// DeferReads omits read-hash checks for a pre-build candidate. It can only produce an incomplete result.
	DeferReads bool `json:"defer_reads"`
	// ChangedContracts and its statement records are the same as Land receives, under the same rules: changed
	// contracts need Head, even with a compatibility proof, or the candidate is invalid_candidate.
	ChangedContracts   map[string]string `json:"changed_contracts"`
	Head               string            `json:"head"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []Review          `json:"reviews"`
	Upstream           string            `json:"upstream"`
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
// Deferred lists declared reads that a deferred check intentionally did not validate.
type CheckResult struct {
	Migration  string      `json:"migration"`
	Generation int64       `json:"generation"`
	Outcome    string      `json:"outcome"`
	Pass       bool        `json:"pass"`
	Deferred   []string    `json:"deferred"`
	Violations []Violation `json:"violations"`
}

// checkStatementChange validates a candidate's changed contracts and the records that clear its statement change, as
// Land and Check receive them. A record is either absent ("") or a value with no surrounding whitespace.
func checkStatementChange(changes map[string]string, head, proof, reviewed string, reviews []Review, upstream string) error {
	for name, hash := range changes {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(hash) == "" {
			return invalid("changed contracts need a name and a statement hash")
		}
	}
	for _, record := range []struct{ name, value string }{
		{"head", head}, {"compatibility proof", proof}, {"reviewed head", reviewed}, {"upstream", upstream},
	} {
		if record.value != strings.TrimSpace(record.value) {
			return invalid("%s %q is blank or has leading or trailing whitespace", record.name, record.value)
		}
	}
	for _, review := range reviews {
		for _, field := range []struct{ name, value string }{
			{"review contract", review.Contract}, {"reviewer", review.Reviewer}, {"review head", review.Head},
		} {
			if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
				return invalid("%s %q is blank or has leading or trailing whitespace", field.name, field.value)
			}
		}
	}
	return nil
}

func reviewClears(review Review, contract Contract, head string) bool {
	return review.Contract == contract.Name && review.Reviewer == contract.Owner && review.Head == head
}

// statementUnreviewed explains which statement changes lack a compatibility proof or an owner review at head.
func statementUnreviewed(contracts []Contract, head, proof, reviewed string, reviews []Review) (string, []string) {
	if len(contracts) == 0 || proof != "" {
		return "", nil
	}
	sameOwner := true
	for _, contract := range contracts[1:] {
		if contract.Owner != contracts[0].Owner {
			sameOwner = false
			break
		}
	}
	cleared := make(map[string]struct{}, len(contracts))
	if head != "" && reviewed == head && sameOwner {
		for _, contract := range contracts {
			cleared[contract.Name] = struct{}{}
		}
	}
	for _, review := range reviews {
		for _, contract := range contracts {
			if reviewClears(review, contract, head) {
				cleared[contract.Name] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(contracts))
	for _, contract := range contracts {
		if _, ok := cleared[contract.Name]; !ok {
			missing = append(missing, contract.Name)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	subject := "contract " + missing[0] + " changes its statement"
	if len(missing) > 1 {
		subject = "contracts " + strings.Join(missing, ", ") + " change their statements"
	}
	at := "the head that lands"
	if head != "" {
		at = head
	}
	review := "no review"
	switch {
	case reviewed != "" && head != "" && reviewed != head:
		review = "a review that passed at " + reviewed + ", not at " + head
	case reviewed != "" && head != "" && !sameOwner:
		review = "a reviewed head at " + reviewed + ", but it clears every changed contract only when they have the same owner"
	case reviewed != "":
		review = "a review that passed at " + reviewed + ", but no head to compare it with"
	}
	return fmt.Sprintf("%s; a statement change lands only with a compatibility proof or an agent's review at %s, "+
		"and this one has no compatibility proof and %s", subject, at, review), missing
}

// statementViolations reports each changed registered contract that the candidate's records do not clear.
func statementViolations(ctx context.Context, q querier, m Migration, req CheckRequest) ([]Violation, error) {
	declared := make([]string, 0, len(req.ChangedContracts))
	for _, name := range sortedNames(req.ChangedContracts) {
		if _, ok := m.Reads[name]; ok {
			declared = append(declared, name)
		}
	}
	if len(declared) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT name, owner FROM contracts WHERE repo = $1 AND name = ANY($2) ORDER BY name`, req.Repo, declared)
	if err != nil {
		return nil, err
	}
	registered, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Contract, error) {
		var contract Contract
		err := row.Scan(&contract.Name, &contract.Owner)
		return contract, err
	})
	if err != nil {
		return nil, err
	}
	why, missing := statementUnreviewed(registered, req.Head, req.CompatibilityProof, req.ReviewedHead, req.Reviews)
	if why == "" {
		return nil, nil
	}
	return []Violation{{Kind: ReasonStatementUnreviewed, Name: missing[0], Message: why}}, nil
}

// refusalForViolation is the refusal Land gives for a violation: a violation's kind is its refusal reason.
func refusalForViolation(v Violation) error {
	return &Refusal{Reason: v.Kind, Message: v.Message}
}

func contractReadsAtVersions(ctx context.Context, tx pgx.Tx, repo string, migration Migration) (map[string]Contract, error) {
	contracts := map[string]Contract{}
	for _, name := range sortedNames(migration.ReadVersions) {
		recorded := migration.ReadVersions[name]
		if recorded == nil {
			var registered bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contracts WHERE repo = $1 AND name = $2)`, repo, name).Scan(&registered); err != nil {
				return nil, err
			}
			if registered {
				return nil, refuse(ReasonStaleRead, "migration %s did not record a version for contract %s", migration.ID, name)
			}
			continue
		}
		current, err := loadContract(ctx, tx, repo, name, true)
		if err != nil {
			var missing *NotFoundError
			if errors.As(err, &missing) {
				return nil, refuse(ReasonStaleRead, "recorded contract %s no longer exists", name)
			}
			return nil, err
		}
		if current.Version != *recorded || current.StatementHash != migration.Reads[name] {
			return nil, refuse(ReasonStaleRead, "migration %s read contract %s at version %d hash %s, current version is %d hash %s",
				migration.ID, name, *recorded, migration.Reads[name], current.Version, current.StatementHash)
		}
		contracts[name] = current
	}
	return contracts, nil
}

// checkContractViolations runs Land's version, declaration, pinned and statement checks without changing contracts.
func checkContractViolations(ctx context.Context, tx pgx.Tx, migration Migration, req CheckRequest) ([]Violation, error) {
	approved := make(map[string]struct{}, len(req.ApprovedPinned))
	for _, name := range req.ApprovedPinned {
		approved[name] = struct{}{}
	}
	var contracts map[string]Contract
	if !req.DeferReads {
		var err error
		contracts, err = contractReadsAtVersions(ctx, tx, req.Repo, migration)
		if err != nil {
			var refusal *Refusal
			if errors.As(err, &refusal) && refusal.Reason == ReasonStaleRead {
				return []Violation{{Kind: ReasonStaleRead, Message: refusal.Message}}, nil
			}
			return nil, err
		}
	}
	var violations []Violation
	for _, name := range sortedNames(req.ChangedContracts) {
		recorded, declared := migration.ReadVersions[name]
		if !declared {
			violations = append(violations, Violation{Kind: ReasonUndeclaredContract, Name: name,
				Message: fmt.Sprintf("migration %s did not declare contract %s as a read", migration.ID, name)})
			continue
		}
		var current Contract
		if req.DeferReads {
			var err error
			current, err = loadContract(ctx, tx, req.Repo, name, false)
			if err != nil {
				var missing *NotFoundError
				if errors.As(err, &missing) {
					continue
				}
				return nil, err
			}
		} else {
			if recorded == nil {
				violations = append(violations, Violation{Kind: ReasonStaleRead, Name: name,
					Message: fmt.Sprintf("migration %s did not record a contract version for %s", migration.ID, name)})
				continue
			}
			current = contracts[name]
			if current.Name == "" {
				violations = append(violations, Violation{Kind: ReasonStaleRead, Name: name,
					Message: fmt.Sprintf("migration %s did not read current contract %s", migration.ID, name)})
				continue
			}
		}
		if current.Pinned {
			if _, ok := approved[name]; !ok {
				violations = append(violations, Violation{Kind: ReasonPinned, Name: name,
					Message: fmt.Sprintf("contract %s is pinned; a person must approve its change", name)})
			}
		}
	}
	statement, err := statementViolations(ctx, tx, migration, req)
	if err != nil {
		return nil, err
	}
	return append(violations, statement...), nil
}

// candidatePrecondition reports the candidate metadata that Land refuses before it checks migration state.
func candidatePrecondition(req CheckRequest) (Violation, bool) {
	var message string
	switch {
	case req.Changed == nil:
		message = "missing changed paths"
	case len(req.Changed) == 0 && len(req.ChangedContracts) > 0:
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

// cleanCheckRequest validates and normalizes candidate paths before they are compared to declared writes, and
// validates the candidate's changed contracts and statement records.
func cleanCheckRequest(req CheckRequest) (CheckRequest, error) {
	if err := checkStatementChange(req.ChangedContracts, req.Head, req.CompatibilityProof, req.ReviewedHead, req.Reviews, req.Upstream); err != nil {
		return CheckRequest{}, err
	}
	changed := make([]string, 0, len(req.Changed))
	for _, path := range req.Changed {
		cleaned, err := cleanRepoPath(path)
		if err != nil {
			return CheckRequest{}, err
		}
		changed = append(changed, cleaned)
	}
	sort.Strings(changed)
	req.Changed = changed
	return req, nil
}

// checkViolations evaluates every candidate check against a migration locked by the caller.
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
		add(Violation{Kind: ReasonEscalated, Message: fmt.Sprintf(
			"migration %s was escalated to the steward: %s", m.ID, m.EscalationReason)})
	}
	ours, err := migrationWrites(ctx, q, req.Repo, req.ID)
	if err != nil {
		return nil, err
	}
	sets, err := openWriteSets(ctx, q, req.Repo, req.ID)
	if err != nil {
		return nil, err
	}
	for _, path := range req.Changed {
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
	names := sortedNames(m.Reads)
	if !req.DeferReads {
		for _, name := range names {
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

func upstreamViolation(repository Repository, migration Migration, upstream string) (Violation, bool) {
	if upstream == "" || repository.UpstreamOwner == migration.Owner {
		return Violation{}, false
	}
	if repository.UpstreamOwner == "" {
		return Violation{
			Kind:    ReasonUpstreamNotAllowed,
			Message: fmt.Sprintf("repository %s has no configured upstream owner", repository.Name),
		}, true
	}
	return Violation{
		Kind: ReasonUpstreamNotAllowed,
		Message: fmt.Sprintf("migration %s is owned by %s, not repository %s's upstream owner %s",
			migration.ID, migration.Owner, repository.Name, repository.UpstreamOwner),
	}, true
}

// Check decides whether a candidate change may land for a migration. It writes nothing but the event that
// records its decision. A refusal of the request itself (an unknown migration, a bad path) is an error; every
// reason the candidate must not land is a violation in the result.
func (s *Store) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	precondition, hasPrecondition := candidatePrecondition(req)
	cleaned, err := cleanCheckRequest(req)
	if err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{Migration: cleaned.ID, Generation: cleaned.Generation, Deferred: []string{}}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, cleaned.Repo, false)
		if err != nil {
			return err
		}
		migration, err := loadMigration(ctx, tx, cleaned.Repo, cleaned.ID, true)
		if err != nil {
			return err
		}
		if cleaned.DeferReads {
			res.Deferred = sortedNames(migration.Reads)
		}
		if hasPrecondition {
			res.Violations = append(res.Violations, precondition)
		}
		violations, err := checkViolations(ctx, tx, migration, cleaned)
		if err != nil {
			return err
		}
		res.Violations = append(res.Violations, violations...)
		contract, err := checkContractViolations(ctx, tx, migration, cleaned)
		if err != nil {
			return err
		}
		res.Violations = append(res.Violations, contract...)
		if upstream, blocked := upstreamViolation(repository, migration, cleaned.Upstream); blocked {
			res.Violations = append(res.Violations, upstream)
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
		return event(ctx, tx, cleaned.Repo, "migration.checked", cleaned.ID, "", map[string]any{
			"generation": cleaned.Generation,
			"pass":       res.Pass,
			"outcome":    res.Outcome,
			"changed":    len(cleaned.Changed),
			"deferred":   res.Deferred,
			"violations": kinds,
		})
	})
	if res.Violations == nil {
		res.Violations = []Violation{}
	}
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
