package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// ledgerCall runs the ledger command line in this process against one test database, and returns its exit code,
// stdout and stderr.
type ledgerCall func(args ...string) (int, string, string)

// ledgerCommand gives a test its own database and a ledgerCall bound to it.
func ledgerCommand(t *testing.T) ledgerCall {
	t.Helper()
	dsn := testpg.Database(t, "ledger_command")
	return func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, func(key string) string {
			if key == "LEDGER_DSN" {
				return dsn
			}
			return ""
		}, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
}

// mustExit fails the test unless the command exits with want.
func mustExit(t *testing.T, call ledgerCall, want int, args ...string) {
	t.Helper()
	if code, _, stderr := call(args...); code != want {
		t.Fatalf("%v: code=%d stderr=%q, want %d", args, code, stderr, want)
	}
}

// mustRun fails the test unless the command exits 0, and returns its stdout.
func mustRun(t *testing.T, call ledgerCall, args ...string) string {
	t.Helper()
	code, stdout, stderr := call(args...)
	if code != 0 {
		t.Fatalf("%v: code=%d stdout=%q stderr=%q, want 0", args, code, stdout, stderr)
	}
	return stdout
}
