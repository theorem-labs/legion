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

const contractColumns = `name, owner, statement_hash, version, pinned, last_change, updated_at`

type contractRow interface {
	Scan(dest ...any) error
}

func scanContract(row contractRow) (Contract, error) {
	var c Contract
	err := row.Scan(&c.Name, &c.Owner, &c.StatementHash, &c.Version, &c.Pinned, &c.LastChange, &c.UpdatedAt)
	return c, err
}

// rowLock is the row lock a read of a contract takes: none, a share lock for a check, or an update lock for a write.
// It is a SQL clause, and only these three values exist.
type rowLock string

const (
	noLock     rowLock = ""
	shareLock  rowLock = " FOR SHARE"
	updateLock rowLock = " FOR UPDATE"
)

func loadContract(ctx context.Context, q querier, repo, name string, lock rowLock) (Contract, error) {
	query := `SELECT ` + contractColumns + ` FROM contracts WHERE repo = $1 AND name = $2` + string(lock)
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
		c, err = loadContract(ctx, tx, repo, name, noLock)
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
		current, err := loadContract(ctx, tx, repo, name, updateLock)
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
			current, err := loadContract(ctx, tx, repo, name, updateLock)
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

// Land validates a complete candidate, advances its changed contracts, makes dependent agreements stale, and closes
// the migration at the supplied commit. It holds the repository, migration and contract locks through every step.
//
// Land applies the rules Check applies, through the same evaluation, and refuses with the first violation Check
// would list. A candidate with changed contracts changes a statement, and it needs Head. Each changed registered
// contract must be cleared by one record: the CompatibilityProof, which clears every one; a review in Reviews that
// names the contract, whose Reviewer is the contract's owner in the ledger and whose Head is the request's Head; or a
// ReviewedHead equal to Head, which clears them only when every changed contract has one owner, and then counts as that
// owner's review. A candidate with no changed contracts changes proofs only and needs none of these records. Upstream
// names the upstream commit a landing carries, from a migration of any owner. It is provenance only: it clears neither
// the statement rule nor a pinned contract. The ledger trusts its caller for these records, as it does for
// ApprovedPinned: it does not build the proof declaration or confirm that the review happened. It stores the record
// that cleared each advanced contract, with the upstream commit beside it, as that contract's last_change, and keeps
// the request's records in the migration.landed event.
func (s *Store) Land(ctx context.Context, req LandRequest) (Migration, error) {
	candidate, err := cleanCandidate(CheckRequest{LandRequest: req})
	if err != nil {
		return Migration{}, err
	}
	req = candidate.LandRequest
	reviews := req.withEmptyLists().Reviews
	var landed Migration
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, req.Repo, true); err != nil {
			return err
		}
		migration, err := loadMigration(ctx, tx, req.Repo, req.Migration, true)
		if err != nil {
			return err
		}
		v, err := evaluate(ctx, tx, migration, candidate, updateLock)
		if err != nil {
			return err
		}
		if len(v.violations) > 0 {
			return refusalForViolation(v.violations[0])
		}
		names := make([]string, 0, len(v.changed))
		for _, current := range v.changed {
			name := current.Name
			names = append(names, name)
			lastChange := v.cleared[name]
			advanced, err := scanContract(tx.QueryRow(ctx, `UPDATE contracts SET statement_hash = $3, version = version + 1,
				last_change = $4, updated_at = clock_timestamp() WHERE repo = $1 AND name = $2
				RETURNING `+contractColumns, req.Repo, name, req.ChangedContracts[name], lastChange))
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
