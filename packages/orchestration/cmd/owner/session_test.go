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

// The owner's worktree path is the owner's own, so whatever is there must be a checkout of the owner's branch of
// the deployment's bare repository.
func TestWorktreeRefusesACheckoutOfAnotherBranch(t *testing.T) {
	f := newCommandFixture(t)
	wt := f.wt(state)
	f.git(f.root, "--git-dir="+f.bare, "worktree", "add", "-q", "-b", "other", wt, "main")
	if code, stderr := f.as(state).fails("worktree"); code != 1 || !strings.Contains(stderr, "other") {
		t.Fatalf("worktree on a checkout of another branch exited %d: %s", code, stderr)
	}
}

// A region architect runs the command from its own jj workspace, which is no owner's: the owner's worktree is
// <state_dir>/owners/<Key(owner)>/wt of the deployment, whichever directory the command runs in, and every command
// that writes records commits there.
func TestWorktreeIsUnderTheDeploymentsStateDirWhereverTheCommandRuns(t *testing.T) {
	f := newCommandFixture(t)
	want := filepath.Join(f.stateDir(), "owners", ownerpkg.Key(caller), "wt")
	for _, dir := range []string{"workspace-a", "workspace-b"} {
		dir = filepath.Join(f.root, dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		var tree struct{ Path, Branch string }
		if err := json.Unmarshal([]byte(f.as(caller).ok("--json", "worktree")), &tree); err != nil {
			t.Fatal(err)
		}
		if tree.Path != want || tree.Branch != ownerpkg.Branch(caller) {
			t.Fatalf("worktree run in %s = %+v, want %s on %s", dir, tree, want, ownerpkg.Branch(caller))
		}
		f.as(caller).ok("need", state, f.write("need.md", needDraft))
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("the command wrote into its working directory %s: %v, %v", dir, entries, err)
		}
	}
	if got := f.git(want, "log", "-1", "--format=%H", "--", ownerpkg.NeedRecord("skeleton", caller, state)); got == "" || got != f.lastCommit(caller, ownerpkg.NeedRecord("skeleton", caller, state)) {
		t.Fatalf("the need's commit in %s is %q, not the owner branch's", want, got)
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
		code := run(f.ctx, append([]string{"--deploy", f.deploy}, command...), f.getenv, &stdout, &stderr)
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
	if code := run(f.ctx, []string{"--deploy", f.deploy, "returned", "returned:" + migration + "@" + strconv.FormatInt(event, 10)}, f.getenv, &stdout, &stderr); code != 0 {
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
