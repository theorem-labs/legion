package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// INFRA-145 plan v5, E3: "librarian: [record-librarian, --, owner, librarian-intake]", and README "Acceptance
// drivers": record-librarian "runs the command after --, if one is given, and exits with its status". The gate
// appends its own arguments after the configured command (conceptgate/librarian.go: Command[0],
// append(Command[1:], "--id", id, "--proposer", proposer, "--proposal", path)), so record-librarian receives
// `-- COMMAND ... --id ID --proposer P --proposal FILE`. It must record that call and run COMMAND with the
// gate's arguments.
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
