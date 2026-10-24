package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/ownerharness"
)

// TestMain lets the test binary run as the executables of a message test: linked as owner it is this command, and
// linked as legion or omp it is internal/ownerharness's test-only stand-in for the supervisor or an owner session.
func TestMain(m *testing.M) {
	switch name := filepath.Base(os.Args[0]); name {
	case "owner":
		os.Exit(run(context.Background(), os.Args[1:], os.Getenv, ".", os.Stdout, os.Stderr))
	case "legion", "omp":
		os.Exit(ownerharness.Main(name, os.Args[1:], os.Stdout))
	}
	os.Exit(m.Run())
}

func TestUsageErrorsExitTwo(t *testing.T) {
	f := newCommandFixture(t)
	for _, args := range [][]string{{"need", state}, {"frobnicate"}, {"done", "start:" + state + "@1"}, {}} {
		if code, stderr := f.as(caller).fails(args...); code != 2 || !strings.Contains(stderr, "usage: owner") {
			t.Errorf("owner %v exited %d, want 2 with the usage text: %s", args, code, stderr)
		}
	}
}
