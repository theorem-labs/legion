package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

type readyListRecord struct {
	ID                 string            `json:"id"`
	Owner              string            `json:"owner"`
	Generation         int64             `json:"generation"`
	Head               string            `json:"head"`
	ReadyEvent         int64             `json:"ready_event"`
	ReadyAt            time.Time         `json:"ready_at"`
	ChangedContracts   map[string]string `json:"changed_contracts"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []struct {
		Contract string `json:"contract"`
		Reviewer string `json:"reviewer"`
		Head     string `json:"head"`
	} `json:"reviews"`
	ApprovedPinned []string `json:"approved_pinned"`
	Upstream       string   `json:"upstream"`
}

func TestReadyCommandRoundTripsFlagAndRequestFileForms(t *testing.T) {
	dsn := testpg.Database(t, "ledger_ready_command")
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
		code, out, stderr := call(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr)
		}
		return out
	}

	invoke("repo", "ensure", "r", "0")
	invoke("open", "r", "M-flag", "alice", "base", "branch", "--write", "a.lean")
	invoke("open", "r", "M-file", "bob", "base", "branch", "--write", "b.lean")
	invoke("ready", "r", "M-flag", "1", "--head", "head-flag", "--changed-contract", "C.flag=hash-flag",
		"--compatibility-proof", "C.compat", "--reviewed-head", "head-flag", "--review", "C.flag=alice@head-flag",
		"--approved-pinned", "C.flag", "--upstream", "upstream-flag")

	request := `{"repo":"r","id":"M-file","generation":1,"head":"head@file","changed_contracts":{"C@file":"hash=from-file"},"compatibility_proof":"C@compat","reviewed_head":"head@file","reviews":[{"contract":"C@file","reviewer":"alice=reviewer","head":"head@file"}],"approved_pinned":["C@file"],"upstream":"upstream@file"}`
	file := filepath.Join(t.TempDir(), "ready.json")
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke("ready", "--request-file", file)

	var records []readyListRecord
	if err := json.Unmarshal([]byte(invoke("--json", "ready-list", "r")), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != "M-flag" || records[1].ID != "M-file" || records[0].ReadyEvent >= records[1].ReadyEvent {
		t.Fatalf("ready-list ordering: %+v", records)
	}
	flag := records[0]
	if flag.Owner != "alice" || flag.Generation != 1 || flag.Head != "head-flag" || flag.ReadyEvent == 0 || flag.ReadyAt.IsZero() ||
		!reflect.DeepEqual(flag.ChangedContracts, map[string]string{"C.flag": "hash-flag"}) || flag.CompatibilityProof != "C.compat" ||
		flag.ReviewedHead != "head-flag" || len(flag.Reviews) != 1 || flag.Reviews[0].Contract != "C.flag" ||
		flag.Reviews[0].Reviewer != "alice" || flag.Reviews[0].Head != "head-flag" || !reflect.DeepEqual(flag.ApprovedPinned, []string{"C.flag"}) ||
		flag.Upstream != "upstream-flag" {
		t.Fatalf("flag request did not round-trip: %+v", flag)
	}
	fromFile := records[1]
	if fromFile.Owner != "bob" || fromFile.Generation != 1 || fromFile.Head != "head@file" || fromFile.ReadyEvent == 0 || fromFile.ReadyAt.IsZero() ||
		!reflect.DeepEqual(fromFile.ChangedContracts, map[string]string{"C@file": "hash=from-file"}) || fromFile.CompatibilityProof != "C@compat" ||
		fromFile.ReviewedHead != "head@file" || len(fromFile.Reviews) != 1 || fromFile.Reviews[0].Contract != "C@file" ||
		fromFile.Reviews[0].Reviewer != "alice=reviewer" || fromFile.Reviews[0].Head != "head@file" || !reflect.DeepEqual(fromFile.ApprovedPinned, []string{"C@file"}) ||
		fromFile.Upstream != "upstream@file" {
		t.Fatalf("request-file request did not round-trip: %+v", fromFile)
	}

	text := invoke("ready-list", "r")
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' })
	if len(lines) != 2 || !strings.Contains(lines[0], "M-flag") || !strings.Contains(lines[0], "ready_event=") ||
		!strings.Contains(lines[1], "M-file") || !strings.Contains(lines[1], "ready_event=") {
		t.Fatalf("ready-list text output is not one readable line per record: %q", text)
	}
}

func TestReadyFlagsRejectNamesContainingDelimiters(t *testing.T) {
	dsn := testpg.Database(t, "ledger_ready_delimiters")
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
		t.Fatalf("ensure: code=%d stderr=%q", code, stderr)
	}
	if code, stderr := call("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean"); code != 0 {
		t.Fatalf("open: code=%d stderr=%q", code, stderr)
	}
	for _, args := range [][]string{
		{"ready", "r", "M-1", "1", "--head", "head", "--changed-contract", "C@name=hash"},
		{"ready", "r", "M-1", "1", "--head", "head", "--changed-contract", "C=name=hash"},
		{"ready", "r", "M-1", "1", "--head", "head", "--review", "C@name=alice@head"},
		{"ready", "r", "M-1", "1", "--head", "head", "--approved-pinned", "C@name"},
	} {
		if code, stderr := call(args...); code != 2 || !strings.Contains(stderr, "request-file") {
			t.Fatalf("%v: code=%d stderr=%q; want delimiter usage refusal", args, code, stderr)
		}
	}
}
