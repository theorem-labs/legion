package ledger

import (
	"context"
	"fmt"
)

// The admission fence. While a batch is admitted (batch_members.admitted), the landing program has checked its members
// against the ledger, and it lands them after GitHub merges its pull request, which cannot be undone. So until
// LandBatch, ResolveBatch or DeclineBatch releases the admission, the ledger refuses with batch_admitted every mutation that would
// change what that check read: a change to a member migration; a transfer of a contract that a member changes, whose
// owner the review rule read; the registration of a name that a member reads; and a landing or an admission that would
// advance a contract that a member reads, or that would read a contract that a member changes. Each fenced mutation
// takes the repository's row lock before it asks, and every admission and every release takes that row FOR UPDATE, so
// the answer holds until the mutation commits.

// admittedIn returns, for each of ids that a batch admits, that batch.
func admittedIn(ctx context.Context, q querier, repo string, ids []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT migration, batch FROM batch_members WHERE repo = $1 AND admitted AND migration = ANY($2)`,
		repo, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batches := map[string]string{}
	for rows.Next() {
		var migration, batch string
		if err := rows.Scan(&migration, &batch); err != nil {
			return nil, err
		}
		batches[migration] = batch
	}
	return batches, rows.Err()
}

// fenceMigration refuses a mutation of migration id while a batch admits it.
func fenceMigration(ctx context.Context, q querier, repo, id string) error {
	batches, err := admittedIn(ctx, q, repo, []string{id})
	if err != nil {
		return err
	}
	if batch, ok := batches[id]; ok {
		return refuse(ReasonBatchAdmitted, "migration %s is admitted in batch %s; that batch's landing or decline releases it", id, batch)
	}
	return nil
}

// reliance is an admitted member that reads a contract, and whether it also changes it. A member that changes a
// contract also reads it, because admission's Check refuses a change to a contract that is not a declared read.
type reliance struct {
	batch, member string
	changes       bool
}

func (r reliance) String() string {
	verb := "reads"
	if r.changes {
		verb = "changes"
	}
	return fmt.Sprintf("admitted member %s of batch %s %s it", r.member, r.batch, verb)
}

// admittedReliance returns, for each of names that an admitted member reads, those members: the ones that change it
// first, then by migration.
func admittedReliance(ctx context.Context, q querier, repo string, names []string) (map[string][]reliance, error) {
	rows, err := q.Query(ctx, `SELECT r.name, b.batch, b.migration, coalesce(m.ready_changed_contracts ? r.name, false) AS changes
		FROM batch_members b
		JOIN migrations m ON m.repo = b.repo AND m.id = b.migration
		JOIN migration_reads r ON r.repo = b.repo AND r.migration = b.migration
		WHERE b.repo = $1 AND b.admitted AND r.name = ANY($2)
		ORDER BY r.name, changes DESC, b.migration`, repo, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relying := map[string][]reliance{}
	for rows.Next() {
		var name string
		var r reliance
		if err := rows.Scan(&name, &r.batch, &r.member, &r.changes); err != nil {
			return nil, err
		}
		relying[name] = append(relying[name], r)
	}
	return relying, rows.Err()
}

// changer returns the first of relying that changes the contract.
func changer(relying []reliance) (reliance, bool) {
	if len(relying) > 0 && relying[0].changes {
		return relying[0], true
	}
	return reliance{}, false
}

// fenceContract refuses a mutation of contract name while an admitted member relies on it: any reader when reads is
// set, else only a member that changes it.
func fenceContract(ctx context.Context, q querier, repo, name string, reads bool) error {
	relying, err := admittedReliance(ctx, q, repo, []string{name})
	if err != nil {
		return err
	}
	r, fenced := changer(relying[name])
	if reads && len(relying[name]) > 0 {
		r, fenced = relying[name][0], true
	}
	if fenced {
		return refuse(ReasonBatchAdmitted, "contract %s is fenced: %s", name, r)
	}
	return nil
}

// fenceViolations reports what the fence refuses for a candidate of migration id that changes the contracts changed:
// the migration itself while a batch admits it, or else each changed contract that an admitted member reads.
func fenceViolations(ctx context.Context, q querier, repo, id string, changed []string) ([]Violation, error) {
	batches, err := admittedIn(ctx, q, repo, []string{id})
	if err != nil {
		return nil, err
	}
	if batch, ok := batches[id]; ok {
		return []Violation{{Kind: ViolationBatchAdmitted, Message: fmt.Sprintf(
			"migration %s is admitted in batch %s; only that batch lands it", id, batch)}}, nil
	}
	relying, err := admittedReliance(ctx, q, repo, changed)
	if err != nil {
		return nil, err
	}
	var violations []Violation
	for _, name := range changed {
		if len(relying[name]) > 0 {
			r := relying[name][0]
			violations = append(violations, Violation{Kind: ViolationBatchAdmitted, Name: name, Other: r.member,
				Message: fmt.Sprintf("the candidate advances contract %s, and %s", name, r)})
		}
	}
	return violations, nil
}
