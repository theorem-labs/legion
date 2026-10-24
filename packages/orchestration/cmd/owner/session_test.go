package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func TestWorktreeRefusesAnotherOwnersCheckout(t *testing.T) {
	f := newCommandFixture(t)
	dir := filepath.Join(f.root, "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := f.as(caller).runIn(dir, "worktree"); code != 0 {
		t.Fatalf("first worktree exited %d: %s", code, stderr)
	}
	if _, stderr, code := f.as(state).runIn(dir, "worktree"); code != 1 || !strings.Contains(stderr, "owner/"+ownerpkg.Key(caller)) {
		t.Fatalf("worktree on another owner's checkout exited %d: %s", code, stderr)
	}
}

// main runs every command in ".", so the worktree must be the owner's ./wt however git resolves paths.
func TestWorktreeIsTheWorkingDirectorysWtWhenRunInDot(t *testing.T) {
	f := newCommandFixture(t)
	dir := filepath.Join(f.root, "owners", "relative")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	out, stderr, code := f.as(caller).runIn(".", "--json", "worktree")
	if code != 0 {
		t.Fatalf("worktree exited %d: %s", code, stderr)
	}
	var tree struct{ Path string }
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "wt"); tree.Path != want {
		t.Fatalf("worktree path = %q, want %q", tree.Path, want)
	}
	f.as(caller).runIn(".", "need", state, f.write("need.md", needDraft))
	if _, stderr, code := f.as(caller).runIn(".", "need", state, f.write("need.md", needDraft)); code != 0 {
		t.Fatalf("need in the same directory exited %d: %s", code, stderr)
	}
	if got := f.lastCommit(caller, ownerpkg.NeedRecord("skeleton", caller, state)); got == "" {
		t.Fatal("need did not commit on the owner's branch")
	}
}

// The supervisor names the owner in LEGION_OWNER; LEGION_OWNER_KEY has a credential's shape, which the daemon
// strips from every pane, so the command never reads it.
func TestCommandsActForLegionOwnerNotLegionOwnerKey(t *testing.T) {
	f := newCommandFixture(t)
	env := func(key string) string {
		if key == "LEGION_OWNER_KEY" {
			return caller
		}
		return f.env[key]
	}
	var stdout, stderr strings.Builder
	code := run(f.ctx, []string{"--deploy", f.deploy, "whoami"}, env, f.as(caller).dir(), &stdout, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "LEGION_OWNER is unset") {
		t.Fatalf("whoami with only LEGION_OWNER_KEY exited %d: %s", code, stderr.String())
	}
}
