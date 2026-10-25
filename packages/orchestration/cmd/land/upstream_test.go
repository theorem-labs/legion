package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// A configured member carries the upstream commit of its ready record: the member whose head is that merge of the
// base lands with the batch, and the member whose head is not is returned as upstream_mismatch.
func TestRunCommandChecksConfiguredUpstreamMembers(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def base := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "root")
	root := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def main := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")

	command(t, repo, "checkout", "-q", "-B", "upstream", root)
	write(t, filepath.Join(repo, "Upstream.lean"), "def upstream := 0\n")
	command(t, repo, "add", "Upstream.lean")
	command(t, repo, "commit", "-qm", "upstream")
	upstream := command(t, repo, "rev-parse", "HEAD")
	merged := command(t, repo, "merge-tree", "--write-tree", "--no-messages", base, upstream)
	synced := command(t, repo, "commit-tree", merged, "-p", base, "-p", upstream, "-m", "sync")
	stale := command(t, repo, "commit-tree", merged, "-p", root, "-p", upstream, "-m", "stale sync")

	config := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"batchId":    "batch",
		"repository": map[string]any{"name": "owner/repo", "path": repo, "git_dir": "/srv/land/repo.git"},
		"base":       base,
		"baseBranch": "trunk",
		"statePath":  filepath.Join(t.TempDir(), "state.json"),
		"members": []map[string]any{
			{"migration": "sync", "generation": 1, "head": synced, "upstream": upstream},
			{"migration": "stale", "generation": 1, "head": stale, "upstream": upstream},
		},
		"build":     []string{"sh", "-c", "exit 0"},
		"hasher":    []string{"sh", "-c", "exit 0"},
		"toolchain": []string{"sh", "-c", "printf tool"},
		"ledger": map[string]any{
			"check":          []string{"sh", "-c", "printf '{\"pass\":true}\\n'"},
			"return":         []string{"sh", "-c", "exit 0"},
			"land":           []string{"sh", "-c", "exit 0"},
			"status":         []string{"sh", "-c", "printf '{\"complete\":false}\\n'"},
			"migrationState": []string{"sh", "-c", "printf '{\"state\":\"open\"}\\n'"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, config, string(raw))
	var output bytes.Buffer

	if err := run(context.Background(), []string{"run", "--config", config}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var batch struct {
		State         string `json:"state"`
		ActiveMembers []struct {
			Migration string `json:"migration"`
			Upstream  string `json:"upstream"`
		} `json:"activeMembers"`
		Dropped []struct {
			Migration string `json:"migration"`
			Reason    string `json:"reason"`
		} `json:"dropped"`
	}
	if err := json.Unmarshal(output.Bytes(), &batch); err != nil {
		t.Fatalf("decode run output %q: %v", output.String(), err)
	}
	if batch.State != "green" || len(batch.ActiveMembers) != 1 ||
		batch.ActiveMembers[0].Migration != "sync" || batch.ActiveMembers[0].Upstream != upstream {
		t.Fatalf("run output = %q, want green with the matching upstream member", output.String())
	}
	if len(batch.Dropped) != 1 || batch.Dropped[0].Migration != "stale" || batch.Dropped[0].Reason != "upstream_stale_base" {
		t.Fatalf("dropped = %+v, want the stale sync returned as upstream_stale_base", batch.Dropped)
	}
}
