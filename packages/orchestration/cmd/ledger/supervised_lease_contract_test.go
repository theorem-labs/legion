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
