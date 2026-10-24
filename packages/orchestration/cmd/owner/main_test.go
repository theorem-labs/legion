package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"frobnicate"}, {"--deploy", "deploy.yaml", "librarian-intake"}, {}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, func(string) string { return "" }, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "usage: owner") {
			t.Errorf("owner %v exited %d, want 2 with the usage text: %s", args, code, stderr.String())
		}
	}
}
