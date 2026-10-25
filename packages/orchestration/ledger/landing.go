package ledger

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/jackc/pgx/v5"
)

const admissionLanded = "landed"

// LandBatchRequest lands an admitted batch after GitHub merged its pull request at MergedCommit. ExpectedVersion is the
// record version the caller read, and Record is the landing program's batch record for the landed state.
type LandBatchRequest struct {
	Repo            string          `json:"repo"`
	ID              string          `json:"id"`
	MergedCommit    string          `json:"merged_commit"`
	ExpectedVersion int64           `json:"expected_version"`
	Record          json.RawMessage `json:"record"`
}

// LandBatch lands batch id's admitted members at the merged commit. In one transaction, after the locks in the batch
// lock order (the repository row, the batch row, the members' migrations by id, then the contracts they read by name):
//   - the replay: a batch already landed at the same merged commit is returned as stored, whatever version and record
//     the request carries, because the request is the retry of the landing whose response was lost;
//   - the version: a batch at another record version than ExpectedVersion is refused with stale_batch;
//   - the landing: each member in position order lands as Land lands it, from the ready record that its admission
//     froze, at the merged commit (landBatch).
//
// It runs no other refusal: admission checked every member, and the fence has kept everything that check read.
func (s *Store) LandBatch(ctx context.Context, req LandBatchRequest) (Batch, error) {
	var landed Batch
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, req.Repo, true); err != nil {
			return err
		}
		stored, err := loadBatch(ctx, tx, req.Repo, req.ID, true)
		if err != nil {
			return err
		}
		if stored.Admission == admissionLanded && stored.MergedCommit == req.MergedCommit {
			landed = stored
			return nil
		}
		if stored.RecordVersion != req.ExpectedVersion {
			return refuse(ReasonStaleBatch, "batch %s is at record version %d, not version %d", req.ID, stored.RecordVersion, req.ExpectedVersion)
		}
		landed, err = landBatch(ctx, tx, stored, req.MergedCommit, req.Record, "batch.landed")
		return err
	})
	if err != nil {
		return Batch{}, err
	}
	return landed, nil
}

// ResolveBatch is an operator's resolution of a batch that GitHub merged and the ledger did not land
// (landed_unrecorded), after the escalation's cause is fixed. The operator sends no record, so the ledger derives it
// from the stored one: state landed, reason "resolved by operator", and pull_request.merged_commit mergedCommit, with
// every other field kept. In one transaction, after the batch lock order, it does with that record at the stored
// version what LandBatch does (landBatch), and appends batch.resolved, whose login is the operator's. A repeat with
// the same merged commit returns the stored batch. The ledger checks neither the stored phase nor an earlier merged
// commit: the operator resolves the batch that the escalation item names.
func (s *Store) ResolveBatch(ctx context.Context, repo, id, mergedCommit string) (Batch, error) {
	var resolved Batch
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, true); err != nil {
			return err
		}
		stored, err := loadBatch(ctx, tx, repo, id, true)
		if err != nil {
			return err
		}
		if stored.Admission == admissionLanded && stored.MergedCommit == mergedCommit {
			resolved = stored
			return nil
		}
		var record json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT record || jsonb_build_object('state', $3::text, 'reason', 'resolved by operator',
				'pull_request', CASE jsonb_typeof(record -> 'pull_request') WHEN 'object' THEN record -> 'pull_request'
					ELSE '{}'::jsonb END || jsonb_build_object('merged_commit', $4::text))
			FROM batches WHERE repo = $1 AND id = $2`, repo, id, PhaseLanded, mergedCommit).Scan(&record); err != nil {
			return err
		}
		resolved, err = landBatch(ctx, tx, stored, mergedCommit, record, "batch.resolved")
		return err
	})
	if err != nil {
		return Batch{}, err
	}
	return resolved, nil
}

// batchMember is one frozen member of a batch, in position order.
type batchMember struct {
	migration  string
	generation int64
	readyEvent int64
}

// admittedMembers reads the members that batch id admits, in position order.
func admittedMembers(ctx context.Context, tx pgx.Tx, repo, id string) ([]batchMember, error) {
	rows, err := tx.Query(ctx, `SELECT migration, generation, ready_event FROM batch_members
		WHERE repo = $1 AND batch = $2 AND admitted ORDER BY position`, repo, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (batchMember, error) {
		var m batchMember
		err := row.Scan(&m.migration, &m.generation, &m.readyEvent)
		return m, err
	})
}

// landBatch makes the writes of a landing of batch, which its caller has locked and version-checked, at mergedCommit
// with the caller's record:
//   - each member that the batch admits, in position order, lands as Land lands it, with the generation, head, changed
//     contracts and statement records of the ready record that its admission froze (the migration.ready event that
//     batch_members.ready_event names), at mergedCommit: its contract advances, the agreements they make stale, and the
//     migration closed as landed, with the batch in its migration.landed event;
//   - the admission released, so the fence no longer holds its members;
//   - one intent per entry of the record's post_land_effects, with the entry's key, op and member and the result
//     {commit: mergedCommit}, so a crash after the landing cannot lose a post-landing effect;
//   - the record at the next version, admission landed, the merged commit, and kind's event.
func landBatch(ctx context.Context, tx pgx.Tx, batch Batch, mergedCommit string, record json.RawMessage, kind string) (Batch, error) {
	members, err := admittedMembers(ctx, tx, batch.Repo, batch.ID)
	if err != nil {
		return Batch{}, err
	}
	ids := make([]string, len(members))
	for i, member := range members {
		ids[i] = member.migration
	}
	migrations, _, err := lockMembers(ctx, tx, batch.Repo, ids)
	if err != nil {
		return Batch{}, err
	}
	for i, member := range members {
		if err := landMember(ctx, tx, batch.Repo, batch.ID, migrations[i], member, mergedCommit); err != nil {
			return Batch{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE batch_members SET admitted = false WHERE repo = $1 AND batch = $2 AND admitted`,
		batch.Repo, batch.ID); err != nil {
		return Batch{}, err
	}
	effectRows, err := tx.Query(ctx, `INSERT INTO batch_effects (repo, batch, key, member, op, result)
		SELECT $1, $2, e->>'key', coalesce(e->>'member', ''), e->>'op', jsonb_build_object('commit', $4::text)
		FROM jsonb_array_elements(CASE jsonb_typeof($3::jsonb -> 'post_land_effects')
			WHEN 'array' THEN $3::jsonb -> 'post_land_effects' ELSE '[]'::jsonb END) e
		ON CONFLICT DO NOTHING RETURNING key`, batch.Repo, batch.ID, string(record), mergedCommit)
	if err != nil {
		return Batch{}, err
	}
	effects, err := pgx.CollectRows(effectRows, pgx.RowTo[string])
	if err != nil {
		return Batch{}, err
	}
	slices.Sort(effects)
	landed, err := scanBatch(tx.QueryRow(ctx, `UPDATE batches
		SET record = $3::jsonb, record_version = record_version + 1, admission = $4, merged_commit = $5,
			updated_at = clock_timestamp()
		WHERE repo = $1 AND id = $2 RETURNING `+batchColumns,
		batch.Repo, batch.ID, string(record), admissionLanded, mergedCommit))
	if err != nil {
		return Batch{}, err
	}
	if effects == nil {
		effects = []string{}
	}
	if err := event(ctx, tx, batch.Repo, kind, batch.ID, "", map[string]any{
		"from_phase": batch.Phase, "phase": landed.Phase, "record_version": landed.RecordVersion,
		"merged_commit": mergedCommit, "members": ids, "effects": effects,
	}); err != nil {
		return Batch{}, err
	}
	return landed, escalateUnrecorded(ctx, tx, landed)
}

// landMember lands member m of batch at mergedCommit from the ready record that its admission froze.
func landMember(ctx context.Context, tx pgx.Tx, repo, batch string, m Migration, member batchMember, mergedCommit string) error {
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM events WHERE repo = $1 AND id = $2`, repo, member.readyEvent).Scan(&data); err != nil {
		return err
	}
	var ready readyEventData
	if err := json.Unmarshal(data, &ready); err != nil {
		return err
	}
	records := ready.withEmptyLists()
	names := sortedNames(records.ChangedContracts)
	contracts := make([]Contract, 0, len(names))
	for _, name := range names {
		current, err := loadContract(ctx, tx, repo, name, noLock)
		if err != nil {
			return err
		}
		contracts = append(contracts, current)
	}
	cleared := clearStatements(contracts, records)
	advances := make([]contractAdvance, 0, len(contracts))
	for _, current := range contracts {
		advances = append(advances, contractAdvance{current: current, hash: records.ChangedContracts[current.Name],
			lastChange: cleared[current.Name]})
	}
	return landMigration(ctx, tx, repo, m, advances, mergedCommit, map[string]any{
		"generation":          member.generation,
		"commit":              mergedCommit,
		"contracts":           names,
		"head":                records.Head,
		"compatibility_proof": records.CompatibilityProof,
		"reviewed_head":       records.ReviewedHead,
		"reviews":             records.Reviews,
		"upstream":            records.Upstream,
		"batch":               batch,
	})
}
