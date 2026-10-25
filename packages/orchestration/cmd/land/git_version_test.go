package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit puts a git on PATH that answers `git --version` with version and does nothing else.
func fakeGit(t *testing.T, version string) {
	t.Helper()
	directory := t.TempDir()
	executable := filepath.Join(directory, "git")
	write(t, executable, "#!/bin/sh\necho \"git version "+version+"\"\n")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The program refuses to start on a git whose merge-tree cannot write a tree, before it reads a configuration.
func TestLandRefusesGitBeforeTheMergeTreeVersion(t *testing.T) {
	for _, version := range []string{"2.37.9", "2.9.5", "1.9.1"} {
		t.Run(version, func(t *testing.T) {
			fakeGit(t, version)
			if err := requireGit(context.Background()); err == nil || !strings.Contains(err.Error(), "2.38") {
				t.Fatalf("requireGit() error = %v, want a refusal that names git 2.38", err)
			}
			err := run(context.Background(), []string{"status", "--config", filepath.Join(t.TempDir(), "none.json")}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "2.38") {
				t.Fatalf("run() error = %v, want the start check to refuse git %s", err, version)
			}
		})
	}
}

func TestLandAcceptsGitFromTheMergeTreeVersion(t *testing.T) {
	for _, version := range []string{"2.38.0", "2.43.0", "2.39.3 (Apple Git-146)", "3.1.2"} {
		t.Run(version, func(t *testing.T) {
			fakeGit(t, version)
			if err := requireGit(context.Background()); err != nil {
				t.Fatalf("requireGit() error = %v, want git %s accepted", err, version)
			}
		})
	}
}
