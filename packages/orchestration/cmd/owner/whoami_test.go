package main

import (
	"encoding/json"
	"reflect"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestWhoamiAndTopicsDescribeEveryOwnerKind(t *testing.T) {
	f := newCommandFixture(t)
	var who map[string]any
	if err := json.Unmarshal([]byte(f.as(second).ok("--json", "whoami")), &who); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"owner": second, "role": "function", "key": ownerpkg.Key(second), "kind": "function", "lean_dir": "Linux/kernel/second/Second",
		"c_file": "kernel/second.c", "active_set": "skeleton/active/fixture.yaml", "active": true, "epoch": float64(2)}
	if !reflect.DeepEqual(who, want) {
		t.Fatalf("whoami = %v, want %v", who, want)
	}
	if err := json.Unmarshal([]byte(f.as(callee).ok("--json", "whoami")), &who); err != nil || who["active"] != false || who["epoch"] != float64(1) {
		t.Fatalf("whoami callee = %v, %v", who, err)
	}

	for owner, wantTopic := range map[string]string{
		caller:      "notifications.lxspec.fact.stack-alignment.users",
		fact:        "notifications.lxspec.fact.stack-alignment.needs",
		"librarian": "notifications.lxspec.concepts",
	} {
		var topics struct {
			Owner  string   `json:"owner"`
			Topics []string `json:"topics"`
		}
		if err := json.Unmarshal([]byte(f.as(owner).ok("--json", "topics")), &topics); err != nil {
			t.Fatal(err)
		}
		if topics.Owner != owner || !containsString(topics.Topics, wantTopic) {
			t.Errorf("topics for %s = %+v, want %s", owner, topics, wantTopic)
		}
	}
	if code, _ := f.as("publisher").fails("topics"); code != 1 {
		t.Fatalf("topics for a role with no subscriptions exited %d, want 1", code)
	}
}
