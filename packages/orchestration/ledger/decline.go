package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

const admissionDeclined = "declined"

// DeclineRequest ends batch id's admission without a landing: Reason is why, Returns are the rounds that send members
// back to their owners, and Successor is the id of the batch that the other members go to next, or empty.
// ExpectedVersion is the record version the caller read, and Record is the landing program's record for the declined
// state.
type DeclineRequest struct {
	Repo            string          `json:"repo"`
	ID              string          `json:"id"`
	Reason          string          `json:"reason"`
	Returns         []RoundRequest  `json:"returns"`
	Successor       string          `json:"successor"`
	ExpectedVersion int64           `json:"expected_version"`
	Record          json.RawMessage `json:"record"`
}

// digest is the request's identity, which a retry after a lost response repeats: the SHA-256 of the canonical JSON of
// its reason, its returns in order with every field (their keys among them), and its successor.
func (req DeclineRequest) digest() (string, error) {
	returns := req.Returns
	if returns == nil {
		returns = []RoundRequest{}
	}
	body, err := json.Marshal(struct {
		Reason    string         `json:"reason"`
		Returns   []RoundRequest `json:"returns"`
		Successor string         `json:"successor"`
	}{req.Reason, returns, req.Successor})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// DeclineBatch ends batch id's admission without landing it. In one transaction, after the locks in the batch lock
// order (the repository row, the batch row, the members' migrations by id, then the contracts they read by name):
//   - the replay: a batch already declined from a request with the same digest is returned as stored, whatever version
//     and record the request carries, because the request is the retry of the decline whose response was lost;
//   - the version: a batch at another record version than ExpectedVersion is refused with stale_batch;
//   - the release: the members are no longer admitted, so the fence no longer holds them;
//   - the returns: each one recorded, as sent, as RecordRound records it, which counts the member's round and clears
//     its ready record;
//   - the write: the caller's record at the next version, admission declined, the digest and the successor, and
//     batch.declined.
//
// A member that is not returned keeps its ready record, so the next batch forms from it. The ledger checks neither
// the returns nor the batch's later use: the landing program returns only this batch's members, and it never admits
// or saves a declined batch again.
func (s *Store) DeclineBatch(ctx context.Context, req DeclineRequest) (Batch, error) {
	digest, err := req.digest()
	if err != nil {
		return Batch{}, err
	}
	var declined Batch
	err = s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, req.Repo, true)
		if err != nil {
			return err
		}
		stored, err := loadBatch(ctx, tx, req.Repo, req.ID, true)
		if err != nil {
			return err
		}
		if stored.Admission == admissionDeclined && stored.DeclineDigest == digest {
			declined = stored
			return nil
		}
		if stored.RecordVersion != req.ExpectedVersion {
			return refuse(ReasonStaleBatch, "batch %s is at record version %d, not version %d", req.ID, stored.RecordVersion, req.ExpectedVersion)
		}
		members, err := admittedMembers(ctx, tx, req.Repo, req.ID)
		if err != nil {
			return err
		}
		ids := make([]string, len(members))
		for i, member := range members {
			ids[i] = member.migration
		}
		if _, _, err := lockMembers(ctx, tx, req.Repo, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE batch_members SET admitted = false WHERE repo = $1 AND batch = $2 AND admitted`,
			req.Repo, req.ID); err != nil {
			return err
		}
		returned := make([]string, len(req.Returns))
		for i, round := range req.Returns {
			if _, err := recordRound(ctx, tx, repository, round); err != nil {
				return err
			}
			returned[i] = round.ID
		}
		if declined, err = scanBatch(tx.QueryRow(ctx, `UPDATE batches
			SET record = $3::jsonb, record_version = record_version + 1, admission = $4, decline_digest = $5,
				successor = nullif($6, ''), updated_at = clock_timestamp()
			WHERE repo = $1 AND id = $2 RETURNING `+batchColumns,
			req.Repo, req.ID, string(req.Record), admissionDeclined, digest, req.Successor)); err != nil {
			return err
		}
		if err := event(ctx, tx, req.Repo, "batch.declined", req.ID, "", map[string]any{
			"from_phase": stored.Phase, "phase": declined.Phase, "record_version": declined.RecordVersion,
			"reason": req.Reason, "returns": returned, "successor": req.Successor, "members": ids, "decline_digest": digest,
		}); err != nil {
			return err
		}
		return escalateUnrecorded(ctx, tx, declined)
	})
	if err != nil {
		return Batch{}, err
	}
	return declined, nil
}
