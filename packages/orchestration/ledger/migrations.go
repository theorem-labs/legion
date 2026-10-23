package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migration states.
const (
	StateOpen      = "open"
	StateLanded    = "landed"
	StateAbandoned = "abandoned"
)

// Kinds of check violation. Any violation means the candidate must not land.
const (
	ViolationNotOpen         = "not_open"         // the migration has landed or been abandoned
	ViolationStaleGeneration = "stale_generation" // the candidate comes from a generation that was taken over
	ViolationEscalated       = "escalated"        // it used up its landing rounds; the steward decides first
	ViolationUndeclaredWrite = "undeclared_write" // a changed path outside the declared writes
	ViolationStaleRead       = "stale_read"       // a read's statement hash changed, or the statement is gone
	ViolationUnverifiedRead  = "unverified_read"  // no current hash was supplied for a read, so it cannot be decided
	ViolationOverlap         = "overlap"          // the declared writes overlap another open migration's
)

// OpenRequest declares a migration: what it writes (files, or directories ending in "/", new files included)
// and the statement hash of each declaration it read.
type OpenRequest struct {
	Repo       string
	ID         string
	Owner      string
	BaseCommit string
	Branch     string
	Writes     []string
	Reads      map[string]string
}

// Migration is a migration's record. Writes and Reads are its declared change lock.
type Migration struct {
	ID               string            `json:"id"`
	Owner            string            `json:"owner"`
	BaseCommit       string            `json:"base_commit"`
	Branch           string            `json:"branch"`
	State            string            `json:"state"`
	Generation       int64             `json:"generation"`
	LandingRounds    int               `json:"landing_rounds"`
	EscalatedAt      *time.Time        `json:"escalated_at,omitempty"`
	EscalationReason string            `json:"escalation_reason,omitempty"`
	OpenedAt         time.Time         `json:"opened_at"`
	ClosedAt         *time.Time        `json:"closed_at,omitempty"`
	CloseReason      string            `json:"close_reason,omitempty"`
	LandedCommit     string            `json:"landed_commit,omitempty"`
	Writes           []string          `json:"writes"`
	Reads            map[string]string `json:"reads"`
	ReadVersions     map[string]*int64 `json:"read_versions,omitempty"`
}

// CheckRequest asks whether a candidate change may land for a migration.
type CheckRequest struct {
	Repo       string
	ID         string
	Generation int64
	// Changed is every path the candidate adds, modifies or deletes, relative to the repository root.
	Changed []string
	// Current maps a read's name to its statement hash in the candidate's base. An empty hash means the statement
	// no longer exists; a name missing from the map was not hashed, so that read cannot be decided.
	Current map[string]string
}

// Violation is one reason a candidate must not land.
type Violation struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Name     string `json:"name,omitempty"`
	Recorded string `json:"recorded,omitempty"`
	Current  string `json:"current,omitempty"`
	Other    string `json:"other,omitempty"`
	Message  string `json:"message"`
}

// CheckResult is the landing decision. Pass is true only with no violations.
type CheckResult struct {
	Migration  string      `json:"migration"`
	Generation int64       `json:"generation"`
	Pass       bool        `json:"pass"`
	Violations []Violation `json:"violations"`
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const migrationColumns = `id, owner, base_commit, branch, state, generation, landing_rounds, escalated_at,
	coalesce(escalation_reason, ''), opened_at, closed_at, coalesce(close_reason, ''), coalesce(landed_commit, '')`

func scanMigration(row pgx.Row) (Migration, error) {
	var m Migration
	err := row.Scan(&m.ID, &m.Owner, &m.BaseCommit, &m.Branch, &m.State, &m.Generation, &m.LandingRounds,
		&m.EscalatedAt, &m.EscalationReason, &m.OpenedAt, &m.ClosedAt, &m.CloseReason, &m.LandedCommit)
	return m, err
}

// loadMigration reads a migration with its declared writes and reads; forUpdate takes its row lock.
func loadMigration(ctx context.Context, q querier, repo, id string, forUpdate bool) (Migration, error) {
	sql := `SELECT ` + migrationColumns + ` FROM migrations WHERE repo = $1 AND id = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	m, err := scanMigration(q.QueryRow(ctx, sql, repo, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, &NotFoundError{What: "migration", ID: id}
	}
	if err != nil {
		return m, err
	}
	ws, err := migrationWrites(ctx, q, repo, id)
	if err != nil {
		return m, err
	}
	for _, w := range ws {
		m.Writes = append(m.Writes, w.String())
	}
	m.Reads = map[string]string{}
	m.ReadVersions = map[string]*int64{}
	rows, err := q.Query(ctx, `SELECT name, hash, version FROM migration_reads WHERE repo = $1 AND migration = $2`, repo, id)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, h string
		var version *int64
		if err := rows.Scan(&n, &h, &version); err != nil {
			return m, err
		}
		m.Reads[n] = h
		m.ReadVersions[n] = version
	}
	return m, rows.Err()
}

func migrationWrites(ctx context.Context, q querier, repo, id string) ([]writePath, error) {
	rows, err := q.Query(ctx, `SELECT path, is_prefix FROM migration_writes WHERE repo = $1 AND migration = $2
		ORDER BY path, is_prefix`, repo, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (writePath, error) {
		var w writePath
		err := r.Scan(&w.Path, &w.Prefix)
		return w, err
	})
}

// openWriteSets returns every open migration's declared writes except skip's, keyed by migration.
func openWriteSets(ctx context.Context, q querier, repo, skip string) (map[string][]writePath, error) {
	rows, err := q.Query(ctx, `SELECT w.migration, w.path, w.is_prefix FROM migration_writes w
		JOIN migrations m ON m.repo = w.repo AND m.id = w.migration
		WHERE w.repo = $1 AND m.state = 'open' AND m.id <> $2`, repo, skip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]writePath{}
	for rows.Next() {
		var id string
		var w writePath
		if err := rows.Scan(&id, &w.Path, &w.Prefix); err != nil {
			return nil, err
		}
		out[id] = append(out[id], w)
	}
	return out, rows.Err()
}

// firstOverlap finds the first open migration (by name) whose writes overlap ws.
func firstOverlap(sets map[string][]writePath, ws []writePath) (string, []PathClash) {
	ids := make([]string, 0, len(sets))
	for id := range sets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if c := clashes(ws, sets[id]); len(c) > 0 {
			return id, c
		}
	}
	return "", nil
}

func overlapRefusal(other string, c []PathClash) *Refusal {
	parts := make([]string, 0, len(c))
	for _, x := range c {
		parts = append(parts, x.Ours+" overlaps "+x.Theirs)
	}
	r := refuse(ReasonOverlap, "open migration %s holds overlapping paths: %s", other, strings.Join(parts, ", "))
	r.Other, r.Overlaps = other, c
	return r
}

func checkReads(reads map[string]string) error {
	for n, h := range reads {
		if strings.TrimSpace(n) == "" {
			return invalid("a read has an empty name")
		}
		if strings.TrimSpace(h) == "" {
			return invalid("read %s has no hash", n)
		}
	}
	return nil
}

func insertWrites(ctx context.Context, tx pgx.Tx, repo, id string, ws []writePath) error {
	for _, w := range ws {
		if _, err := tx.Exec(ctx, `INSERT INTO migration_writes (repo, migration, path, is_prefix) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, repo, id, w.Path, w.Prefix); err != nil {
			return err
		}
	}
	return nil
}

func contractReadVersion(ctx context.Context, tx pgx.Tx, repo, name string) (*int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `SELECT version FROM contracts WHERE repo = $1 AND name = $2`, repo, name).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func upsertReads(ctx context.Context, tx pgx.Tx, repo, id string, reads map[string]string) error {
	for n, h := range reads {
		version, err := contractReadVersion(ctx, tx, repo, n)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO migration_reads (repo, migration, name, hash, version) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (repo, migration, name) DO UPDATE SET hash = EXCLUDED.hash, version = EXCLUDED.version`,
			repo, id, n, h, version); err != nil {
			return err
		}
	}
	return nil
}

func writeStrings(ws []writePath) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.String()
	}
	return out
}

// OpenMigration records a new open migration and its change lock at generation 1. It is refused, naming the
// other migration and the overlapping paths, when a declared write overlaps an open migration's in either
// direction (a directory containing the other's file, or a file inside the other's directory).
func (s *Store) OpenMigration(ctx context.Context, req OpenRequest) (Migration, error) {
	if req.ID == "" || req.Owner == "" {
		return Migration{}, invalid("a migration needs a name and an owner")
	}
	if len(req.Writes) == 0 {
		return Migration{}, invalid("migration %s declares no writes", req.ID)
	}
	ws, err := parseWrites(req.Writes)
	if err != nil {
		return Migration{}, err
	}
	if err := checkReads(req.Reads); err != nil {
		return Migration{}, err
	}
	var m Migration
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, req.Repo, true); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migrations WHERE repo = $1 AND id = $2)`,
			req.Repo, req.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return refuse(ReasonExists, "migration %s already exists; a new migration needs a new name", req.ID)
		}
		sets, err := openWriteSets(ctx, tx, req.Repo, req.ID)
		if err != nil {
			return err
		}
		if other, c := firstOverlap(sets, ws); other != "" {
			return overlapRefusal(other, c)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO migrations (repo, id, owner, base_commit, branch) VALUES ($1, $2, $3, $4, $5)`,
			req.Repo, req.ID, req.Owner, req.BaseCommit, req.Branch); err != nil {
			return err
		}
		if err := insertWrites(ctx, tx, req.Repo, req.ID, ws); err != nil {
			return err
		}
		if err := upsertReads(ctx, tx, req.Repo, req.ID, req.Reads); err != nil {
			return err
		}
		if err := event(ctx, tx, req.Repo, "migration.opened", req.ID, req.Owner, map[string]any{
			"base_commit": req.BaseCommit, "branch": req.Branch, "writes": writeStrings(ws), "reads": req.Reads,
		}); err != nil {
			return err
		}
		m, err = loadMigration(ctx, tx, req.Repo, req.ID, false)
		return err
	})
	return m, err
}

// lockOpenAt loads a migration under its row lock and refuses it unless it is open at generation gen.
func lockOpenAt(ctx context.Context, tx pgx.Tx, repo, id string, gen int64) (Migration, error) {
	m, err := loadMigration(ctx, tx, repo, id, true)
	if err != nil {
		return m, err
	}
	if m.State != StateOpen {
		return m, refuse(ReasonNotOpen, "migration %s is %s", id, m.State)
	}
	if gen != m.Generation {
		return m, refuse(ReasonStaleGeneration, "migration %s is at generation %d (owner %s), not %d",
			id, m.Generation, m.Owner, gen)
	}
	return m, nil
}

// ExtendMigration adds declared writes and reads to an open migration, under the same overlap rule as
// OpenMigration. A read already declared takes the new hash.
func (s *Store) ExtendMigration(ctx context.Context, repo, id string, gen int64, writes []string, reads map[string]string) (Migration, error) {
	ws, err := parseWrites(writes)
	if err != nil {
		return Migration{}, err
	}
	if err := checkReads(reads); err != nil {
		return Migration{}, err
	}
	if len(ws) == 0 && len(reads) == 0 {
		return Migration{}, invalid("nothing to add to migration %s", id)
	}
	var m Migration
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, true); err != nil {
			return err
		}
		cur, err := lockOpenAt(ctx, tx, repo, id, gen)
		if err != nil {
			return err
		}
		sets, err := openWriteSets(ctx, tx, repo, id)
		if err != nil {
			return err
		}
		if other, c := firstOverlap(sets, ws); other != "" {
			return overlapRefusal(other, c)
		}
		if err := insertWrites(ctx, tx, repo, id, ws); err != nil {
			return err
		}
		if err := upsertReads(ctx, tx, repo, id, reads); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "migration.extended", id, cur.Owner, map[string]any{
			"generation": gen, "writes": writeStrings(ws), "reads": reads,
		}); err != nil {
			return err
		}
		m, err = loadMigration(ctx, tx, repo, id, false)
		return err
	})
	return m, err
}

// TakeOver gives an open migration to a new owner at generation gen+1. gen must be the current generation, so
// of two replacements that read the same record only one wins; every later call carrying gen is refused.
func (s *Store) TakeOver(ctx context.Context, repo, id, owner string, gen int64) (Migration, error) {
	if owner == "" {
		return Migration{}, invalid("empty owner")
	}
	var m Migration
	err := s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := lockOpenAt(ctx, tx, repo, id, gen)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE migrations SET owner = $3, generation = generation + 1 WHERE repo = $1 AND id = $2`,
			repo, id, owner); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "migration.taken_over", id, owner, map[string]any{
			"from_owner": cur.Owner, "from_generation": gen, "generation": gen + 1,
		}); err != nil {
			return err
		}
		m, err = loadMigration(ctx, tx, repo, id, false)
		return err
	})
	return m, err
}

// cleanCheckRequest validates and normalizes candidate paths before they are compared to declared writes.
func cleanCheckRequest(req CheckRequest) (CheckRequest, error) {
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
		add(Violation{Kind: ViolationNotOpen, Message: fmt.Sprintf("migration %s is %s", m.ID, m.State)})
	}
	if req.Generation != m.Generation {
		add(Violation{Kind: ViolationStaleGeneration, Message: fmt.Sprintf(
			"candidate is from generation %d; migration %s is at generation %d (owner %s)",
			req.Generation, m.ID, m.Generation, m.Owner)})
	}
	if m.EscalatedAt != nil {
		add(Violation{Kind: ViolationEscalated, Message: fmt.Sprintf(
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
		v := Violation{Kind: ViolationUndeclaredWrite, Path: path,
			Message: fmt.Sprintf("%s is outside migration %s's declared writes", path, m.ID)}
		if other := holderOf(sets, path); other != "" {
			v.Other = other
			v.Message += "; open migration " + other + " declares it"
		}
		add(v)
	}
	names := sortedNames(m.Reads)
	for _, name := range names {
		recorded := m.Reads[name]
		current, ok := req.Current[name]
		switch {
		case !ok:
			add(Violation{Kind: ViolationUnverifiedRead, Name: name, Recorded: recorded,
				Message: fmt.Sprintf("no current hash for read %s, so it cannot be checked", name)})
		case current == "":
			add(Violation{Kind: ViolationStaleRead, Name: name, Recorded: recorded,
				Message: fmt.Sprintf("read %s no longer exists (recorded hash %s)", name, recorded)})
		case current != recorded:
			add(Violation{Kind: ViolationStaleRead, Name: name, Recorded: recorded, Current: current,
				Message: fmt.Sprintf("read %s changed: recorded %s, now %s", name, recorded, current)})
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
				add(Violation{Kind: ViolationOverlap, Path: clash.Ours, Other: id,
					Message: fmt.Sprintf("declared %s overlaps open migration %s's %s", clash.Ours, id, clash.Theirs)})
			}
		}
	}
	return violations, nil
}

// Check decides whether a candidate change may land for a migration. It writes nothing but the event that
// records its decision. A refusal of the request itself (an unknown migration, a bad path) is an error; every
// reason the candidate must not land is a violation in the result.
func (s *Store) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	cleaned, err := cleanCheckRequest(req)
	if err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{Migration: cleaned.ID, Generation: cleaned.Generation}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		res.Violations = nil
		if _, err := lockRepo(ctx, tx, cleaned.Repo, false); err != nil {
			return err
		}
		migration, err := loadMigration(ctx, tx, cleaned.Repo, cleaned.ID, true)
		if err != nil {
			return err
		}
		res.Violations, err = checkViolations(ctx, tx, migration, cleaned)
		if err != nil {
			return err
		}
		res.Pass = len(res.Violations) == 0
		kinds := make([]string, 0, len(res.Violations))
		for _, violation := range res.Violations {
			kinds = append(kinds, violation.Kind)
		}
		return event(ctx, tx, cleaned.Repo, "migration.checked", cleaned.ID, "", map[string]any{
			"generation": cleaned.Generation, "pass": res.Pass, "changed": len(cleaned.Changed), "violations": kinds,
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

// RecordRound records one landing round the migration failed (its candidate was sent back). When the
// repository's limit is set and the migration has now used that many rounds, it is escalated to the steward.
func (s *Store) RecordRound(ctx context.Context, repo, id string, gen int64, reason string) (Migration, error) {
	var m Migration
	err := s.tx(ctx, func(tx pgx.Tx) error {
		r, err := lockRepo(ctx, tx, repo, false)
		if err != nil {
			return err
		}
		cur, err := lockOpenAt(ctx, tx, repo, id, gen)
		if err != nil {
			return err
		}
		rounds := cur.LandingRounds + 1
		if _, err := tx.Exec(ctx, `UPDATE migrations SET landing_rounds = $3 WHERE repo = $1 AND id = $2`, repo, id, rounds); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "migration.round_failed", id, "", map[string]any{
			"generation": gen, "landing_rounds": rounds, "reason": reason,
		}); err != nil {
			return err
		}
		if r.MaxLandingRounds > 0 && rounds >= r.MaxLandingRounds && cur.EscalatedAt == nil {
			why := fmt.Sprintf("failed %d of %d landing rounds; last: %s", rounds, r.MaxLandingRounds, reason)
			if err := escalate(ctx, tx, repo, id, why); err != nil {
				return err
			}
		}
		m, err = loadMigration(ctx, tx, repo, id, false)
		return err
	})
	return m, err
}

func escalate(ctx context.Context, tx pgx.Tx, repo, id, why string) error {
	if _, err := tx.Exec(ctx, `UPDATE migrations SET escalated_at = clock_timestamp(), escalation_reason = $3
		WHERE repo = $1 AND id = $2`, repo, id, why); err != nil {
		return err
	}
	return event(ctx, tx, repo, "migration.escalated", id, "", map[string]any{"reason": why})
}

// Escalate flags for the steward the named open migration, or with id "" every open one, that has failed at
// least maxRounds landing rounds (0 means the repository's limit). It stays open; Check refuses it until the
// steward retries it or closes it. It returns the migrations it newly flagged. A named migration below the limit
// is refused.
func (s *Store) Escalate(ctx context.Context, repo, id string, maxRounds int) ([]Migration, error) {
	var out []Migration
	err := s.tx(ctx, func(tx pgx.Tx) error {
		out = nil
		r, err := lockRepo(ctx, tx, repo, false)
		if err != nil {
			return err
		}
		n := maxRounds
		if n <= 0 {
			n = r.MaxLandingRounds
		}
		if n <= 0 {
			return invalid("no landing-round limit: give one, or set the repository's")
		}
		var ids []string
		if id != "" {
			m, err := loadMigration(ctx, tx, repo, id, true)
			if err != nil {
				return err
			}
			if m.State != StateOpen {
				return refuse(ReasonNotOpen, "migration %s is %s", id, m.State)
			}
			if m.LandingRounds < n {
				return refuse(ReasonBelowLimit, "migration %s has failed %d landing rounds, below %d", id, m.LandingRounds, n)
			}
			if m.EscalatedAt == nil {
				ids = []string{id}
			}
		} else {
			rows, err := tx.Query(ctx, `SELECT id FROM migrations WHERE repo = $1 AND state = 'open'
				AND escalated_at IS NULL AND landing_rounds >= $2 ORDER BY id FOR UPDATE`, repo, n)
			if err != nil {
				return err
			}
			if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return err
			}
		}
		for _, mid := range ids {
			var rounds int
			if err := tx.QueryRow(ctx, `SELECT landing_rounds FROM migrations WHERE repo = $1 AND id = $2`, repo, mid).Scan(&rounds); err != nil {
				return err
			}
			if err := escalate(ctx, tx, repo, mid, fmt.Sprintf("failed %d landing rounds, limit %d", rounds, n)); err != nil {
				return err
			}
			m, err := loadMigration(ctx, tx, repo, mid, false)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	if out == nil {
		out = []Migration{}
	}
	return out, err
}

// Retry is the steward's answer to an escalation: the flag clears and the landing-round count starts again.
func (s *Store) Retry(ctx context.Context, repo, id string) (Migration, error) {
	var m Migration
	err := s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := loadMigration(ctx, tx, repo, id, true)
		if err != nil {
			return err
		}
		if cur.State != StateOpen {
			return refuse(ReasonNotOpen, "migration %s is %s", id, cur.State)
		}
		if cur.EscalatedAt == nil {
			return refuse(ReasonNotEscalated, "migration %s is not escalated", id)
		}
		if _, err := tx.Exec(ctx, `UPDATE migrations SET escalated_at = NULL, escalation_reason = NULL, landing_rounds = 0
			WHERE repo = $1 AND id = $2`, repo, id); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "migration.retried", id, "", map[string]any{"landing_rounds": cur.LandingRounds}); err != nil {
			return err
		}
		m, err = loadMigration(ctx, tx, repo, id, false)
		return err
	})
	return m, err
}

// Abandon ends an open migration without landing it, releasing all its declared writes at once. The declarations
// stay on the record as history.
func (s *Store) Abandon(ctx context.Context, repo, id string, gen int64, reason string) (Migration, error) {
	var m Migration
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRepo(ctx, tx, repo, true); err != nil {
			return err
		}
		cur, err := lockOpenAt(ctx, tx, repo, id, gen)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE migrations SET state = $3, closed_at = clock_timestamp(), close_reason = $4
			WHERE repo = $1 AND id = $2`, repo, id, StateAbandoned, reason); err != nil {
			return err
		}
		if err := event(ctx, tx, repo, "migration.abandoned", id, cur.Owner, map[string]any{
			"generation": gen, "reason": reason, "released": cur.Writes,
		}); err != nil {
			return err
		}
		m, err = loadMigration(ctx, tx, repo, id, false)
		return err
	})
	return m, err
}

// Migration reads one migration's record from a read-only repeatable-read snapshot.
func (s *Store) Migration(ctx context.Context, repo, id string) (Migration, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Migration{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	migration, err := loadMigration(ctx, tx, repo, id, false)
	if err != nil {
		return Migration{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Migration{}, err
	}
	return migration, nil
}

// migrations lists a repository's migrations in a state, by name.
func migrations(ctx context.Context, q querier, repo, state string) ([]Migration, error) {
	rows, err := q.Query(ctx, `SELECT id FROM migrations WHERE repo = $1 AND state = $2 ORDER BY id`, repo, state)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := []Migration{}
	for _, id := range ids {
		m, err := loadMigration(ctx, q, repo, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
