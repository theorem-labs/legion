package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

func TestHelpStartsWithOneLineUsage(t *testing.T) {
	var out, err bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, func(string) string { return "" }, &out, &err); code != 0 {
		t.Fatalf("help exit code %d: %s", code, err.String())
	}
	if first := strings.SplitN(out.String(), "\n", 2)[0]; !strings.HasPrefix(first, "usage: ledger ") {
		t.Fatalf("help first line %q", first)
	}
}

func TestEveryLedgerSubcommandRoundTrips(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, err bytes.Buffer
		if code := run(context.Background(), args, getenv, &out, &err); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, err.String())
		}
		return out.String()
	}

	invoke("repo", "ensure", "r", "3")
	invoke("claim", "r", "item", "alice", "1m")
	claimed := invoke("--json", "item", "r", "item")
	var held struct {
		State      string     `json:"state"`
		Holder     string     `json:"holder"`
		Generation int64      `json:"generation"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(claimed), &held); err != nil || held.ExpiresAt == nil {
		t.Fatalf("claimed item: %v %+v", err, held)
	}
	claimedExpiry := *held.ExpiresAt
	invoke("renew", "r", "item", "alice", "1", "1m")
	renewed := invoke("--json", "item", "r", "item")
	held = struct {
		State      string     `json:"state"`
		Holder     string     `json:"holder"`
		Generation int64      `json:"generation"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}{}
	if err := json.Unmarshal([]byte(renewed), &held); err != nil || held.State != "open" || held.Holder != "alice" ||
		held.Generation != 1 || held.ExpiresAt == nil {
		t.Fatalf("renewed item: %v %+v", err, held)
	}
	if want := claimedExpiry.Add(time.Minute); !held.ExpiresAt.Equal(want) {
		t.Fatalf("renewed expiry %s, want %s", held.ExpiresAt, want)
	}
	invoke("release", "r", "item", "alice", "1")
	released := invoke("--json", "item", "r", "item")
	held = struct {
		State      string     `json:"state"`
		Holder     string     `json:"holder"`
		Generation int64      `json:"generation"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}{}
	if err := json.Unmarshal([]byte(released), &held); err != nil || held.Holder != "" || held.ExpiresAt != nil {
		t.Fatalf("released item: %v %+v", err, held)
	}
	invoke("claim", "r", "done", "alice", "1m")
	invoke("complete", "r", "done", "alice", "1", "result")
	invoke("expire", "r")
	item := invoke("--json", "item", "r", "done")
	var completed struct {
		State  string `json:"state"`
		Holder string `json:"holder"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(item), &completed); err != nil {
		t.Fatalf("item JSON: %v: %q", err, item)
	}
	if completed.State != "done" || completed.Holder != "" || completed.Result != "result" {
		t.Fatalf("completed item: %+v", completed)
	}

	invoke("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
	invoke("extend", "r", "M-1", "1", "--write", "b.lean")
	check := invoke("--json", "check", "r", "M-1", "1", "--path", "a.lean", "--path", "b.lean")
	var checked struct {
		Pass bool `json:"pass"`
	}
	if err := json.Unmarshal([]byte(check), &checked); err != nil || !checked.Pass {
		t.Fatalf("clean check: %v %q", err, check)
	}
	invoke("takeover", "r", "M-1", "bob", "1")
	invoke("round", "r", "M-1", "2", "failed")
	roundStatus := invoke("--json", "status", "r")
	var openStatus struct {
		Migrations []struct {
			ID            string `json:"id"`
			Owner         string `json:"owner"`
			Generation    int64  `json:"generation"`
			LandingRounds int    `json:"landing_rounds"`
		} `json:"Migrations"`
	}
	if err := json.Unmarshal([]byte(roundStatus), &openStatus); err != nil || len(openStatus.Migrations) != 1 ||
		openStatus.Migrations[0].ID != "M-1" || openStatus.Migrations[0].Owner != "bob" ||
		openStatus.Migrations[0].Generation != 2 || openStatus.Migrations[0].LandingRounds != 1 {
		t.Fatalf("migration after round: %v %+v", err, openStatus)
	}
	invoke("abandon", "r", "M-1", "2", "stopped")

	invoke("contract", "register", "r", "C", "alice", "hash-1", "false")
	contract := invoke("--json", "contract", "show", "r", "C")
	var shown struct {
		Owner         string `json:"owner"`
		StatementHash string `json:"statement_hash"`
		Version       int64  `json:"version"`
	}
	if err := json.Unmarshal([]byte(contract), &shown); err != nil || shown.Owner != "alice" || shown.StatementHash != "hash-1" || shown.Version != 1 {
		t.Fatalf("registered contract: %v %+v", err, shown)
	}
	invoke("contract", "transfer", "r", "C", "alice", "bob")
	contract = invoke("--json", "contract", "show", "r", "C")
	if err := json.Unmarshal([]byte(contract), &shown); err != nil || shown.Owner != "bob" {
		t.Fatalf("transferred contract: %v %+v", err, shown)
	}
	invoke("agree", "r", "A-1", "--party", "bob", "--read", "C=1")
	invoke("open", "r", "M-2", "bob", "base", "branch", "--write", "c.lean", "--read", "C=hash-1")
	invoke("land", "--repo", "r", "--migration", "M-2", "--generation", "1", "--commit", "commit-1",
		"--path", "c.lean", "--current", "C=hash-1", "--changed-contract", "C=hash-2")
	stale := invoke("--json", "stale", "r")
	var staleAgreements []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stale), &staleAgreements); err != nil || len(staleAgreements) != 1 || staleAgreements[0].ID != "A-1" {
		t.Fatalf("stale agreements: %v %+v", err, staleAgreements)
	}
	status := invoke("--json", "status", "r")
	if err := json.Unmarshal([]byte(status), &openStatus); err != nil || len(openStatus.Migrations) != 0 {
		t.Fatalf("open migrations after abandon and land: %v %+v", err, openStatus)
	}
	events := invoke("--json", "events", "r", "0")
	var log []struct {
		Kind    string          `json:"kind"`
		Subject string          `json:"subject"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(events), &log); err != nil {
		t.Fatalf("event JSON: %v: %q", err, events)
	}
	advanced, staled := 0, 0
	for _, event := range log {
		var data struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch {
		case event.Kind == "contract.advanced" && event.Subject == "C":
			advanced++
			if data.Version != 2 {
				t.Fatalf("contract advance: %+v", event)
			}
		case event.Kind == "agreement.staled":
			if event.Subject != "A-1" {
				t.Fatalf("unexpected stale-agreement event: %+v", event)
			}
			staled++
			if data.Version != 2 {
				t.Fatalf("agreement stale: %+v", event)
			}
		}
	}
	if advanced != 1 || staled != 1 {
		t.Fatalf("landing event counts: contract.advanced=%d agreement.staled=%d log=%+v", advanced, staled, log)
	}
}

// enqueue, owners, a supervised claim and release-holder round-trip through the command line: the
// owner is the exact node id, the stored item and its item.enqueued event say so, and the exit codes
// are 0 for an enqueue or a repeat, 1 for an item another owner holds, and 2 for usage errors.
func TestOwnerCommandsRoundTrip(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
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
	invoke := func(args ...string) string {
		t.Helper()
		code, out, errOut := call(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, errOut)
		}
		return out
	}
	invoke("repo", "ensure", "r", "3")
	invoke("enqueue", "r", "item-1", "function:get_mountpoint")
	invoke("enqueue", "r", "item-1", "function:get_mountpoint")
	invoke("enqueue", "r", "item-2", "struct:mount")

	var item struct {
		Owner string `json:"owner"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &item); err != nil ||
		item.Owner != "function:get_mountpoint" || item.State != "open" {
		t.Fatalf("enqueued item: %v %+v", err, item)
	}
	var log []struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
		Data    struct {
			Owner string `json:"owner"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "events", "r", "0", "item-1")), &log); err != nil {
		t.Fatal(err)
	}
	enqueued := 0
	for _, e := range log {
		if e.Kind == "item.enqueued" {
			enqueued++
			if e.Data.Owner != "function:get_mountpoint" {
				t.Fatalf("enqueue event: %+v", e)
			}
		}
	}
	if enqueued != 1 {
		t.Fatalf("%d item.enqueued events for item-1, want 1: %+v", enqueued, log)
	}

	var owners []string
	if err := json.Unmarshal([]byte(invoke("--json", "owners", "r")), &owners); err != nil ||
		len(owners) != 2 || owners[0] != "function:get_mountpoint" || owners[1] != "struct:mount" {
		t.Fatalf("open owners: %v %v", err, owners)
	}

	if code, out, _ := call("--json", "enqueue", "r", "item-1", "struct:mount"); code != 1 || !strings.Contains(out, `"reason":"exists"`) {
		t.Fatalf("enqueue for another owner: code %d output %q", code, out)
	}
	for _, usage := range [][]string{{"enqueue", "r", "item-1"}, {"enqueue", "r", "", "struct:mount"}, {"owners"}, {"release-holder", "r", "x"}} {
		if code, _, _ := call(usage...); code != 2 {
			t.Fatalf("%v exited %d, want 2", usage, code)
		}
	}

	invoke("claim", "r", "item-1", "function:get_mountpoint", "0")
	var held struct {
		Holder    string     `json:"holder"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &held); err != nil ||
		held.Holder != "function:get_mountpoint" || held.ExpiresAt != nil {
		t.Fatalf("supervised claim: %v %+v", err, held)
	}
	var freed []struct {
		ID         string `json:"id"`
		Generation int64  `json:"generation"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "release-holder", "r", "function:get_mountpoint", "pane gone")), &freed); err != nil ||
		len(freed) != 1 || freed[0].ID != "item-1" || freed[0].Generation != 1 {
		t.Fatalf("released holder: %v %+v", err, freed)
	}
	if code, _, _ := call("complete", "r", "item-1", "function:get_mountpoint", "1", "late"); code != 1 {
		t.Fatalf("late result exited %d, want the refusal's 1", code)
	}
	var after struct {
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal([]byte(invoke("--json", "item", "r", "item-1")), &after); err != nil || after.Holder != "" {
		t.Fatalf("item after release-holder: %v %+v", err, after)
	}
	invoke("claim", "r", "item-2", "struct:mount", "0")
	invoke("complete", "r", "item-2", "struct:mount", "1", "done")
	if err := json.Unmarshal([]byte(invoke("--json", "owners", "r")), &owners); err != nil ||
		len(owners) != 1 || owners[0] != "function:get_mountpoint" {
		t.Fatalf("open owners after a completion: %v %v", err, owners)
	}
	if code, _, _ := call("--json", "enqueue", "r", "item-2", "struct:mount"); code != 1 {
		t.Fatalf("enqueue of a done item exited %d, want 1", code)
	}
}

func TestCommandExitCodes(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		return run(context.Background(), args, getenv, &out, &err), out.String()
	}
	if code, _ := call("claim"); code != 2 {
		t.Fatalf("usage exit code %d, want 2", code)
	}
	if code, _ := call("item", "r", "missing"); code != 3 {
		t.Fatalf("not found exit code %d, want 3", code)
	}
	if code, _ := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure exit code %d", code)
	}
	if code, _ := call("contract", "register", "r", "C", "alice", "hash-1", "false"); code != 0 {
		t.Fatalf("register exit code %d", code)
	}
	code, out := call("--json", "contract", "register", "r", "C", "alice", "hash-1", "false")
	if code != 1 || !strings.Contains(out, `"reason":"exists"`) {
		t.Fatalf("refusal code %d output %q", code, out)
	}
	file := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(file, []byte("postgres://invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--dsn-file", file, "repo", "ensure", "r", "0"}, getenv, &stdout, &stderr); code != 4 {
		t.Fatalf("database exit code %d, want 4", code)
	}
}

func TestCheckRefusalExitsOneWithJSONResult(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
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
	if code, _, stderr := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure exit=%d stderr=%s", code, stderr)
	}
	if code, _, stderr := call("open", "r", "M-1", "alice", "base", "branch", "--write", "owned.lean"); code != 0 {
		t.Fatalf("open exit=%d stderr=%s", code, stderr)
	}
	code, stdout, stderr := call("--json", "check", "r", "M-1", "1", "--path", "outside.lean")
	if code != 1 {
		t.Fatalf("refused check exit=%d stdout=%s stderr=%s; want 1", code, stdout, stderr)
	}
	var result struct {
		Pass       bool `json:"pass"`
		Violations []struct {
			Kind string `json:"kind"`
		} `json:"violations"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("refused check JSON: %v: %q", err, stdout)
	}
	if result.Pass || len(result.Violations) != 1 || result.Violations[0].Kind != "undeclared_write" {
		t.Fatalf("refused check result: %+v", result)
	}
}

func TestLandReadsJSONRequestFile(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	invoke := func(args ...string) {
		t.Helper()
		var out, err bytes.Buffer
		if code := run(context.Background(), args, getenv, &out, &err); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, err.String())
		}
	}
	invoke("repo", "ensure", "r", "0")
	invoke("contract", "register", "r", "C", "alice", "hash-1", "false")
	invoke("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean", "--read", "C=hash-1")
	request := `{"repo":"r","migration":"M-1","generation":1,"commit":"commit-1","changed_paths":["a.lean"],"current":{"C":"hash-1"},"changed_contracts":{"C":"hash-2"}}`
	file := filepath.Join(t.TempDir(), "land.json")
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke("land", "--request-file", file)
}

func TestLandJSONRequestRequiresChangedPaths(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		return run(context.Background(), args, getenv, &out, &err), err.String()
	}
	if code, stderr := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure: code=%d stderr=%s", code, stderr)
	}
	if code, stderr := call("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean"); code != 0 {
		t.Fatalf("open: code=%d stderr=%s", code, stderr)
	}
	file := filepath.Join(t.TempDir(), "land.json")
	request := `{"repo":"r","migration":"M-1","generation":1,"commit":"commit-1","current":{},"changed_contracts":{}}`
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stderr := call("land", "--request-file", file); code != 2 || !strings.Contains(stderr, "changed paths") {
		t.Fatalf("missing changed_paths: code=%d stderr=%q", code, stderr)
	}
}

func TestLandFlagsRequireChangedPathsOrNoPaths(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		return run(context.Background(), args, getenv, &out, &err), err.String()
	}
	if code, stderr := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure: code=%d stderr=%s", code, stderr)
	}
	if code, stderr := call("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean"); code != 0 {
		t.Fatalf("open: code=%d stderr=%s", code, stderr)
	}
	land := []string{"land", "--repo", "r", "--migration", "M-1", "--generation", "1", "--commit", "commit-1"}
	if code, stderr := call(land...); code != 2 || !strings.Contains(stderr, "--path") {
		t.Fatalf("land without --path or --no-paths: code=%d stderr=%q", code, stderr)
	}
	if code, stderr := call(append(land, "--no-paths", "--path", "a.lean")...); code != 2 || !strings.Contains(stderr, "--no-paths") {
		t.Fatalf("land with both --no-paths and --path: code=%d stderr=%q", code, stderr)
	}
	if code, stderr := call(append(land, "--no-paths")...); code != 0 {
		t.Fatalf("land with --no-paths: code=%d stderr=%q", code, stderr)
	}
}

func TestRepeatableListFlagsPreserveCommaPaths(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, err bytes.Buffer
		if code := run(context.Background(), args, getenv, &out, &err); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, err.String())
		}
		return out.String()
	}
	invoke("repo", "ensure", "r", "0")
	invoke("open", "r", "M-1", "alice", "base", "branch",
		"--write", "a,b.lean", "--read", "X=hash")
	result := invoke("--json", "check", "r", "M-1", "1",
		"--path", "a,b.lean", "--current", "X=hash")
	var check struct {
		Pass bool `json:"pass"`
	}
	if err := json.Unmarshal([]byte(result), &check); err != nil || !check.Pass {
		t.Fatalf("comma path check: %v %q", err, result)
	}
}

func TestJSONRequestFilesPreserveEqualsNamesAndHashes(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, err bytes.Buffer
		if code := run(context.Background(), args, getenv, &out, &err); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, err.String())
		}
		return out.String()
	}
	writeRequest := func(name, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	invoke("repo", "ensure", "r", "0")
	invoke("contract", "register", "r", "A=B", "owner", "h=1", "false")
	openFile := writeRequest("open.json", `{"repo":"r","id":"M-1","owner":"owner","base_commit":"base","branch":"branch","writes":["a=b.lean"],"reads":{"A=B":"h=1"}}`)
	invoke("open", "--request-file", openFile)
	extendFile := writeRequest("extend.json", `{"repo":"r","id":"M-1","generation":1,"writes":["b=c.lean"],"reads":{}}`)
	invoke("extend", "--request-file", extendFile)
	checkFile := writeRequest("check.json", `{"repo":"r","id":"M-1","generation":1,"changed":["a=b.lean","b=c.lean"],"current":{"A=B":"h=1"}}`)
	check := invoke("--json", "check", "--request-file", checkFile)
	var checked struct {
		Pass bool `json:"pass"`
	}
	if err := json.Unmarshal([]byte(check), &checked); err != nil || !checked.Pass {
		t.Fatalf("equals check: %v %q", err, check)
	}
	agreeFile := writeRequest("agree.json", `{"repo":"r","id":"agreement","parties":["owner"],"reads":{"A=B":1}}`)
	invoke("agree", "--request-file", agreeFile)
	landFile := writeRequest("land.json", `{"repo":"r","migration":"M-1","generation":1,"commit":"commit-1","changed_paths":["a=b.lean","b=c.lean"],"current":{"A=B":"h=1"},"changed_contracts":{"A=B":"h=2"}}`)
	invoke("land", "--request-file", landFile)
	contract := invoke("--json", "contract", "show", "r", "A=B")
	var shown struct {
		StatementHash string `json:"statement_hash"`
		Version       int64  `json:"version"`
	}
	if err := json.Unmarshal([]byte(contract), &shown); err != nil || shown.StatementHash != "h=2" || shown.Version != 2 {
		t.Fatalf("equals contract: %v %+v", err, shown)
	}
}

func TestFlagMapArgumentsRejectAmbiguousEquals(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
	getenv := func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}
	call := func(args ...string) (int, string) {
		t.Helper()
		var out, err bytes.Buffer
		return run(context.Background(), args, getenv, &out, &err), err.String()
	}
	mustCode := func(want int, args ...string) {
		t.Helper()
		code, stderr := call(args...)
		if code != want || !strings.Contains(stderr, "use --request-file") {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
	}
	if code, stderr := call("repo", "ensure", "r", "0"); code != 0 {
		t.Fatalf("ensure: code=%d stderr=%q", code, stderr)
	}
	mustCode(2, "open", "r", "M-1", "owner", "base", "branch", "--write", "a.lean", "--read", "A=B=h=1")
	if code, stderr := call("open", "r", "M-1", "owner", "base", "branch", "--write", "a.lean"); code != 0 {
		t.Fatalf("open: code=%d stderr=%q", code, stderr)
	}
	mustCode(2, "extend", "r", "M-1", "1", "--read", "A=B=h=1")
	mustCode(2, "check", "r", "M-1", "1", "--current", "A=B=h=1")
	mustCode(2, "agree", "r", "agreement", "--party", "owner", "--read", "A=B=1")
	mustCode(2, "land", "--repo", "r", "--migration", "M-1", "--generation", "1", "--commit", "commit-1",
		"--current", "A=B=h=1")
}

func TestRepoEnsureRejectsRoundLimitsOutsideInt32(t *testing.T) {
	dsn := testpg.Database(t, "ledger_command")
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
	if code, _, stderr := call("repo", "ensure", "max", "2147483647"); code != 0 {
		t.Fatalf("max limit: code=%d stderr=%q", code, stderr)
	}
	statusCode, status, stderr := call("--json", "status", "max")
	if statusCode != 0 || !strings.Contains(status, `"max_landing_rounds":2147483647`) {
		t.Fatalf("max status: code=%d output=%q stderr=%q", statusCode, status, stderr)
	}
	for _, limit := range []string{"2147483648", "4294967296", "-1"} {
		name := "bad-" + limit
		if code, _, stderr := call("repo", "ensure", name, limit); code != 2 {
			t.Errorf("limit %s: code=%d stderr=%q; want 2", limit, code, stderr)
		}
		if code, _, _ := call("status", name); code != 3 {
			t.Errorf("limit %s persisted repository: status code=%d; want 3", limit, code)
		}
	}
}
