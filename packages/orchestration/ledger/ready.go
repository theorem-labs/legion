package ledger

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReadyRequest stores the exact landing records an owner offers for a migration.
//
// Ready refuses an upstream record unless the migration's owner is the repository's upstream owner. It checks the other
// records' shape only and stores them as given, as Land records them: Land applies the review and pinned rules when the
// migration lands.
type ReadyRequest struct {
	Repo               string            `json:"repo"`
	ID                 string            `json:"id"`
	Generation         int64             `json:"generation"`
	Head               string            `json:"head"`
	ChangedContracts   map[string]string `json:"changed_contracts"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []Review          `json:"reviews"`
	ApprovedPinned     []string          `json:"approved_pinned"`
	Upstream           string            `json:"upstream"`
}

// ReadyRecord is a ready migration's stored landing record.
type ReadyRecord struct {
	ID                 string            `json:"id"`
	Owner              string            `json:"owner"`
	Generation         int64             `json:"generation"`
	Head               string            `json:"head"`
	ReadyEvent         int64             `json:"ready_event"`
	ReadyAt            time.Time         `json:"ready_at"`
	ChangedContracts   map[string]string `json:"changed_contracts"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []Review          `json:"reviews"`
	ApprovedPinned     []string          `json:"approved_pinned"`
	Upstream           string            `json:"upstream"`
}

type readyRecords struct {
	CompatibilityProof string   `json:"compatibility_proof"`
	ReviewedHead       string   `json:"reviewed_head"`
	Reviews            []Review `json:"reviews"`
	ApprovedPinned     []string `json:"approved_pinned"`
	Upstream           string   `json:"upstream"`
}

type readyEventData struct {
	Generation       int64             `json:"generation"`
	Head             string            `json:"head"`
	ChangedContracts map[string]string `json:"changed_contracts"`
	readyRecords
}

func checkReadyValue(label, value string, required bool) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return invalid("%s is empty", label)
	}
	if trimmed != value {
		return invalid("%s %q has leading or trailing whitespace", label, value)
	}
	return nil
}

func checkReadyRequest(req ReadyRequest) error {
	if err := checkReadyValue("head", req.Head, true); err != nil {
		return err
	}
	for name, hash := range req.ChangedContracts {
		if err := checkReadyValue("changed contract name", name, true); err != nil {
			return err
		}
		if err := checkReadyValue("changed contract hash", hash, true); err != nil {
			return err
		}
	}
	for _, record := range []struct {
		label string
		value string
	}{
		{"compatibility proof", req.CompatibilityProof},
		{"reviewed head", req.ReviewedHead},
		{"upstream", req.Upstream},
	} {
		if err := checkReadyValue(record.label, record.value, false); err != nil {
			return err
		}
	}
	for _, review := range req.Reviews {
		for _, field := range []struct {
			label string
			value string
		}{
			{"review contract", review.Contract},
			{"reviewer", review.Reviewer},
			{"review head", review.Head},
		} {
			if err := checkReadyValue(field.label, field.value, true); err != nil {
				return err
			}
		}
	}
	for _, name := range req.ApprovedPinned {
		if err := checkReadyValue("approved pinned name", name, true); err != nil {
			return err
		}
	}
	return nil
}

// clearReady removes every stored ready field and reports whether a ready record was present.
// Callers invoke it from their mutation transaction before they append the event that reports the clearing.
func clearReady(ctx context.Context, tx pgx.Tx, repo, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE migrations
		SET ready_head = NULL, ready_at = NULL, ready_event = NULL, ready_changed_contracts = NULL, ready_records = NULL
		WHERE repo = $1 AND id = $2 AND ready_event IS NOT NULL`, repo, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Ready records the exact landing inputs an owner supplies for an open, current, un-escalated migration. A later
// ready replaces the previous record and has the id of its own migration.ready event. An absent list or map is
// recorded empty, so a record has one shape whether its request came from flags or from JSON.
func (s *Store) Ready(ctx context.Context, req ReadyRequest) (ReadyRecord, error) {
	if err := checkReadyRequest(req); err != nil {
		return ReadyRecord{}, err
	}
	if req.ChangedContracts == nil {
		req.ChangedContracts = map[string]string{}
	}
	if req.Reviews == nil {
		req.Reviews = []Review{}
	}
	if req.ApprovedPinned == nil {
		req.ApprovedPinned = []string{}
	}
	changedContracts, err := json.Marshal(req.ChangedContracts)
	if err != nil {
		return ReadyRecord{}, err
	}
	records := readyRecords{
		CompatibilityProof: req.CompatibilityProof,
		ReviewedHead:       req.ReviewedHead,
		Reviews:            req.Reviews,
		ApprovedPinned:     req.ApprovedPinned,
		Upstream:           req.Upstream,
	}
	storedRecords, err := json.Marshal(records)
	if err != nil {
		return ReadyRecord{}, err
	}
	ready := ReadyRecord{
		ID:                 req.ID,
		Generation:         req.Generation,
		Head:               req.Head,
		ChangedContracts:   req.ChangedContracts,
		CompatibilityProof: req.CompatibilityProof,
		ReviewedHead:       req.ReviewedHead,
		Reviews:            req.Reviews,
		ApprovedPinned:     req.ApprovedPinned,
		Upstream:           req.Upstream,
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		repository, err := lockRepo(ctx, tx, req.Repo, true)
		if err != nil {
			return err
		}
		migration, err := lockOpenAt(ctx, tx, req.Repo, req.ID, req.Generation)
		if err != nil {
			return err
		}
		if migration.EscalatedAt != nil {
			return refuse(ReasonEscalated, "migration %s was escalated to the steward: %s", req.ID, migration.EscalationReason)
		}
		if upstream, blocked := upstreamViolation(repository, migration, req.Upstream); blocked {
			return refusalForViolation(upstream)
		}
		event, err := eventRecord(ctx, tx, req.Repo, "migration.ready", req.ID, migration.Owner, readyEventData{
			Generation: req.Generation, Head: req.Head, ChangedContracts: req.ChangedContracts, readyRecords: records,
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE migrations
			SET ready_head = $3, ready_at = $4, ready_event = $5, ready_changed_contracts = $6, ready_records = $7
			WHERE repo = $1 AND id = $2
			RETURNING owner`, req.Repo, req.ID, req.Head, event.At, event.ID, string(changedContracts), string(storedRecords)).
			Scan(&ready.Owner); err != nil {
			return err
		}
		ready.ReadyEvent, ready.ReadyAt = event.ID, event.At
		return nil
	})
	return ready, err
}

func scanReadyRecord(row pgx.CollectableRow) (ReadyRecord, error) {
	var ready ReadyRecord
	var changedContracts, records []byte
	if err := row.Scan(&ready.ID, &ready.Owner, &ready.Generation, &ready.Head, &ready.ReadyEvent, &ready.ReadyAt,
		&changedContracts, &records); err != nil {
		return ReadyRecord{}, err
	}
	if err := json.Unmarshal(changedContracts, &ready.ChangedContracts); err != nil {
		return ReadyRecord{}, err
	}
	var stored readyRecords
	if err := json.Unmarshal(records, &stored); err != nil {
		return ReadyRecord{}, err
	}
	ready.CompatibilityProof = stored.CompatibilityProof
	ready.ReviewedHead = stored.ReviewedHead
	ready.Reviews = stored.Reviews
	ready.ApprovedPinned = stored.ApprovedPinned
	ready.Upstream = stored.Upstream
	return ready, nil
}

// ReadyMigrations lists every currently open migration that has a ready record, in the order that the ready events
// were appended.
func (s *Store) ReadyMigrations(ctx context.Context, repo string) ([]ReadyRecord, error) {
	ready := []ReadyRecord{}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, owner, generation, ready_head, ready_event, ready_at,
			ready_changed_contracts, ready_records
			FROM migrations
			WHERE repo = $1 AND state = 'open' AND ready_event IS NOT NULL
			ORDER BY ready_event`, repo)
		if err != nil {
			return err
		}
		ready, err = pgx.CollectRows(rows, scanReadyRecord)
		return err
	})
	return ready, err
}
