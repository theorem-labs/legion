package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// TestSupervisedLeaseCommandsCatchExpiryAndRenewalRegressions catches a zero TTL being rejected,
// serialising an expiry that does not exist, or allowing that lease to be renewed.
func TestSupervisedLeaseCommandsCatchExpiryAndRenewalRegressions(t *testing.T) {
	dsn := testpg.Database(t, "ledger_supervised_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		code := run(context.Background(), args, getenv, &out, &err)
		return code, out.String()
	}
	if code, out := call("repo", "ensure", "r", "3"); code != 0 {
		t.Fatalf("ensure exited %d: %s", code, out)
	}
	code, claimJSON := call("--json", "claim", "r", "item", "supervisor:worker", "0")
	if code != 0 {
		t.Fatalf("claim supervised lease exited %d: %s", code, claimJSON)
	}
	var claim map[string]json.RawMessage
	if err := json.Unmarshal([]byte(claimJSON), &claim); err != nil {
		t.Fatalf("decode supervised claim: %v: %q", err, claimJSON)
	}
	if _, found := claim["expires_at"]; found {
		t.Fatalf("supervised claim JSON has expires_at: %s", claimJSON)
	}

	if code, out := call("--json", "renew", "r", "item", "supervisor:worker", "1", "1m"); code != 1 || !strings.Contains(out, `"reason":"supervised_lease"`) {
		t.Fatalf("renew supervised lease exited %d with %s, want supervised_lease refusal", code, out)
	}
}

// TestExpireCommandPrintsAnEmptyListWhenNothingExpires catches a sweep that frees nothing printing null instead of a
// list. A supervised lease never expires, so a sweep beside one frees nothing, and its output is still a list.
func TestExpireCommandPrintsAnEmptyListWhenNothingExpires(t *testing.T) {
	dsn := testpg.Database(t, "ledger_expire_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string, string) {
		t.Helper()
		var out, err bytes.Buffer
		code := run(context.Background(), args, getenv, &out, &err)
		return code, out.String(), err.String()
	}
	if code, _, stderr := call("repo", "ensure", "r", "3"); code != 0 {
		t.Fatalf("ensure exited %d: %s", code, stderr)
	}
	if code, stdout, stderr := call("--json", "expire", "r"); code != 0 || stdout != "[]\n" {
		t.Fatalf("expire with no lease: code=%d stdout=%q stderr=%q, want []", code, stdout, stderr)
	}
	if code, _, stderr := call("claim", "r", "item", "supervisor:worker", "0"); code != 0 {
		t.Fatalf("supervised claim exited %d: %s", code, stderr)
	}
	if code, stdout, stderr := call("--json", "expire", "r"); code != 0 || stdout != "[]\n" {
		t.Fatalf("expire beside a supervised lease: code=%d stdout=%q stderr=%q, want []", code, stdout, stderr)
	}
}

// TestClaimCommandTakesOnlyTheLiteralZeroAsSupervised catches a duration that is zero, such as 0s, granting a lease
// that never ends. Only the literal 0 asks for a supervised lease; a zero duration was refused before supervised
// leases existed, and it is still refused, so a script that computes a ttl of zero gets an error, not a lease that
// only a supervisor can free.
func TestClaimCommandTakesOnlyTheLiteralZeroAsSupervised(t *testing.T) {
	dsn := testpg.Database(t, "ledger_zero_ttl_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string, string) {
		t.Helper()
		var out, err bytes.Buffer
		code := run(context.Background(), args, getenv, &out, &err)
		return code, out.String(), err.String()
	}
	if code, _, stderr := call("repo", "ensure", "r", "3"); code != 0 {
		t.Fatalf("ensure exited %d: %s", code, stderr)
	}
	for _, ttl := range []string{"0s", "0m", "-0s", "0.0s", "0h0m0s"} {
		code, stdout, stderr := call("--json", "claim", "r", "item-"+ttl, "holder", ttl)
		if code != 2 || !strings.Contains(stderr, "0 for a supervised lease") {
			t.Fatalf("claim with ttl %q: code=%d stdout=%q stderr=%q, want exit 2 naming the supervised spelling", ttl, code, stdout, stderr)
		}
		if code, stdout, _ := call("--json", "item", "r", "item-"+ttl); code != 3 {
			t.Fatalf("claim with ttl %q created item: code=%d stdout=%q", ttl, code, stdout)
		}
	}
	if code, stdout, stderr := call("--json", "claim", "r", "item-0", "holder", "0"); code != 0 || strings.Contains(stdout, "expires_at") {
		t.Fatalf("claim with ttl 0: code=%d stdout=%q stderr=%q, want a supervised lease", code, stdout, stderr)
	}
}
