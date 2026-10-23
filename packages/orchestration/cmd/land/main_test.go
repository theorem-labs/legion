package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlameCommandPrintsEvidenceBackedMember(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def gone := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def replacement := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	members := filepath.Join(t.TempDir(), "members.tsv")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, members, "one\t"+head+"\n")
	write(t, log, "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: Unknown identifier `gone`\n")
	var output bytes.Buffer

	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--log", log}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, "one\tAlpha\tgone\tunknown\t3\tremoved") {
		t.Fatalf("blame output = %q, want structured evidence for one", got)
	}
}

func TestBlameCommandExcludesListedMembers(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def gone := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def replacement := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	members := filepath.Join(t.TempDir(), "members.tsv")
	excluded := filepath.Join(t.TempDir(), "excluded.txt")
	log := filepath.Join(t.TempDir(), "build.log")
	write(t, members, "one\t"+head+"\n")
	write(t, excluded, "one\n")
	write(t, log, "✖ [1/1] Building Alpha\nerror: Alpha.lean:1:1: Unknown identifier `gone`\n")
	var output bytes.Buffer

	if err := run(context.Background(), []string{"blame", "--repo", repo, "--base", base, "--members", members, "--exclude", excluded, "--log", log}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := output.String(); got != "" {
		t.Fatalf("blame output = %q, want excluded member omitted", got)
	}
}

func TestRunCommandBuildsConfiguredBatch(t *testing.T) {
	repo := t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Alpha.lean"), "def base := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "base")
	base := command(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "Alpha.lean"), "def member := 0\n")
	command(t, repo, "add", "Alpha.lean")
	command(t, repo, "commit", "-qm", "member")
	head := command(t, repo, "rev-parse", "HEAD")

	config := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"batchId":        "batch",
		"repository":     "owner/repo",
		"repositoryPath": repo,
		"base":           base,
		"baseBranch":     "trunk",
		"statePath":      filepath.Join(t.TempDir(), "state.json"),
		"members":        []map[string]any{{"migration": "one", "generation": 1, "head": head}},
		"build":          []string{"sh", "-c", "exit 0"},
		"hasher":         []string{"sh", "-c", "exit 0"},
		"toolchain":      []string{"sh", "-c", "printf tool"},
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
	if got := output.String(); !strings.Contains(got, `"state":"green"`) || strings.Contains(got, `"configurationHash":""`) || strings.Contains(got, `"toolchain":""`) {
		t.Fatalf("run output = %q, want bound green receipt", got)
	}
	output.Reset()
	if err := run(context.Background(), []string{"status", "--config", config}, &output); err != nil {
		t.Fatalf("status error = %v", err)
	}
	if got := output.String(); !strings.Contains(got, `"id":"batch"`) {
		t.Fatalf("status output = %q, want stored batch", got)
	}
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func command(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
