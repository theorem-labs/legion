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

// TestCheckMirrorsLandRefusals exercises one candidate through both APIs and both command forms. A check request is
// the land request with nothing added, so the same candidate goes to both commands, and the CLI's request-file form
// writes one file for both. Every row names the one reason Land would refuse; Check must report that same kind.
func TestCheckMirrorsLandRefusals(t *testing.T) {
	type setup func(t *testing.T, s *ledger.Store, dsn string) ledger.CheckRequest
	tests := []struct {
		name  string
		kind  string
		setup setup
	}{
		{
			name: "invalid candidate has no changed paths",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.ChangedPaths = nil
				return candidate
			},
		},
		{
			name: "invalid candidate changes a contract without paths",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedPaths = []string{}
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "invalid candidate changes a contract without a head despite proof",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedContracts = map[string]string{"C": "hash-2"}
				candidate.CompatibilityProof = "Spec.C_compat"
				return candidate
			},
		},
		{
			name: "invalid candidate has no commit",
			kind: "invalid_candidate",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.Commit = ""
				return candidate
			},
		},
		{
			name: "migration is not open",
			kind: ledger.ReasonNotOpen,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
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
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.Generation = 0
				return candidate
			},
		},
		{
			name: "migration is escalated",
			kind: ledger.ReasonEscalated,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				if _, err := s.EnsureRepo(context.Background(), "r", 1); err != nil {
					t.Fatal(err)
				}
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				if _, err := s.RecordRound(context.Background(), ledger.RoundRequest{
					Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Key: "parity-round",
				}); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "candidate writes outside its lock",
			kind: ledger.ReasonUndeclaredWrite,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.ChangedPaths = []string{"outside.lean"}
				return candidate
			},
		},
		{
			name: "candidate has an overlapping migration",
			kind: ledger.ReasonOverlap,
			setup: func(t *testing.T, s *ledger.Store, dsn string) ledger.CheckRequest {
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
			name: "declared read has no current hash",
			kind: ledger.ReasonUnverifiedRead,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.Current = map[string]string{}
				return candidate
			},
		},
		{
			name: "contract version is stale even when the hash returned to its old value",
			kind: ledger.ReasonStaleRead,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				writer := parityOpen(t, s, "M-writer", "writer.lean", map[string]string{"C": "hash-1"})
				parityStatement(&writer, "writer-head", "writer-head")
				if _, err := s.Land(context.Background(), writer.LandRequest); err != nil {
					t.Fatal(err)
				}
				return candidate
			},
		},
		{
			name: "changed contract was undeclared",
			kind: ledger.ReasonUndeclaredContract,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "pinned contract lacks approval",
			kind: ledger.ReasonPinned,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", true)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				parityStatement(&candidate, "head-1", "head-1")
				return candidate
			},
		},
		{
			name: "statement has neither review nor proof",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				parityStatement(&candidate, "head-1", "")
				return candidate
			},
		},
		{
			name: "upstream provenance is accepted from any migration owner",
			kind: "",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				candidate := parityOpen(t, s, "M-1", "a.lean", nil)
				candidate.Upstream = "upstream-commit"
				return candidate
			},
		},
		{
			name: "one contract owner accepts one reviewed head",
			kind: "",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				parityRegister(t, s, "D", "hash-d1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1", "D": "hash-d1"})
				candidate.ChangedContracts = map[string]string{"C": "hash-2", "D": "hash-d2"}
				candidate.Head, candidate.ReviewedHead = "head-1", "head-1"
				return candidate
			},
		},
		{
			name: "two contract owners reject one reviewed head",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				parityRegisterOwner(t, s, "D", "bob", "hash-d1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1", "D": "hash-d1"})
				candidate.ChangedContracts = map[string]string{"C": "hash-2", "D": "hash-d2"}
				candidate.Head, candidate.ReviewedHead = "head-1", "head-1"
				return candidate
			},
		},
		{
			name: "two owners accept one review each",
			kind: "",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				parityRegisterOwner(t, s, "D", "bob", "hash-d1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1", "D": "hash-d1"})
				candidate.ChangedContracts, candidate.Head = map[string]string{"C": "hash-2", "D": "hash-d2"}, "head-1"
				candidate.Reviews = []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}, {Contract: "D", Reviewer: "bob", Head: "head-1"}}
				return candidate
			},
		},
		{
			name: "wrong review reviewer is refused",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedContracts, candidate.Head = map[string]string{"C": "hash-2"}, "head-1"
				candidate.Reviews = []ledger.Review{{Contract: "C", Reviewer: "bob", Head: "head-1"}}
				return candidate
			},
		},
		{
			name: "wrong review head is refused",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedContracts, candidate.Head = map[string]string{"C": "hash-2"}, "head-1"
				candidate.Reviews = []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-0"}}
				return candidate
			},
		},
		{
			name: "upstream provenance does not clear a statement",
			kind: ledger.ReasonStatementUnreviewed,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", false)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedContracts, candidate.Head, candidate.Upstream = map[string]string{"C": "hash-2"}, "head-1", "upstream-commit"
				return candidate
			},
		},
		{
			name: "pinned contract remains pinned under upstream provenance",
			kind: ledger.ReasonPinned,
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				parityRegister(t, s, "C", "hash-1", true)
				candidate := parityOpen(t, s, "M-1", "a.lean", map[string]string{"C": "hash-1"})
				candidate.ChangedContracts, candidate.Head, candidate.Upstream = map[string]string{"C": "hash-2"}, "head-1", "upstream-commit"
				candidate.Reviews = []ledger.Review{{Contract: "C", Reviewer: "alice", Head: "head-1"}}
				return candidate
			},
		},
		{
			name: "clean candidate passes",
			kind: "",
			setup: func(t *testing.T, s *ledger.Store, _ string) ledger.CheckRequest {
				return parityOpen(t, s, "M-1", "a.lean", nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, surface := range []struct {
				name string
				run  func(t *testing.T, s *ledger.Store, dsn string, candidate ledger.CheckRequest, kind string)
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
	parityRegisterOwner(t, s, name, "alice", hash, pinned)
}

func parityRegisterOwner(t *testing.T, s *ledger.Store, name, owner, hash string, pinned bool) {
	t.Helper()
	if _, err := s.RegisterContract(context.Background(), "r", name, owner, hash, pinned); err != nil {
		t.Fatal(err)
	}
}

// parityOpen opens migration id writing path and reading reads, and returns a clean candidate for it.
func parityOpen(t *testing.T, s *ledger.Store, id, path string, reads map[string]string) ledger.CheckRequest {
	t.Helper()
	m, err := s.OpenMigration(context.Background(), ledger.OpenRequest{
		Repo: "r", ID: id, Owner: "alice", BaseCommit: "base", Branch: "branch/" + id, Writes: []string{path}, Reads: reads,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := make(map[string]string, len(reads))
	for name, hash := range reads {
		current[name] = hash
	}
	return ledger.CheckRequest{LandRequest: ledger.LandRequest{
		Repo: "r", Migration: m.ID, Generation: m.Generation, Commit: "candidate", ChangedPaths: []string{path}, Current: current,
	}}
}

func parityStatement(candidate *ledger.CheckRequest, head, reviewed string) {
	candidate.ChangedContracts = map[string]string{"C": "hash-2"}
	candidate.Head, candidate.ReviewedHead = head, reviewed
}

func assertAPIParity(t *testing.T, s *ledger.Store, _ string, candidate ledger.CheckRequest, kind string) {
	t.Helper()
	result, err := s.Check(context.Background(), candidate)
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

	_, err = s.Land(context.Background(), candidate.LandRequest)
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

func assertFlagParity(t *testing.T, _ *ledger.Store, dsn string, candidate ledger.CheckRequest, kind string) {
	check := []string{"--json", "check", candidate.Repo, candidate.Migration, strconv.FormatInt(candidate.Generation, 10)}
	land := []string{"--json", "land", "--repo", candidate.Repo, "--migration", candidate.Migration,
		"--generation", strconv.FormatInt(candidate.Generation, 10)}
	flags := candidateFlags(candidate.LandRequest)
	assertCLIParity(t, dsn, kind, append(check, flags...), append(land, flags...))
}

// assertRequestFileParity writes the candidate's land request once and gives that one file to check and to land.
func assertRequestFileParity(t *testing.T, _ *ledger.Store, dsn string, candidate ledger.CheckRequest, kind string) {
	file := writeParityRequest(t, "candidate.json", candidate.LandRequest)
	assertCLIParity(t, dsn, kind, []string{"--json", "check", "--request-file", file}, []string{"--json", "land", "--request-file", file})
}

func assertCLIParity(t *testing.T, dsn, kind string, checkArgs, landArgs []string) {
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

// candidateFlags gives the flags that state a candidate, the same for check and land.
func candidateFlags(request ledger.LandRequest) []string {
	var args []string
	if request.Commit != "" {
		args = append(args, "--commit", request.Commit)
	}
	if request.ChangedPaths != nil {
		if len(request.ChangedPaths) == 0 {
			args = append(args, "--no-paths")
		}
		for _, path := range request.ChangedPaths {
			args = append(args, "--path", path)
		}
	}
	for _, name := range sortedMapKeys(request.Current) {
		args = append(args, "--current", name+"="+request.Current[name])
	}
	for _, name := range sortedMapKeys(request.ChangedContracts) {
		args = append(args, "--changed-contract", name+"="+request.ChangedContracts[name])
	}
	for _, name := range request.ApprovedPinned {
		args = append(args, "--approved-pinned", name)
	}
	for _, record := range []struct{ flag, value string }{
		{"--head", request.Head}, {"--compatibility-proof", request.CompatibilityProof},
		{"--reviewed-head", request.ReviewedHead}, {"--upstream", request.Upstream},
	} {
		if record.value != "" {
			args = append(args, record.flag, record.value)
		}
	}
	for _, review := range request.Reviews {
		args = append(args, "--review", review.Contract+"="+review.Reviewer+"@"+review.Head)
	}
	return args
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
