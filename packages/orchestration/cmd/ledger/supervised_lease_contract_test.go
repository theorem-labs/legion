package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSupervisedLeaseCommandsCatchExpiryAndRenewalRegressions catches a zero TTL being rejected,
// serialising an expiry that does not exist, or allowing that lease to be renewed.
func TestSupervisedLeaseCommandsCatchExpiryAndRenewalRegressions(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "3")
	code, claimJSON, _ := call("--json", "claim", "r", "item", "supervisor:worker", "0")
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

	if code, out, _ := call("--json", "renew", "r", "item", "supervisor:worker", "1", "1m"); code != 1 || !strings.Contains(out, `"reason":"supervised_lease"`) {
		t.Fatalf("renew supervised lease exited %d with %s, want supervised_lease refusal", code, out)
	}
}

// TestExpireCommandPrintsAnEmptyListWhenNothingExpires catches a sweep that frees nothing printing null instead of a
// list. A supervised lease never expires, so a sweep beside one frees nothing, and its output is still a list.
func TestExpireCommandPrintsAnEmptyListWhenNothingExpires(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "3")
	if code, stdout, stderr := call("--json", "expire", "r"); code != 0 || stdout != "[]\n" {
		t.Fatalf("expire with no lease: code=%d stdout=%q stderr=%q, want []", code, stdout, stderr)
	}
	mustExit(t, call, 0, "claim", "r", "item", "supervisor:worker", "0")
	if code, stdout, stderr := call("--json", "expire", "r"); code != 0 || stdout != "[]\n" {
		t.Fatalf("expire beside a supervised lease: code=%d stdout=%q stderr=%q, want []", code, stdout, stderr)
	}
}

// TestClaimCommandTakesOnlyTheLiteralZeroAsSupervised catches a duration that is zero, such as 0s, granting a lease
// that never ends. Only the literal 0 asks for a supervised lease; a zero duration was refused before supervised
// leases existed, and it is still refused, so a script that computes a ttl of zero gets an error, not a lease that
// only a supervisor can free.
func TestClaimCommandTakesOnlyTheLiteralZeroAsSupervised(t *testing.T) {
	call := ledgerCommand(t)
	mustExit(t, call, 0, "repo", "ensure", "r", "3")
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

// TestLeaseCommandsTextNamesASupervisedLease catches the text output of a supervised claim printing a zero time as its
// end. Without --json, a supervised lease says that it is supervised and prints no end, and a fixed-end lease, claimed
// or renewed, prints its end.
func TestLeaseCommandsTextNamesASupervisedLease(t *testing.T) {
	call := ledgerCommand(t)
	invoke := func(args ...string) string {
		t.Helper()
		return mustRun(t, call, args...)
	}
	invoke("repo", "ensure", "r", "3")
	if got := invoke("claim", "r", "item", "supervisor:worker", "0"); got != "item holder=supervisor:worker generation=1 supervised\n" {
		t.Fatalf("supervised claim text = %q", got)
	}
	for _, args := range [][]string{{"claim", "r", "fixed", "agent", "1h"}, {"renew", "r", "fixed", "agent", "1", "1m"}} {
		if got := invoke(args...); !strings.HasPrefix(got, "fixed holder=agent generation=1 expires_at=2") || strings.Contains(got, "supervised") {
			t.Fatalf("%v text = %q, want its end", args, got)
		}
	}
}
