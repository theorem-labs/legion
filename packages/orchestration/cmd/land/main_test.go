package main

import (
	"bytes"
	"context"
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
	if got := output.String(); !strings.Contains(got, "one\tident:gone") {
		t.Fatalf("blame output = %q, want evidence for one", got)
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
