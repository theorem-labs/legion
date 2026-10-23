package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

type landCheckCandidate struct {
	check ledger.CheckRequest
	land  ledger.LandRequest
}

// TestCheckMirrorsLandRefusals exercises the same candidate through both APIs and both command forms. Every row
// names the one reason Land would refuse; Check must report that same kind before the candidate reaches Land.
func TestCheckMirrorsLandRefusals(t *testing.T) {
	type setup func(t *testing.T, s *ledger.Store, dsn string) landCheckCandidate
	tests := []struct {
		name  string
		kind  string
		setup setup
	}{
		{
			name: "invalid candidate has no changed paths",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.check.Changed = nil
				candidate.land.ChangedPaths = nil
				return candidate
			},
		},
		{
			name: "invalid candidate changes a contract without paths",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.check.Changed = []string{}
				candidate.land.ChangedPaths = []string{}
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "invalid candidate changes a contract without a head despite proof",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.check.ChangedContracts = map[string]string{"C": "hash-2"}
				candidate.check.CompatibilityProof = "Spec.C_compat"
				candidate.land.ChangedContracts = map[string]string{"C": "hash-2"}
				candidate.land.CompatibilityProof = "Spec.C_compat"
				return candidate
			},
		},
		{
			name: "invalid candidate has no commit",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.check.Commit = ""
				candidate.land.Commit = ""
				return candidate
			},
		},
		{
			name: "migration is not open",
			kind: ledger.ReasonNotOpen,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				if _, err := s.Abandon(context.Background(), "r", "M-1", 1, "stopped"); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "migration generation is stale",
			kind: ledger.ReasonStaleGeneration,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.check.Generation = 0
				candidate.land.Generation = 0
				return candidate
			},
		},
		{
			name: "migration is escalated",
			kind: ledger.ReasonEscalated,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				if _, err := s.EnsureRepo(context.Background(), "r", 1); err != nil {
					t.Fatal(err)
				}
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				if _, err := s.RecordRound(context.Background(), "r", "M-1", 1, "build failed"); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "candidate writes outside its lock",
			kind: ledger.ReasonUndeclaredWrite,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.check.Changed = []string{"outside.lean"}
				candidate.land.ChangedPaths = []string{"outside.lean"}
				return candidate
			},
		},
		{
			name: "candidate has an overlapping migration",
			kind: ledger.ReasonOverlap,
			setup: func(t *testing.T, s *ledger.Store, dsn string) landCheckCandidate {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				conn, err := pgx.Connect(context.Background(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close(context.Background()) })
				if _, err := conn.Exec(context.Background(), `INSERT INTO migrations (repo, id, owner) VALUES ($1, $2, $3)`, "r", "M-overlap", "bob"); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec(context.Background(), `INSERT INTO migration_writes (repo, migration, path, is_prefix) VALUES ($1, $2, $3, false)`, "r", "M-overlap", "a.lean"); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "contract version is stale even when the hash returned to its old value",
			kind: ledger.ReasonStaleRead,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				writer := parityOpen(t, s, "M-writer", "writer.lean", map[string]string{"C": "hash-1"})
				parityStatement(&writer, "writer-head", "writer-head")
				if _, err := s.Land(context.Background(), writer.land); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "changed contract was undeclared",
			kind: ledger.ReasonUndeclaredContract,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "pinned contract lacks approval",
			kind: ledger.ReasonPinned,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", true)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "statement has neither review nor proof",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				parityStatement(&candidate, "head-1", "")
				return candidate
			},
		},
		{
			name: "clean candidate passes",
			kind: "",
			setup: func(t *testing.T, s *ledger.Store, _ string) landCheckCandidate {
				return parityOpen(t, s, "M-1", "a.lean", nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, surface := range []struct {
				name string
				run  func(t *testing.T, s *ledger.Store, dsn string, candidate landCheckCandidate, kind string)
			}{
				{name: "Go API", run: assertAPIParity},
				{name: "CLI flags", run: assertFlagParity},
				{name: "CLI request file", run: assertRequestFileParity},
			} {
				t.Run(surface.name, func(t *testing.T) {
					dsn := testpg.Database(t, "ledger_parity")
					s := parityStore(t, dsn)
					surface.run(t, s, dsn, test.setup(t, s, dsn), test.kind)
				})
			}
		})
	}
}

func parityStore(t *testing.T, dsn string) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.EnsureRepo(context.Background(), "r", 3); err != nil {
		t.Fatal(err)
	}
	return s
}

func parityRegister(t *testing.T, s *ledger.Store, name, hash string, pinned bool) {
	t.Helper()
	if _, err := s.RegisterContract(context.Background(), "r", name, "alice", hash, pinned); err != nil {
		t.Fatal(err)
	}
}

func parityOpen(t *testing.T, s *ledger.Store, id, path string, reads map[string]string) landCheckCandidate {
	t.Helper()
	m, err := s.OpenMigration(context.Background(), ledger.OpenRequest{
		Repo: "r", ID: id, Owner: "alice", BaseCommit: "base", Branch: "branch/" + id, Writes: []string{path}, Reads: reads,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := copyStringMap(reads)
	return landCheckCandidate{
		check: ledger.CheckRequest{
			Repo: "r", ID: m.ID, Generation: m.Generation, Commit: "candidate", Changed: []string{path}, Current: current,
		},
		land: ledger.LandRequest{
			Repo: "r", Migration: m.ID, Generation: m.Generation, Commit: "candidate", ChangedPaths: []string{path}, Current: copyStringMap(reads),
		},
	}
}

func parityStatement(candidate *landCheckCandidate, head, reviewed string) {
	candidate.check.ChangedContracts = map[string]string{"C": "hash-2"}
	candidate.check.Head, candidate.check.ReviewedHead = head, reviewed
	candidate.land.ChangedContracts = map[string]string{"C": "hash-2"}
	candidate.land.Head, candidate.land.ReviewedHead = head, reviewed
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func assertAPIParity(t *testing.T, s *ledger.Store, _ string, candidate landCheckCandidate, kind string) {
	t.Helper()
	result, err := s.Check(context.Background(), candidate.check)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "" {
		if !result.Pass || result.Outcome != ledger.CheckOutcomePass || len(result.Violations) != 0 {
			t.Fatalf("passing check: %+v", result)
		}
	} else if result.Pass || result.Outcome != ledger.CheckOutcomeRefused || len(result.Violations) != 1 || result.Violations[0].Kind != kind {
		t.Fatalf("refused check: %+v; want %s", result, kind)
	}

	_, err = s.Land(context.Background(), candidate.land)
	if kind == "" {
		if err != nil {
			t.Fatalf("passing land: %v", err)
		}
		return
	}
	var refusal *ledger.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != kind {
		t.Fatalf("refused land: %v; want %s", err, kind)
	}
}

func assertFlagParity(t *testing.T, _ *ledger.Store, dsn string, candidate landCheckCandidate, kind string) {
	assertCLIParity(t, dsn, candidate, kind, checkFlagArgs(candidate.check), landFlagArgs(candidate.land))
}

func assertRequestFileParity(t *testing.T, _ *ledger.Store, dsn string, candidate landCheckCandidate, kind string) {
	checkFile := writeParityRequest(t, "check.json", candidate.check)
	landFile := writeParityRequest(t, "land.json", candidate.land)
	assertCLIParity(t, dsn, candidate, kind, []string{"--json", "check", "--request-file", checkFile}, []string{"--json", "land", "--request-file", landFile})
}

func assertCLIParity(t *testing.T, dsn string, _ landCheckCandidate, kind string, checkArgs, landArgs []string) {
	t.Helper()
	call := func(args []string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, func(key string) string {
			if key == "LEDGER_DSN" {
				return dsn
			}
			return ""
		}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	checkCode, checkStdout, checkStderr := call(checkArgs)
	assertCheckResult(t, checkCode, checkStdout, checkStderr, kind)
	landCode, landStdout, landStderr := call(landArgs)
	assertLandResult(t, landCode, landStdout, landStderr, kind)
}

func assertCheckResult(t *testing.T, code int, stdout, stderr, kind string) {
	t.Helper()
	var result struct {
		Outcome    string `json:"outcome"`
		Pass       bool   `json:"pass"`
		Violations []struct {
			Kind string `json:"kind"`
		} `json:"violations"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("check JSON: %v; stdout=%q stderr=%q", err, stdout, stderr)
	}
	if kind == "" {
		if code != 0 || !result.Pass || result.Outcome != ledger.CheckOutcomePass || len(result.Violations) != 0 {
			t.Fatalf("passing check: code=%d result=%+v stderr=%q", code, result, stderr)
		}
		return
	}
	if code != 1 || result.Pass || result.Outcome != ledger.CheckOutcomeRefused || len(result.Violations) != 1 || result.Violations[0].Kind != kind {
		t.Fatalf("refused check: code=%d result=%+v stderr=%q; want %s", code, result, stderr, kind)
	}
}

func assertLandResult(t *testing.T, code int, stdout, stderr, kind string) {
	t.Helper()
	if kind == "" {
		if code != 0 {
			t.Fatalf("passing land: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		return
	}
	var refusal struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stdout), &refusal); err != nil || code != 1 || refusal.Reason != kind {
		t.Fatalf("refused land: code=%d reason=%q stdout=%q stderr=%q err=%v; want %s", code, refusal.Reason, stdout, stderr, err, kind)
	}
}

func checkFlagArgs(request ledger.CheckRequest) []string {
	args := []string{"--json", "check", request.Repo, request.ID, strconv.FormatInt(request.Generation, 10)}
	appendCandidateFlags(&args, request.Changed, request.Current, request.ChangedContracts, request.Commit, request.ApprovedPinned,
		request.DeferReads, request.Head, request.CompatibilityProof, request.ReviewedHead)
	return args
}

func landFlagArgs(request ledger.LandRequest) []string {
	args := []string{"--json", "land", "--repo", request.Repo, "--migration", request.Migration, "--generation", strconv.FormatInt(request.Generation, 10)}
	appendCandidateFlags(&args, request.ChangedPaths, request.Current, request.ChangedContracts, request.Commit, request.ApprovedPinned,
		false, request.Head, request.CompatibilityProof, request.ReviewedHead)
	return args
}

func appendCandidateFlags(args *[]string, changed []string, current, changedContracts map[string]string, commit string, approved []string, deferReads bool, head, proof, reviewed string) {
	if commit != "" {
		*args = append(*args, "--commit", commit)
	}
	if changed != nil {
		if len(changed) == 0 {
			*args = append(*args, "--no-paths")
		} else {
			for _, path := range changed {
				*args = append(*args, "--path", path)
			}
		}
	}
	for _, name := range sortedMapKeys(current) {
		*args = append(*args, "--current", name+"="+current[name])
	}
	for _, name := range sortedMapKeys(changedContracts) {
		*args = append(*args, "--changed-contract", name+"="+changedContracts[name])
	}
	for _, name := range approved {
		*args = append(*args, "--approved-pinned", name)
	}
	if deferReads {
		*args = append(*args, "--defer-reads")
	}
	for _, record := range []struct{ flag, value string }{
		{"--head", head}, {"--compatibility-proof", proof}, {"--reviewed-head", reviewed},
	} {
		if record.value != "" {
			*args = append(*args, record.flag, record.value)
		}
	}
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeParityRequest(t *testing.T, name string, request any) string {
	t.Helper()
	contents, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
