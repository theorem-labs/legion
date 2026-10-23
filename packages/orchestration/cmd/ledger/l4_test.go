package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

func TestL4RepoEnsureConfiguresAndClearsUpstreamOwner(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "1", "--upstream-owner", "upstream")
	if got := l4UpstreamOwner(t, call); got != "upstream" {
		t.Fatalf("upstream owner %q, want upstream", got)
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "2")
	if got := l4UpstreamOwner(t, call); got != "upstream" {
		t.Fatalf("upstream owner changed without its flag: %q", got)
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "2", "--upstream-owner", "")
	if got := l4UpstreamOwner(t, call); got != "" {
		t.Fatalf("upstream owner %q after clearing", got)
	}
}

func TestL4ReadyRejectsNonUpstreamOwnersInBothCLIForms(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "0", "--upstream-owner", "upstream")
	mustL4Code(t, call, 0, "open", "r", "M-flag", "alice", "base", "branch", "--write", "a.lean")
	assertL4Refusal(t, call, []string{"--json", "ready", "r", "M-flag", "1", "--head", "head", "--upstream", "upstream-commit"})

	mustL4Code(t, call, 0, "open", "r", "M-file", "alice", "base", "branch", "--write", "b.lean")
	file := filepath.Join(t.TempDir(), "ready.json")
	request := `{"repo":"r","id":"M-file","generation":1,"head":"head","upstream":"upstream-commit"}`
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	assertL4Refusal(t, call, []string{"--json", "ready", "--request-file", file})

	mustL4Code(t, call, 0, "open", "r", "M-allowed", "upstream", "base", "branch", "--write", "c.lean")
	mustL4Code(t, call, 0, "ready", "r", "M-allowed", "1", "--head", "head", "--upstream", "upstream-commit")
}

func TestL4ContractListShowsProofReviewAndUpstreamRecords(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "0", "--upstream-owner", "upstream")

	mustL4Code(t, call, 0, "contract", "register", "r", "Proof", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-proof", "alice", "base", "branch", "--write", "a.lean", "--read", "Proof=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-proof", "--generation", "1", "--commit", "commit-proof",
		"--path", "a.lean", "--current", "Proof=hash-1", "--changed-contract", "Proof=hash-2", "--head", "head-proof", "--compatibility-proof", "Spec.Proof.compat")

	mustL4Code(t, call, 0, "contract", "register", "r", "Review", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-review", "alice", "base", "branch", "--write", "b.lean", "--read", "Review=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-review", "--generation", "1", "--commit", "commit-review",
		"--path", "b.lean", "--current", "Review=hash-1", "--changed-contract", "Review=hash-2", "--head", "head-review", "--review", "Review=alice@head-review")

	mustL4Code(t, call, 0, "contract", "register", "r", "Upstream", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-upstream", "upstream", "base", "branch", "--write", "c.lean", "--read", "Upstream=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-upstream", "--generation", "1", "--commit", "commit-upstream",
		"--path", "c.lean", "--current", "Upstream=hash-1", "--changed-contract", "Upstream=hash-2", "--head", "head-upstream", "--review", "Upstream=alice@head-upstream", "--upstream", "upstream-commit")

	mustL4Code(t, call, 0, "contract", "register", "r", "ReviewedHead", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-reviewed-head", "alice", "base", "branch", "--write", "d.lean", "--read", "ReviewedHead=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-reviewed-head", "--generation", "1", "--commit", "commit-reviewed-head",
		"--path", "d.lean", "--current", "ReviewedHead=hash-1", "--changed-contract", "ReviewedHead=hash-2", "--head", "head-reviewed-head", "--reviewed-head", "head-reviewed-head")

	mustL4Code(t, call, 0, "contract", "register", "r", "UpstreamReviewedHead", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-upstream-reviewed-head", "upstream", "base", "branch", "--write", "e.lean", "--read", "UpstreamReviewedHead=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-upstream-reviewed-head", "--generation", "1", "--commit", "commit-upstream-reviewed-head",
		"--path", "e.lean", "--current", "UpstreamReviewedHead=hash-1", "--changed-contract", "UpstreamReviewedHead=hash-2", "--head", "head-upstream-reviewed-head", "--reviewed-head", "head-upstream-reviewed-head", "--upstream", "upstream-commit")

	mustL4Code(t, call, 0, "contract", "register", "r", "UpstreamUnrelated", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-upstream-unrelated", "upstream", "base", "branch", "--write", "f.lean", "--read", "UpstreamUnrelated=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-upstream-unrelated", "--generation", "1", "--commit", "commit-upstream-unrelated",
		"--path", "f.lean", "--current", "UpstreamUnrelated=hash-1", "--changed-contract", "UpstreamUnrelated=hash-2", "--head", "head-unrelated",
		"--reviewed-head", "head-unrelated", "--review", "Other=bob@head-unrelated", "--upstream", "upstream-commit")

	mustL4Code(t, call, 0, "contract", "register", "r", "UpstreamExplicit", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-upstream-explicit", "upstream", "base", "branch", "--write", "g.lean", "--read", "UpstreamExplicit=hash-1")
	mustL4Code(t, call, 0, "land", "--repo", "r", "--migration", "M-upstream-explicit", "--generation", "1", "--commit", "commit-upstream-explicit",
		"--path", "g.lean", "--current", "UpstreamExplicit=hash-1", "--changed-contract", "UpstreamExplicit=hash-2", "--head", "head-explicit",
		"--reviewed-head", "head-explicit", "--review", "Other=bob@head-explicit", "--review", "UpstreamExplicit=alice@head-explicit", "--upstream", "upstream-commit")

	code, stdout, stderr := call("--json", "contract", "list", "r")
	if code != 0 {
		t.Fatalf("contract list: code=%d stderr=%q", code, stderr)
	}
	var contracts []struct {
		Name       string          `json:"name"`
		LastChange json.RawMessage `json:"last_change"`
	}
	if err := json.Unmarshal([]byte(stdout), &contracts); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, contract := range contracts {
		var change any
		if err := json.Unmarshal(contract.LastChange, &change); err != nil {
			t.Fatalf("%s last change %q: %v", contract.Name, contract.LastChange, err)
		}
		got[contract.Name] = change
	}
	want := map[string]any{
		"Proof":                map[string]any{"proof": "Spec.Proof.compat"},
		"Review":               map[string]any{"review": map[string]any{"contract": "Review", "reviewer": "alice", "head": "head-review"}},
		"Upstream":             map[string]any{"upstream": "upstream-commit", "reviews": []any{map[string]any{"contract": "Upstream", "reviewer": "alice", "head": "head-upstream"}}},
		"ReviewedHead":         map[string]any{"review": map[string]any{"contract": "ReviewedHead", "reviewer": "alice", "head": "head-reviewed-head"}},
		"UpstreamReviewedHead": map[string]any{"upstream": "upstream-commit", "reviews": []any{map[string]any{"contract": "UpstreamReviewedHead", "reviewer": "alice", "head": "head-upstream-reviewed-head"}}},
		"UpstreamUnrelated":    map[string]any{"upstream": "upstream-commit", "reviews": []any{map[string]any{"contract": "UpstreamUnrelated", "reviewer": "alice", "head": "head-unrelated"}}},
		"UpstreamExplicit":     map[string]any{"upstream": "upstream-commit", "reviews": []any{map[string]any{"contract": "UpstreamExplicit", "reviewer": "alice", "head": "head-explicit"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contract list changes %#v, want %#v", got, want)
	}
}

// A mixed-owner landing through the command line, with both --reviewed-head and each owner's --review, is accepted, and
// contract list shows each owner's own review as its contract's last_change.
func TestL4MixedOwnerLandingRecordsEachExplicitReview(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "0")
	mustL4Code(t, call, 0, "contract", "register", "r", "C", "alice", "hash-1", "false")
	mustL4Code(t, call, 0, "contract", "register", "r", "D", "bob", "hash-1", "false")
	mustL4Code(t, call, 0, "open", "r", "M-mixed", "alice", "base", "branch", "--write", "a.lean", "--read", "C=hash-1", "--read", "D=hash-1")
	candidate := []string{"--path", "a.lean", "--current", "C=hash-1", "--current", "D=hash-1",
		"--changed-contract", "C=hash-2", "--changed-contract", "D=hash-2", "--head", "head-1", "--reviewed-head", "head-1",
		"--review", "D=bob@head-1", "--review", "C=alice@head-1"}
	mustL4Code(t, call, 0, append([]string{"--json", "check", "r", "M-mixed", "1", "--commit", "commit-1"}, candidate...)...)
	mustL4Code(t, call, 0, append([]string{"land", "--repo", "r", "--migration", "M-mixed", "--generation", "1", "--commit", "commit-1"}, candidate...)...)

	code, stdout, stderr := call("--json", "contract", "list", "r")
	if code != 0 {
		t.Fatalf("contract list: code=%d stderr=%q", code, stderr)
	}
	var contracts []struct {
		Name       string          `json:"name"`
		LastChange json.RawMessage `json:"last_change"`
	}
	if err := json.Unmarshal([]byte(stdout), &contracts); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, contract := range contracts {
		var change any
		if err := json.Unmarshal(contract.LastChange, &change); err != nil {
			t.Fatalf("%s last change %q: %v", contract.Name, contract.LastChange, err)
		}
		got[contract.Name] = change
	}
	want := map[string]any{
		"C": map[string]any{"review": map[string]any{"contract": "C", "reviewer": "alice", "head": "head-1"}},
		"D": map[string]any{"review": map[string]any{"contract": "D", "reviewer": "bob", "head": "head-1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contract list changes %#v, want %#v", got, want)
	}
}

type l4Call func(args ...string) (int, string, string)

func l4Command(t *testing.T) l4Call {
	t.Helper()
	dsn := testpg.Database(t, "ledger_l4_command")
	return func(args ...string) (int, string, string) {
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
}

func mustL4Code(t *testing.T, call l4Call, want int, args ...string) {
	t.Helper()
	if code, _, stderr := call(args...); code != want {
		t.Fatalf("%v: code=%d stderr=%q, want %d", args, code, stderr, want)
	}
}

func l4UpstreamOwner(t *testing.T, call l4Call) string {
	t.Helper()
	code, stdout, stderr := call("--json", "status", "r")
	if code != 0 {
		t.Fatalf("status: code=%d stderr=%q", code, stderr)
	}
	var status struct {
		Repository struct {
			UpstreamOwner string `json:"upstream_owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	return status.Repository.UpstreamOwner
}

func assertL4Refusal(t *testing.T, call l4Call, args []string) {
	t.Helper()
	code, stdout, stderr := call(args...)
	if code != 1 {
		t.Fatalf("%v: code=%d stdout=%q stderr=%q, want 1", args, code, stdout, stderr)
	}
	var refusal struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stdout), &refusal); err != nil || refusal.Reason != "upstream_not_allowed" {
		t.Fatalf("%v: refusal=%+v stdout=%q err=%v", args, refusal, stdout, err)
	}
}
