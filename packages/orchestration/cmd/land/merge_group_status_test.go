package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGitHub is a gh executable that logs each call's arguments, one call per line, and answers every GraphQL query
// with a merge queue holding one group, so a program that reads the queue to post a status there would find one.
func fakeGitHub(t *testing.T) (executable, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "gh.log")
	executable = filepath.Join(dir, "gh")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + log + `
case "$*" in
*graphql*) printf '{"data":{"repository":{"mergeQueue":{"entries":{"nodes":[{"headCommit":{"oid":"group-head","tree":{"oid":"group-tree"}},"pullRequest":{"number":7}}]}}}}}' ;;
esac
`
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable, log
}

// The required landing/merge-group status comes from the repository's GitHub Actions workflow (the ruleset names
// the github-actions integration), so the program posts none of its own: two posters of one required status could
// disagree. A serving program with GitHub configured and a group in the merge queue makes no status call and does not
// read the merge queue.
func TestServePostsNoMergeGroupStatus(t *testing.T) {
	repo, _, _ := readyRepository(t)
	_, dsnFile := servingLedger(t, "owner/repo")
	executable, log := fakeGitHub(t)
	config := writeConfig(t, map[string]any{
		"repository":  map[string]any{"name": "owner/repo", "path": repo, "git_dir": "/srv/land/repo.git"},
		"baseBranch":  "trunk",
		"build":       []string{"sh", "-c", "exit 0"},
		"hasher":      []string{"sh", "-c", "exit 0"},
		"toolchain":   []string{"sh", "-c", "printf tool"},
		"ledger":      map[string]any{"dsn_file": dsnFile},
		"escalations": map[string]any{"owner": "landing-operator"},
		"github":      map[string]any{"remote": "origin", "executable": executable, "pollInterval": int64(50 * time.Millisecond)},
		"interval":    int64(time.Hour),
	})

	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := run(ctx, []string{"serve", "--config", config}, &lockedBuffer{}); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("serve() error = %v, want the ended context", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, call := range strings.Split(string(calls), "\n") {
		if strings.Contains(call, "/statuses/") || strings.Contains(call, "landing/merge-group") || strings.Contains(call, "mergeQueue(") {
			t.Fatalf("serve called gh %q: the program posts no merge-group status of its own", call)
		}
	}
}
