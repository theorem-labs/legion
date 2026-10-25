package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

const admissionAdmitted = "admitted"

// AdmitMember is one member of an admission: a ready migration, the id of the migration.ready event whose record the
// landing program read, and what the program computed from the candidate for it. Everything else that its Check needs
// comes from the stored ready record.
type AdmitMember struct {
	Migration     string            `json:"migration"`
	ReadyEvent    int64             `json:"ready_event"`
	CurrentHashes map[string]string `json:"current_hashes"`
	ChangedPaths  []string          `json:"changed_paths"`
}

// AdmitRequest admits a batch's members for one candidate: the base it was built on, its commit and tree, and its
// members in landing order. ExpectedVersion is the record version the caller read, and Record is the landing
// program's batch record for the admitted state.
type AdmitRequest struct {
	Repo            string          `json:"repo"`
	ID              string          `json:"id"`
	Base            string          `json:"base"`
	CandidateCommit string          `json:"candidate_commit"`
	CandidateTree   string          `json:"candidate_tree"`
	Members         []AdmitMember   `json:"members"`
	ExpectedVersion int64           `json:"expected_version"`
	Record          json.RawMessage `json:"record"`
}

// digest is the request's identity, which a retry after a lost response repeats: the SHA-256 of its canonical JSON
// without the version and the record. That is the base, the candidate commit and tree, and each member in order with
// its migration, ready event, sorted changed paths and current hashes (which JSON encodes sorted by name).
func (req AdmitRequest) digest() (string, error) {
	type member struct {
		Migration     string            `json:"migration"`
		ReadyEvent    int64             `json:"ready_event"`
		ChangedPaths  []string          `json:"changed_paths"`
		CurrentHashes map[string]string `json:"current_hashes"`
	}
	members := make([]member, 0, len(req.Members))
	for _, m := range req.Members {
		paths := slices.Clone(m.ChangedPaths)
		slices.Sort(paths)
		members = append(members, member{m.Migration, m.ReadyEvent, paths, m.CurrentHashes})
	}
	body, err := json.Marshal(struct {
		Base            string   `json:"base"`
		CandidateCommit string   `json:"candidate_commit"`
		CandidateTree   string   `json:"candidate_tree"`
		Members         []member `json:"members"`
	}{req.Base, req.CandidateCommit, req.CandidateTree, members})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// AdmitBatch admits batch id's members for the landing of one candidate, and from then on fences everything their
// admission checked until the batch lands or is declined. In one transaction, after the locks in the order that every
// batch call takes them (the repository row, the batch row, the members' migrations by id, then the contracts they read
// by name):
//   - the replay: a batch already admitted from a request with the same digest is returned as stored, whatever version
//     and record the request carries, because the request is the retry of the admission whose response was lost;
//   - the version: a batch at another record version than ExpectedVersion is refused with stale_batch;
//   - the batch: a batch admitted from another request is refused with batch_admitted;
//   - each member, in the requested order: one with no ready record is refused not_ready, and one whose ready record
//     is not ReadyEvent's is refused ready_changed; one that reads a contract that a member of another admitted batch
//     changes is refused batch_admitted; one that Check refuses, with the generation, head and statement records of its
//     ready record and the candidate's commit, paths and hashes, is refused with Check's first reason and lists all of
//     them (Check's own batch_admitted covers a member that another batch admits, and a change to a contract that an
//     admitted member reads); and one that changes a contract an earlier member reads, or reads a contract an earlier
//     member changes, is refused batch_conflict;
//   - the write: the members frozen in the requested order with their ready records' generation, head and event and
//     marked admitted, the admission and its digest, the caller's record at the next version, and batch.admitted.
//
// A refusal names the member and records nothing. Each member is checked against the ledger as it is, not as it will be
// after the earlier members land: batch_conflict refuses every member whose Check an earlier member's landing could
// change, because a landing changes only the contracts it advances and its own migration.
func (s *Store) AdmitBatch(ctx context.Context, req AdmitRequest) (Batch, error) {
	digest, err := req.digest()
	if err != nil {
		return Batch{}, err
	}
	var admitted Batch
	err = s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, req.Repo, true)
		if err != nil {
			return err
		}
		stored, err := loadBatch(ctx, tx, req.Repo, req.ID, true)
		if err != nil {
			return err
		}
		if stored.Admission == admissionAdmitted && stored.AdmissionDigest == digest {
			admitted = stored
			return nil
		}
		if stored.RecordVersion != req.ExpectedVersion {
			return refuse(ReasonStaleBatch, "batch %s is at record version %d, not version %d", req.ID, stored.RecordVersion, req.ExpectedVersion)
		}
		if stored.Admission == admissionAdmitted {
			return refuse(ReasonBatchAdmitted, "batch %s is already admitted from another request", req.ID)
		}
		frozen, err := admitMembers(ctx, tx, repository, req)
		if err != nil {
			return err
		}
		ids := make([]string, len(frozen))
		for position, member := range frozen {
			ids[position] = member.ID
			if _, err := tx.Exec(ctx, `INSERT INTO batch_members (repo, batch, migration, position, generation, head, ready_event, admitted)
				VALUES ($1, $2, $3, $4, $5, $6, $7, true)`,
				req.Repo, req.ID, member.ID, position, member.Generation, member.Head, member.ReadyEvent); err != nil {
				return err
			}
		}
		if admitted, err = scanBatch(tx.QueryRow(ctx, `UPDATE batches
			SET record = $3::jsonb, record_version = record_version + 1, admission = $4, admission_digest = $5,
				updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING `+batchColumns,
			req.Repo, req.ID, string(req.Record), admissionAdmitted, digest)); err != nil {
			return err
		}
		if err := event(ctx, tx, req.Repo, "batch.admitted", req.ID, "", map[string]any{
			"from_phase": stored.Phase, "phase": admitted.Phase, "record_version": admitted.RecordVersion,
			"base": req.Base, "candidate_commit": req.CandidateCommit, "candidate_tree": req.CandidateTree,
			"members": ids, "admission_digest": digest,
		}); err != nil {
			return err
		}
		return escalateUnrecorded(ctx, tx, admitted)
	})
	if err != nil {
		return Batch{}, err
	}
	return admitted, nil
}

// admitMembers locks the request's members and the contracts they read, in the batch lock order, and checks each member
// in the requested order. It returns their ready records in that order, or the refusal of the first member refused.
func admitMembers(ctx context.Context, tx pgx.Tx, repository Repository, req AdmitRequest) ([]ReadyRecord, error) {
	ids := make([]string, len(req.Members))
	for i, member := range req.Members {
		ids[i] = member.Migration
	}
	migrations, names, err := lockMembers(ctx, tx, req.Repo, ids)
	if err != nil {
		return nil, err
	}
	// Check refuses a member that another batch admits, and a change to a contract that an admitted member reads. What
	// it does not refuse is a read of a contract that a member of another admitted batch changes: that batch's landing
	// would then advance it under this one after the merge, and its own LandBatch would meet the fence.
	relying, err := admittedReliance(ctx, tx, req.Repo, names)
	if err != nil {
		return nil, err
	}

	readies := make([]ReadyRecord, len(req.Members))
	for i, member := range req.Members {
		m := migrations[i]
		named := func(refusal *Refusal) *Refusal {
			refusal.Member = m.ID
			return refusal
		}
		ready, ok, err := loadReady(ctx, tx, req.Repo, m.ID)
		if err != nil {
			return nil, err
		}
		switch {
		case !ok:
			return nil, named(refuse(ReasonNotReady, "member %s has no ready record: it is %s, or it moved on since it was readied", m.ID, m.State))
		case ready.ReadyEvent != member.ReadyEvent:
			return nil, named(refuse(ReasonReadyChanged, "member %s was readied again: its ready record is event %d, not event %d",
				m.ID, ready.ReadyEvent, member.ReadyEvent))
		}
		for _, name := range sortedNames(m.Reads) {
			if r, held := changer(relying[name]); held {
				refusal := named(refuse(ReasonBatchAdmitted, "member %s reads contract %s, and %s", m.ID, name, r))
				refusal.Other = r.member
				return nil, refusal
			}
		}
		cleaned, err := cleanCandidate(CheckRequest{LandRequest: LandRequest{
			Repo: req.Repo, Migration: m.ID, Generation: ready.Generation, Commit: req.CandidateCommit,
			ChangedPaths: member.ChangedPaths, Current: member.CurrentHashes, StatementRecords: ready.StatementRecords,
		}})
		if err != nil {
			return nil, err
		}
		checked, err := evaluate(ctx, tx, m, cleaned, updateLock)
		if err != nil {
			return nil, err
		}
		if violations := checked.violations; len(violations) > 0 {
			err := refusalForViolation(violations[0])
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				return nil, err
			}
			reasons := make([]string, len(violations))
			for k, v := range violations {
				reasons[k] = fmt.Sprintf("%s: %s", v.Kind, v.Message)
			}
			refusal.Message = fmt.Sprintf("member %s: %s", m.ID, strings.Join(reasons, "; "))
			refusal.Violations = violations
			return nil, named(refusal)
		}
		for j := range i {
			earlier := readies[j]
			if name, ok := contractConflict(m, ready, migrations[j], earlier); ok {
				refusal := named(refuse(ReasonBatchConflict, "members %s and %s of batch %s both rely on contract %s, and one of them changes it",
					earlier.ID, m.ID, req.ID, name))
				refusal.Other = earlier.ID
				return nil, refusal
			}
		}
		readies[i] = ready
	}
	return readies, nil
}

// lockMembers takes, after the repository and batch rows that its caller holds, the rest of the batch lock order: the
// row locks of migrations ids, sorted by id, then of the contracts they read, sorted by name. It returns the migrations
// in the order of ids, and the sorted names of the contracts they read.
func lockMembers(ctx context.Context, tx pgx.Tx, repo string, ids []string) ([]Migration, []string, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM migrations WHERE repo = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`,
		repo, ids); err != nil {
		return nil, nil, err
	}
	migrations := make([]Migration, len(ids))
	read := map[string]struct{}{}
	for i, id := range ids {
		m, err := loadMigration(ctx, tx, repo, id, false)
		if err != nil {
			return nil, nil, err
		}
		migrations[i] = m
		for name := range m.Reads {
			read[name] = struct{}{}
		}
	}
	names := sortedNames(read)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM contracts WHERE repo = $1 AND name = ANY($2) ORDER BY name FOR UPDATE`,
		repo, names); err != nil {
		return nil, nil, err
	}
	return migrations, names, nil
}

// contractConflict returns a contract that member m changes and earlier reads, or that m reads and earlier changes.
// A member's changed contracts are among its reads, because Check refuses an undeclared one.
func contractConflict(m Migration, ready ReadyRecord, earlier Migration, earlierReady ReadyRecord) (string, bool) {
	for _, name := range sortedNames(m.Reads) {
		_, changes := ready.ChangedContracts[name]
		_, reads := earlier.Reads[name]
		_, earlierChanges := earlierReady.ChangedContracts[name]
		if (changes && reads) || earlierChanges {
			return name, true
		}
	}
	return "", false
}

// loadReady reads the ready record of migration id, and reports whether it has one: it is open and readied.
func loadReady(ctx context.Context, q querier, repo, id string) (ReadyRecord, bool, error) {
	rows, err := q.Query(ctx, `SELECT m.id, m.owner, m.generation, m.ready_event, m.ready_at, m.ready_records,
			coalesce((SELECT array_agg(r.name ORDER BY r.name) FROM migration_reads r
				WHERE r.repo = m.repo AND r.migration = m.id), '{}')
		FROM migrations m
		WHERE m.repo = $1 AND m.id = $2 AND m.state = 'open' AND m.ready_event IS NOT NULL`, repo, id)
	if err != nil {
		return ReadyRecord{}, false, err
	}
	records, err := pgx.CollectRows(rows, scanReadyRecord)
	if err != nil || len(records) == 0 {
		return ReadyRecord{}, false, err
	}
	return records[0], true, nil
}
