package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func escalationOwners(t *testing.T, call l4Call) []string {
	t.Helper()
	code, stdout, stderr := call("--json", "status", "r")
	if code != 0 {
		t.Fatalf("status: code=%d stderr=%q", code, stderr)
	}
	var status struct {
		Repository struct {
			UpstreamOwner    string   `json:"upstream_owner"`
			EscalationOwners []string `json:"escalation_owners"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	if status.Repository.EscalationOwners == nil {
		t.Fatalf("status prints no escalation_owners array: %s", stdout)
	}
	return status.Repository.EscalationOwners
}

// repo ensure takes --escalation-owner, repeatable, beside --upstream-owner in any order. The owners given replace the
// stored set, no flag keeps it, and a single empty owner clears it.
func TestRepoEnsureConfiguresEscalationOwners(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "1")
	if got := escalationOwners(t, call); len(got) != 0 {
		t.Fatalf("escalation owners of a new repository = %q", got)
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "1", "--escalation-owner", "steward:b", "--escalation-owner", "steward:a")
	if got, want := escalationOwners(t, call), []string{"steward:a", "steward:b"}; !slices.Equal(got, want) {
		t.Fatalf("escalation owners = %q, want %q", got, want)
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "2")
	if got, want := escalationOwners(t, call), []string{"steward:a", "steward:b"}; !slices.Equal(got, want) {
		t.Fatalf("escalation owners after an ensure without the flag = %q, want %q", got, want)
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "2", "--escalation-owner", "steward:c", "--upstream-owner", "upstream")
	if got := escalationOwners(t, call); !slices.Equal(got, []string{"steward:c"}) || l4UpstreamOwner(t, call) != "upstream" {
		t.Fatalf("escalation owners %q, upstream owner %q", got, l4UpstreamOwner(t, call))
	}
	mustL4Code(t, call, 0, "repo", "ensure", "r", "2", "--escalation-owner", "")
	if got := escalationOwners(t, call); len(got) != 0 {
		t.Fatalf("escalation owners after clearing = %q", got)
	}
}

// A malformed repo ensure exits 2 and changes nothing.
func TestRepoEnsureRefusesMalformedOwnerFlags(t *testing.T) {
	call := l4Command(t)
	mustL4Code(t, call, 0, "repo", "ensure", "r", "1", "--escalation-owner", "steward:a", "--upstream-owner", "upstream")
	for _, test := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"empty owner among others", []string{"--escalation-owner", "", "--escalation-owner", "steward:b"}, "escalation owner"},
		{"blank owner", []string{"--escalation-owner", " "}, "escalation owner"},
		{"missing value", []string{"--escalation-owner"}, "repo ensure <repo>"},
		{"repeated upstream owner", []string{"--upstream-owner", "a", "--upstream-owner", "b"}, "--upstream-owner"},
		{"unknown flag", []string{"--owner", "a"}, "repo ensure <repo>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"repo", "ensure", "r", "3"}, test.args...)
			code, stdout, stderr := call(args...)
			if code != 2 || !strings.Contains(stderr, test.stderr) {
				t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", args, code, stdout, stderr, test.stderr)
			}
		})
	}
	if got := escalationOwners(t, call); !slices.Equal(got, []string{"steward:a"}) || l4UpstreamOwner(t, call) != "upstream" {
		t.Fatalf("refused ensures changed the repository: escalation owners %q, upstream owner %q", got, l4UpstreamOwner(t, call))
	}
}
