package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchRepository commits Alpha/Core.lean (namespace Alpha, def gone := 0,
// a blank line, def keep := 1) and Alpha/Use.lean, which uses Alpha.gone, as
// the base. Then, for each
// member, it commits a branch from the base with one edit of Alpha/Core.lean:
// a value -<text> removes the line <text>, a value +<text> adds <text> before
// `end Alpha`. It returns each branch's head.
func scratchRepository(t *testing.T, members map[string]string) (repo, base string, heads map[string]string) {
	t.Helper()
	repo = t.TempDir()
	command(t, repo, "init", "-q")
	command(t, repo, "config", "user.email", "test@example.invalid")
	command(t, repo, "config", "user.name", "Test")
	core := "namespace Alpha\ndef gone := 0\n\ndef keep := 1\nend Alpha\n"
	if err := os.MkdirAll(filepath.Join(repo, "Alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "Alpha", "Core.lean"), core)
	write(t, filepath.Join(repo, "Alpha", "Use.lean"), "import Alpha.Core\ndef use := Alpha.gone\n")
	command(t, repo, "add", ".")
	command(t, repo, "commit", "-qm", "base")
	base = command(t, repo, "rev-parse", "HEAD")
	heads = make(map[string]string, len(members))
	for id, edit := range members {
		command(t, repo, "checkout", "-q", "-b", id, base)
		var edited string
		switch {
		case strings.HasPrefix(edit, "-"):
			edited = strings.Replace(core, strings.TrimPrefix(edit, "-")+"\n", "", 1)
		case strings.HasPrefix(edit, "+"):
			edited = strings.Replace(core, "end Alpha\n", strings.TrimPrefix(edit, "+")+"\nend Alpha\n", 1)
		default:
			t.Fatalf("member %s edit %q is not -<line> or +<line>", id, edit)
		}
		write(t, filepath.Join(repo, "Alpha", "Core.lean"), edited)
		command(t, repo, "commit", "-qam", id)
		heads[id] = command(t, repo, "rev-parse", "HEAD")
	}
	command(t, repo, "checkout", "-q", base)
	return repo, base, heads
}

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeConfig writes a land configuration and returns its path.
func writeConfig(t *testing.T, values map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "land.json")
	write(t, path, string(raw))
	return path
}
