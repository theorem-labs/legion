package owner

import (
	"reflect"
	"slices"
	"testing"
)

func TestSubscriptionsAddFactUsersNeedsConceptsAndAll(t *testing.T) {
	fixture := loadRoutingFixture(t)

	caller := fixture.subscriptions(t, "function:caller")
	for _, want := range append(append([]string(nil), fixture.topics.Owners["function:caller"]...), "notifications.lxspec.fact.stack-alignment.users") {
		if !contains(caller, want) {
			t.Errorf("caller subscriptions = %v, missing %q", caller, want)
		}
	}
	if contains(fixture.subscriptions(t, "function:second@kernel/second.c"), "notifications.lxspec.fact.stack-alignment.users") {
		t.Error("a function that states no fact subscribed to the fact's users topic")
	}

	// Role owners are in no topics.json, so the command adds all for them: protocol adopt and revert, landings and
	// the score reach every owner on it.
	if got := fixture.subscriptions(t, "fact-stack-alignment"); !reflect.DeepEqual(got, []string{"notifications.lxspec.fact.stack-alignment.needs", "notifications.lxspec.all"}) {
		t.Fatalf("fact owner subscriptions = %v", got)
	}
	if got := fixture.subscriptions(t, "librarian"); !reflect.DeepEqual(got, []string{"notifications.lxspec.concepts", "notifications.lxspec.all"}) {
		t.Fatalf("librarian subscriptions = %v", got)
	}
	if got := fixture.subscriptions(t, "function:caller"); len(got) != len(slices.Compact(slices.Sorted(slices.Values(got)))) {
		t.Fatalf("caller subscriptions repeat a topic: %v", got)
	}
}

func TestSubscriptionsRefuseTopicsOutsideTheNamespace(t *testing.T) {
	fixture := loadRoutingFixture(t)
	topics := fixture.topics
	topics.Prefix = ""
	if got, err := Subscriptions("librarian", "librarian", topics, fixture.facts, fixture.factOwners); err == nil {
		t.Fatalf("Subscriptions with no prefix = %v, want an error", got)
	}
}

func TestParseFactOwnersRefusesDuplicateOwners(t *testing.T) {
	_, err := ParseFactOwners([]byte("facts:\n  - {name: one, owner: fact-shared, concept: null}\n  - {name: two, owner: fact-shared, concept: null}\n"))
	if err == nil {
		t.Fatal("ParseFactOwners accepted one owner for two facts")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
