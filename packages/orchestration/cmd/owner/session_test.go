package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// An owner runs as an ordinary architect session, whose environment names no owner: the command acts for the owner
// that --owner names, and a launch variable such as LEGION_OWNER names nobody. A command that needs an owner and has
// no --owner is a usage error that names the flag, before it reads anything.
func TestCommandsActForTheOwnerFlagNotALaunchVariable(t *testing.T) {
	f := newCommandFixture(t)
	f.env["LEGION_OWNER"] = caller
	for _, command := range [][]string{{"whoami"}, {"inbox"}, {"worktree"}, {"need", state, f.write("need.md", needDraft)}} {
		var stdout, stderr strings.Builder
		code := run(f.ctx, append([]string{"--deploy", f.deploy}, command...), f.getenv, f.root, &stdout, &stderr)
		if code != exitUsage || !strings.Contains(stderr.String(), "--owner") {
			t.Errorf("%v with LEGION_OWNER and no --owner exited %d: %s", command, code, stderr.String())
		}
	}
	if f.needWritten(state) {
		t.Fatal("a need with no --owner was committed")
	}
	// returned reads a migration's return event, which acts for no owner: it needs no --owner.
	migration := "m:" + ownerpkg.Key(caller) + ":1"
	event := f.insertEvent(migration, `{"generation":1,"landing_rounds":1,"reason":"head_unavailable","batch":"","detail":"","log":"","key":"b-7/m-2/1","ready_cleared":true}`)
	var stdout, stderr strings.Builder
	if code := run(f.ctx, []string{"--deploy", f.deploy, "returned", "returned:" + migration + "@" + strconv.FormatInt(event, 10)}, f.getenv, f.root, &stdout, &stderr); code != 0 {
		t.Fatalf("returned with no --owner exited %d: %s", code, stderr.String())
	}
}

// The ledger repository is the copy's skeleton/owner.yaml repo and the ledger is the deployment's ledger_dsn_file: a
// launch variable that names another repository or DSN file changes nothing.
func TestCommandsReadTheLedgerFromTheCopyAndTheDeployment(t *testing.T) {
	f := newCommandFixture(t)
	f.env["LEGION_LEDGER_REPOSITORY"] = "another-repository"
	f.env["LEGION_LEDGER_DSN_FILE"] = filepath.Join(f.root, "missing-dsn")
	need := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	f.as(caller).ok("need", state, f.write("need.md", needDraft))
	if item := f.item(need); item.State != "open" || item.Owner != state {
		t.Fatalf("need item in repository r = %+v", item)
	}
	var inbox []ownerpkg.InboxItem
	if err := json.Unmarshal([]byte(f.as(state).ok("--json", "inbox")), &inbox); err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != need {
		t.Fatalf("the target's inbox = %+v, want %s", inbox, need)
	}
}

// --ledger-dsn-file names the ledger instead of the deployment's ledger_dsn_file; without it, a ledger_dsn_file the
// command cannot read stops the command, naming that file.
func TestLedgerDSNFileFlagOverridesTheDeployment(t *testing.T) {
	f := newCommandFixture(t)
	missing := filepath.Join(f.root, "missing-dsn")
	deploy := filepath.Join(f.root, "deploy-missing-dsn.yaml")
	f.writeFile(deploy, strings.Replace(string(f.read(f.deploy)), filepath.Join(f.root, "ledger-dsn"), missing, 1))
	need := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"

	code, stderr := f.as(caller).fails("--deploy", deploy, "need", state, f.write("need.md", needDraft))
	if code == 0 || !strings.Contains(stderr, missing) || !strings.Contains(stderr, "ledger_dsn_file") {
		t.Fatalf("need with an unreadable ledger_dsn_file exited %d: %s", code, stderr)
	}
	if f.needWritten(state) {
		t.Fatal("the need was committed with no ledger")
	}
	f.as(caller).ok("--deploy", deploy, "--ledger-dsn-file", filepath.Join(f.root, "ledger-dsn"), "need", state, f.write("need.md", needDraft))
	if item := f.item(need); item.State != "open" || item.Owner != state {
		t.Fatalf("need item through --ledger-dsn-file = %+v", item)
	}
}
