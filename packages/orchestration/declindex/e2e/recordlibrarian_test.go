package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A gate configured with `librarian: [record-librarian, --, COMMAND, ARGS...]` appends its own arguments after the
// configured command (conceptgate/librarian.go: Command[0], append(Command[1:], "--id", id, "--proposer", proposer,
// "--proposal", path)), so record-librarian receives `-- COMMAND ARGS... --id ID --proposer P --proposal FILE`. It
// must record that call and run COMMAND with its own arguments, then the gate's.
func TestRecordLibrarianWrapsTheConfiguredCommandAsTheGateCallsIt(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	command := filepath.Join(dir, "intake")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+seen+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(dir, "proposal")
	if err := os.WriteFile(proposal, []byte("envelope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configured := []string{"--", command, "--ledger-dsn-file", "/run/dsn"}
	gate := []string{"--id", "near-0123456789abcdef", "--proposer", "function:alloc_mnt_ns", "--proposal", proposal}
	record := filepath.Join(dir, "record.tsv")
	cmd := exec.Command("./record-librarian", append(configured, gate...)...)
	cmd.Env = append(os.Environ(), "LIBRARIAN_RECORD="+record)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("record-librarian %v: %v\n%s", append(configured, gate...), err, out)
	}
	line, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(line), "\tnear-0123456789abcdef\tfunction:alloc_mnt_ns\t") {
		t.Fatalf("record %q, %v: want one line for the gate's call", line, err)
	}
	got, err := os.ReadFile(seen)
	want := strings.Join([]string{"--ledger-dsn-file", "/run/dsn", "--id", "near-0123456789abcdef", "--proposer",
		"function:alloc_mnt_ns", "--proposal"}, "\n")
	if err != nil || !strings.HasPrefix(string(got), want+"\n") {
		t.Fatalf("the wrapped command got %q (%v), want its own arguments, then the gate's", got, err)
	}
}

// The README's form names the call first and the command after `--`; the command then runs with its own arguments
// only, and its exit status is the helper's.
func TestRecordLibrarianRunsTheCommandAfterItsOwnArguments(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	command := filepath.Join(dir, "intake")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+seen+"'\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proposal := filepath.Join(dir, "proposal")
	if err := os.WriteFile(proposal, []byte("envelope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record.tsv")
	cmd := exec.Command("./record-librarian", "--id", "near-0123456789abcdef", "--proposer", "function:open_file",
		"--proposal", proposal, "--", command, "one", "two")
	cmd.Env = append(os.Environ(), "LIBRARIAN_RECORD="+record)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("record-librarian: %v, want the command's exit 7\n%s", err, out)
	}
	if line, err := os.ReadFile(record); err != nil || !strings.Contains(string(line), "\tnear-0123456789abcdef\tfunction:open_file\t") {
		t.Fatalf("record %q, %v", line, err)
	}
	if got, err := os.ReadFile(seen); err != nil || string(got) != "one\ntwo\n" {
		t.Fatalf("the command got %q (%v), want only its own arguments", got, err)
	}
}
