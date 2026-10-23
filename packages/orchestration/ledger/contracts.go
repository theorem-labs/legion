package ledger

import (
	"context"
	"errors"
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

func loadContract(ctx context.Context, q querier, repo, name string, forUpdate bool) (Contract, error) {
	query := `SELECT name, owner, statement_hash, version, pinned, updated_at FROM contracts WHERE repo = $1 AND name = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var c Contract
	err := q.QueryRow(ctx, query, repo, name).Scan(&c.Name, &c.Owner, &c.StatementHash, &c.Version, &c.Pinned, &c.UpdatedAt)
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
		err := tx.QueryRow(ctx, `INSERT INTO contracts (repo, name, owner, statement_hash, pinned)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING
			RETURNING name, owner, statement_hash, version, pinned, updated_at`, repo, name, owner, hash, pinned).
			Scan(&c.Name, &c.Owner, &c.StatementHash, &c.Version, &c.Pinned, &c.UpdatedAt)
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
		if err := tx.QueryRow(ctx, `UPDATE contracts SET owner = $3, updated_at = clock_timestamp()
			WHERE repo = $1 AND name = $2
			RETURNING name, owner, statement_hash, version, pinned, updated_at`, repo, name, toOwner).
			Scan(&c.Name, &c.Owner, &c.StatementHash, &c.Version, &c.Pinned, &c.UpdatedAt); err != nil {
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
// inputs Check receives. Current supplies every read that was not a registered contract when the migration recorded it.
type LandRequest struct {
	Repo             string            `json:"repo"`
	Migration        string            `json:"migration"`
	Generation       int64             `json:"generation"`
	Commit           string            `json:"commit"`
	ChangedPaths     []string          `json:"changed_paths"`
	Current          map[string]string `json:"current"`
	ChangedContracts map[string]string `json:"changed_contracts"`
	ApprovedPinned   []string          `json:"approved_pinned"`
}

func refusalForViolation(v Violation) *Refusal {
	switch v.Kind {
	case ViolationNotOpen:
		return refuse(ReasonNotOpen, "%s", v.Message)
	case ViolationStaleGeneration:
		return refuse(ReasonStaleGeneration, "%s", v.Message)
	case ViolationEscalated:
		return refuse(ReasonEscalated, "%s", v.Message)
	case ViolationUndeclaredWrite:
		return refuse(ReasonUndeclaredWrite, "%s", v.Message)
	case ViolationOverlap:
		return refuse(ReasonOverlap, "%s", v.Message)
	default:
		return refuse(ReasonStaleRead, "%s", v.Message)
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

// Land validates a complete candidate, advances its changed contracts, makes dependent agreements stale, and closes
// the migration at the supplied commit. It holds the repository, migration and contract locks through every step.
func (s *Store) Land(ctx context.Context, req LandRequest) (Migration, error) {
	if strings.TrimSpace(req.Commit) == "" {
		return Migration{}, invalid("empty landed commit")
	}
	for name, hash := range req.ChangedContracts {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(hash) == "" {
			return Migration{}, invalid("landed contracts need a name and statement hash")
		}
	}
	candidate, err := cleanCheckRequest(CheckRequest{
		Repo: req.Repo, ID: req.Migration, Generation: req.Generation, Changed: req.ChangedPaths, Current: req.Current,
	})
	if err != nil {
		return Migration{}, err
	}
	names := sortedNames(req.ChangedContracts)
	approved := make(map[string]struct{}, len(req.ApprovedPinned))
	for _, name := range req.ApprovedPinned {
		approved[name] = struct{}{}
	}
	var landed Migration
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, req.Repo, true); err != nil {
			return err
		}
		migration, err := lockOpenAt(ctx, tx, req.Repo, req.Migration, req.Generation)
		if err != nil {
			return err
		}
		violations, err := checkViolations(ctx, tx, migration, candidate, true)
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
		}
		for _, name := range names {
			current := contracts[name]
			var advanced Contract
			if err := tx.QueryRow(ctx, `UPDATE contracts SET statement_hash = $3, version = version + 1,
				updated_at = clock_timestamp() WHERE repo = $1 AND name = $2
				RETURNING name, owner, statement_hash, version, pinned, updated_at`, req.Repo, name, req.ChangedContracts[name]).
				Scan(&advanced.Name, &advanced.Owner, &advanced.StatementHash, &advanced.Version, &advanced.Pinned, &advanced.UpdatedAt); err != nil {
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
			"generation": req.Generation,
			"commit":     req.Commit,
			"contracts":  names,
		}); err != nil {
			return err
		}
		landed, err = loadMigration(ctx, tx, req.Repo, req.Migration, false)
		return err
	})
	return landed, err
}
