package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func checkContractArgs(name, owner, hash string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return invalid("empty contract name")
	case strings.TrimSpace(owner) == "":
		return invalid("empty contract owner")
	case strings.TrimSpace(hash) == "":
		return invalid("contract %s has no statement hash", name)
	}
	return nil
}

const contractColumns = `name, owner, statement_hash, version, pinned, coalesce(last_change::text, ''), updated_at`

type contractRow interface {
	Scan(dest ...any) error
}

func scanContract(row contractRow) (Contract, error) {
	var c Contract
	var lastChange string
	err := row.Scan(&c.Name, &c.Owner, &c.StatementHash, &c.Version, &c.Pinned, &lastChange, &c.UpdatedAt)
	c.LastChange = LastChange(lastChange)
	return c, err
}

func loadContract(ctx context.Context, q querier, repo, name string, forUpdate bool) (Contract, error) {
	query := `SELECT ` + contractColumns + ` FROM contracts WHERE repo = $1 AND name = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	c, err := scanContract(q.QueryRow(ctx, query, repo, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, &NotFoundError{What: "contract", ID: name}
	}
	return c, err
}

// RegisterContract records a new contract at version one. Its first hash is the one that later changes compare.
func (s *Store) RegisterContract(ctx context.Context, repo, name, owner, hash string, pinned bool) (Contract, error) {
	if err := checkContractArgs(name, owner, hash); err != nil {
		return Contract{}, err
	}
	var c Contract
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		var err error
		c, err = scanContract(tx.QueryRow(ctx, `INSERT INTO contracts (repo, name, owner, statement_hash, pinned)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING
			RETURNING `+contractColumns, repo, name, owner, hash, pinned))
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse(ReasonExists, "contract %s already exists", name)
		}
		if err != nil {
			return err
		}
		return event(ctx, tx, repo, "contract.registered", name, owner, map[string]any{
			"statement_hash": hash,
			"version":        c.Version,
			"pinned":         pinned,
		})
	})
	return c, err
}

// Contract reads one contract's current owner, hash and version.
func (s *Store) Contract(ctx context.Context, repo, name string) (Contract, error) {
	var c Contract
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = loadContract(ctx, tx, repo, name, false)
		return err
	})
	return c, err
}

// Contracts lists every contract of a repository at its current owner, hash and version, ordered by name, from one
// read-only repeatable-read snapshot. A repository with no contracts gives an empty list; an unknown one is
// NotFoundError.
func (s *Store) Contracts(ctx context.Context, repo string) ([]Contract, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE name = $1)`, repo).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, &NotFoundError{What: "repository", ID: repo}
	}
	rows, err := tx.Query(ctx, `SELECT `+contractColumns+` FROM contracts
		WHERE repo = $1 ORDER BY name`, repo)
	if err != nil {
		return nil, err
	}
	contracts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contract, error) {
		return scanContract(r)
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if contracts == nil {
		contracts = []Contract{}
	}
	return contracts, nil
}

// TransferContract moves a contract to a new owner without changing its statement or version.
func (s *Store) TransferContract(ctx context.Context, repo, name, fromOwner, toOwner string) (Contract, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(fromOwner) == "" || strings.TrimSpace(toOwner) == "" {
		return Contract{}, invalid("a contract transfer needs a name, current owner and new owner")
	}
	var c Contract
	err := s.tx(ctx, func(tx pgx.Tx) error {
		current, err := loadContract(ctx, tx, repo, name, true)
		if err != nil {
			return err
		}
		if current.Owner != fromOwner {
			return refuse(ReasonNotOwner, "contract %s is owned by %s, not %s", name, current.Owner, fromOwner)
		}
		c, err = scanContract(tx.QueryRow(ctx, `UPDATE contracts SET owner = $3, updated_at = clock_timestamp()
			WHERE repo = $1 AND name = $2
			RETURNING `+contractColumns, repo, name, toOwner))
		if err != nil {
			return err
		}
		return event(ctx, tx, repo, "contract.transferred", name, toOwner, map[string]any{
			"from_owner": fromOwner,
			"version":    c.Version,
		})
	})
	return c, err
}

func checkAgreementArgs(id string, parties []string, reads map[string]int64) error {
	if strings.TrimSpace(id) == "" {
		return invalid("empty agreement id")
	}
	if len(parties) == 0 {
		return invalid("agreement %s has no parties", id)
	}
	for _, party := range parties {
		if strings.TrimSpace(party) == "" {
			return invalid("agreement %s has an empty party", id)
		}
	}
	for name := range reads {
		if strings.TrimSpace(name) == "" {
			return invalid("agreement %s reads an empty contract name", id)
		}
	}
	return nil
}

func sortedNames[V any](items map[string]V) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func loadAgreement(ctx context.Context, q querier, repo, id string, forUpdate bool) (Agreement, error) {
	query := `SELECT id, parties, state, created_at, staled_at FROM agreements WHERE repo = $1 AND id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var a Agreement
	err := q.QueryRow(ctx, query, repo, id).Scan(&a.ID, &a.Parties, &a.State, &a.CreatedAt, &a.StaledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, &NotFoundError{What: "agreement", ID: id}
	}
	if err != nil {
		return a, err
	}
	a.Reads = map[string]int64{}
	rows, err := q.Query(ctx, `SELECT contract, version FROM agreement_reads
		WHERE repo = $1 AND agreement = $2 ORDER BY contract`, repo, id)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var contract string
		var version int64
		if err := rows.Scan(&contract, &version); err != nil {
			return a, err
		}
		a.Reads[contract] = version
	}
	return a, rows.Err()
}

func agreementsInState(ctx context.Context, q querier, repo, state string) ([]Agreement, error) {
	rows, err := q.Query(ctx, `SELECT id FROM agreements WHERE repo = $1 AND state = $2 ORDER BY id`, repo, state)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]Agreement, 0, len(ids))
	for _, id := range ids {
		a, err := loadAgreement(ctx, q, repo, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// RecordAgreement records the current contract versions that its parties read. It refuses a version that has
// already been superseded, so each agreement has a coherent contract basis.
func (s *Store) RecordAgreement(ctx context.Context, repo, id string, parties []string, reads map[string]int64) (Agreement, error) {
	if err := checkAgreementArgs(id, parties, reads); err != nil {
		return Agreement{}, err
	}
	names := sortedNames(reads)
	var a Agreement
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO agreements (repo, id, parties) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING RETURNING id, parties, state, created_at, staled_at`, repo, id, parties).
			Scan(&a.ID, &a.Parties, &a.State, &a.CreatedAt, &a.StaledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse(ReasonExists, "agreement %s already exists", id)
		}
		if err != nil {
			return err
		}
		a.Reads = make(map[string]int64, len(reads))
		for _, name := range names {
			current, err := loadContract(ctx, tx, repo, name, true)
			if err != nil {
				return err
			}
			if got := reads[name]; got != current.Version {
				return refuse(ReasonStaleVersion, "agreement %s read contract %s at version %d, current version is %d",
					id, name, got, current.Version)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agreement_reads (repo, agreement, contract, version)
				VALUES ($1, $2, $3, $4)`, repo, id, name, current.Version); err != nil {
				return err
			}
			a.Reads[name] = current.Version
		}
		return event(ctx, tx, repo, "agreement.recorded", id, "", map[string]any{"reads": a.Reads, "parties": parties})
	})
	return a, err
}

// StaleAgreements lists every agreement whose recorded contract version was superseded by a landing.
func (s *Store) StaleAgreements(ctx context.Context, repo string) ([]Agreement, error) {
	var out []Agreement
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = agreementsInState(ctx, tx, repo, "stale")
		return err
	})
	return out, err
}

// LandRequest is the candidate that Land validates and closes. ChangedPaths and Current are the same candidate
// inputs Check receives. Current supplies every declared read; registered contracts also receive a version fence.
type LandRequest struct {
	Repo             string            `json:"repo"`
	Migration        string            `json:"migration"`
	Generation       int64             `json:"generation"`
	Commit           string            `json:"commit"`
	ChangedPaths     []string          `json:"changed_paths"`
	Current          map[string]string `json:"current"`
	ChangedContracts map[string]string `json:"changed_contracts"`
	ApprovedPinned   []string          `json:"approved_pinned"`
	// Head is the member's branch head that the landing program built. A statement change requires it.
	Head string `json:"head"`
	// CompatibilityProof is the fully qualified name of a declaration that proves that each changed contract gives
	// its callers all that the old one gave.
	CompatibilityProof string `json:"compatibility_proof"`
	// ReviewedHead is the commit at which an agent's review of the change passed.
	ReviewedHead string   `json:"reviewed_head"`
	Reviews      []Review `json:"reviews"`
	Upstream     string   `json:"upstream"`
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
	return []Violation{{Kind: ViolationStatementUnreviewed, Name: missing[0], Message: why}}, nil
}

// clearingRecord returns the one record that cleared an advanced contract, in priority order: the compatibility
// proof, then an explicit review naming the contract with its owner and the head, then the reviewed head as that
// owner's review. contract is the row Land read under its lock, so its owner is the one the review rule checked.
func clearingRecord(req LandRequest, contract Contract) (proof string, review Review, ok bool) {
	if req.CompatibilityProof != "" {
		return req.CompatibilityProof, Review{}, true
	}
	for _, candidate := range req.Reviews {
		if reviewClears(candidate, contract, req.Head) {
			return "", candidate, true
		}
	}
	if req.Head != "" && req.ReviewedHead == req.Head {
		return "", Review{Contract: contract.Name, Reviewer: contract.Owner, Head: req.ReviewedHead}, true
	}
	return "", Review{}, false
}

// contractLastChange is the last_change record of an advanced contract: the record that cleared it, with the
// upstream commit beside it when the landing carries upstream provenance.
func contractLastChange(req LandRequest, contract Contract) (LastChange, error) {
	proof, review, ok := clearingRecord(req, contract)
	if !ok {
		return "", invalid("changed contract %s has no clearing record", contract.Name)
	}
	switch {
	case req.Upstream != "" && proof != "":
		return marshalLastChange(struct {
			Upstream string `json:"upstream"`
			Proof    string `json:"proof"`
		}{Upstream: req.Upstream, Proof: proof})
	case req.Upstream != "":
		return marshalLastChange(struct {
			Upstream string   `json:"upstream"`
			Reviews  []Review `json:"reviews"`
		}{Upstream: req.Upstream, Reviews: []Review{review}})
	case proof != "":
		return marshalLastChange(struct {
			Proof string `json:"proof"`
		}{Proof: proof})
	default:
		return marshalLastChange(struct {
			Review Review `json:"review"`
		}{Review: review})
	}
}

func marshalLastChange(record any) (LastChange, error) {
	data, err := json.Marshal(record)
	return LastChange(data), err
}

func refusalForViolation(v Violation) error {
	switch v.Kind {
	case ViolationNotOpen:
		return refuse(ReasonNotOpen, "%s", v.Message)
	case ViolationStaleGeneration:
		return refuse(ReasonStaleGeneration, "%s", v.Message)
	case ViolationEscalated:
		return refuse(ReasonEscalated, "%s", v.Message)
	case ViolationInvalidCandidate:
		return refuse(ReasonInvalidCandidate, "%s", v.Message)
	case ViolationUndeclaredWrite:
		return refuse(ReasonUndeclaredWrite, "%s", v.Message)
	case ViolationStaleRead:
		return refuse(ReasonStaleRead, "%s", v.Message)
	case ViolationUnverifiedRead:
		return refuse(ReasonUnverifiedRead, "%s", v.Message)
	case ViolationOverlap:
		return refuse(ReasonOverlap, "%s", v.Message)
	case ViolationUndeclaredContract:
		return refuse(ReasonUndeclaredContract, "%s", v.Message)
	case ViolationPinned:
		return refuse(ReasonPinned, "%s", v.Message)
	case ViolationUpstreamNotAllowed:
		return refuse(ReasonUpstreamNotAllowed, "%s", v.Message)
	case ViolationStatementUnreviewed:
		return refuse(ReasonStatementUnreviewed, "%s", v.Message)
	default:
		return fmt.Errorf("unmapped violation kind %q", v.Kind)
	}
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
				return []Violation{{Kind: ViolationStaleRead, Message: refusal.Message}}, nil
			}
			return nil, err
		}
	}
	var violations []Violation
	for _, name := range sortedNames(req.ChangedContracts) {
		recorded, declared := migration.ReadVersions[name]
		if !declared {
			violations = append(violations, Violation{Kind: ViolationUndeclaredContract, Name: name,
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
				violations = append(violations, Violation{Kind: ViolationStaleRead, Name: name,
					Message: fmt.Sprintf("migration %s did not record a contract version for %s", migration.ID, name)})
				continue
			}
			current = contracts[name]
			if current.Name == "" {
				violations = append(violations, Violation{Kind: ViolationStaleRead, Name: name,
					Message: fmt.Sprintf("migration %s did not read current contract %s", migration.ID, name)})
				continue
			}
		}
		if current.Pinned {
			if _, ok := approved[name]; !ok {
				violations = append(violations, Violation{Kind: ViolationPinned, Name: name,
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

// Land validates a complete candidate, advances its changed contracts, makes dependent agreements stale, and closes
// the migration at the supplied commit. It holds the repository, migration and contract locks through every step.
//
// A candidate with changed contracts changes a statement, and it needs Head. Each changed registered contract must be
// cleared by one record: the CompatibilityProof, which clears every one; a review in Reviews that names the contract,
// whose Reviewer is the contract's owner in the ledger and whose Head is the request's Head; or a ReviewedHead equal to
// Head, which clears them only when every changed contract has one owner, and then counts as that owner's review.
// Otherwise Land refuses with statement_unreviewed, naming every contract left uncleared, after the shared checks and
// the pinned approvals and before any write. A candidate with no changed contracts changes proofs only and needs none
// of these records. Upstream is accepted only when the migration's owner is the repository's upstream owner, and is
// refused with upstream_not_allowed otherwise. It is provenance only: it clears neither the statement rule nor a pinned
// contract. The ledger trusts its caller for these records, as it does for ApprovedPinned: it does not build the proof
// declaration or confirm that the review happened. It stores the record that cleared each advanced contract, with the
// upstream commit beside it, as that contract's last_change, and keeps the request's records in the migration.landed
// event.
func (s *Store) Land(ctx context.Context, req LandRequest) (Migration, error) {
	candidateRequest := CheckRequest{
		Repo: req.Repo, ID: req.Migration, Generation: req.Generation, Commit: req.Commit, Changed: req.ChangedPaths,
		Current: req.Current, ApprovedPinned: req.ApprovedPinned, ChangedContracts: req.ChangedContracts, Head: req.Head,
		CompatibilityProof: req.CompatibilityProof, ReviewedHead: req.ReviewedHead, Reviews: req.Reviews, Upstream: req.Upstream,
	}
	if precondition, ok := candidatePrecondition(candidateRequest); ok {
		return Migration{}, refusalForViolation(precondition)
	}
	candidate, err := cleanCheckRequest(candidateRequest)
	if err != nil {
		return Migration{}, err
	}
	names := sortedNames(req.ChangedContracts)
	reviews := req.Reviews
	if reviews == nil {
		reviews = []Review{}
	}
	approved := make(map[string]struct{}, len(req.ApprovedPinned))
	for _, name := range req.ApprovedPinned {
		approved[name] = struct{}{}
	}
	var landed Migration
	err = s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, req.Repo, true)
		if err != nil {
			return err
		}
		migration, err := lockOpenAt(ctx, tx, req.Repo, req.Migration, req.Generation)
		if err != nil {
			return err
		}
		violations, err := checkViolations(ctx, tx, migration, candidate)
		if err != nil {
			return err
		}
		if len(violations) > 0 {
			return refusalForViolation(violations[0])
		}
		contracts, err := contractReadsAtVersions(ctx, tx, req.Repo, migration)
		if err != nil {
			return err
		}
		statementContracts := make([]Contract, 0, len(names))
		for _, name := range names {
			recorded, declared := migration.ReadVersions[name]
			if !declared {
				return refuse(ReasonUndeclaredContract, "migration %s did not declare contract %s as a read", migration.ID, name)
			}
			if recorded == nil {
				return refuse(ReasonStaleRead, "migration %s did not record a contract version for %s", migration.ID, name)
			}
			current := contracts[name]
			if current.Name == "" {
				return refuse(ReasonStaleRead, "migration %s did not read current contract %s", migration.ID, name)
			}
			if current.Pinned {
				if _, ok := approved[name]; !ok {
					return refuse(ReasonPinned, "contract %s is pinned; a person must approve its change", name)
				}
			}
			statementContracts = append(statementContracts, current)
		}
		if why, _ := statementUnreviewed(statementContracts, req.Head, req.CompatibilityProof, req.ReviewedHead, req.Reviews); why != "" {
			return refuse(ReasonStatementUnreviewed, "%s", why)
		}
		if upstream, blocked := upstreamViolation(repository, migration, req.Upstream); blocked {
			return refusalForViolation(upstream)
		}
		for _, name := range names {
			current := contracts[name]
			var advanced Contract
			lastChange, err := contractLastChange(req, current)
			if err != nil {
				return err
			}
			advanced, err = scanContract(tx.QueryRow(ctx, `UPDATE contracts SET statement_hash = $3, version = version + 1,
				last_change = $4, updated_at = clock_timestamp() WHERE repo = $1 AND name = $2
				RETURNING `+contractColumns, req.Repo, name, req.ChangedContracts[name], string(lastChange)))
			if err != nil {
				return err
			}
			if err := event(ctx, tx, req.Repo, "contract.advanced", name, migration.Owner, map[string]any{
				"from_hash": current.StatementHash,
				"to_hash":   advanced.StatementHash,
				"version":   advanced.Version,
			}); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `UPDATE agreements SET state = 'stale', staled_at = clock_timestamp()
				WHERE repo = $1 AND state = 'agreed' AND EXISTS (
					SELECT 1 FROM agreement_reads
					WHERE agreement_reads.repo = agreements.repo
						AND agreement_reads.agreement = agreements.id
						AND contract = $2 AND version < $3
				) RETURNING id`, req.Repo, name, advanced.Version)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, agreement := range ids {
				if err := event(ctx, tx, req.Repo, "agreement.staled", agreement, "", map[string]any{
					"contract": name,
					"version":  advanced.Version,
				}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE migrations SET state = $3, closed_at = clock_timestamp(),
			close_reason = NULL, landed_commit = $4 WHERE repo = $1 AND id = $2`, req.Repo, req.Migration, StateLanded, req.Commit); err != nil {
			return err
		}
		if err := event(ctx, tx, req.Repo, "migration.landed", req.Migration, migration.Owner, map[string]any{
			"generation":          req.Generation,
			"commit":              req.Commit,
			"contracts":           names,
			"head":                req.Head,
			"compatibility_proof": req.CompatibilityProof,
			"reviewed_head":       req.ReviewedHead,
			"reviews":             reviews,
			"upstream":            req.Upstream,
		}); err != nil {
			return err
		}
		landed, err = loadMigration(ctx, tx, req.Repo, req.Migration, false)
		return err
	})
	return landed, err
}
