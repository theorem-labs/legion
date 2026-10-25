package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The landing hook's command line as declindex/README.md documents it, run as written after a landing: it brings
// the index to the landed commit, then checks the ledger, and each command exits 0.
func TestTheDocumentedLandingHookRuns(t *testing.T) {
	hook := documentedHook(t)
	config := fixture(t)
	root := filepath.Dir(config)
	seed := filepath.Join(root, "tree")
	writeTestFile(t, filepath.Join(seed, "registry", "registry.yaml"), "version: 2\n")
	git(t, seed, "init", "-q")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "first")
	remote := filepath.Join(root, "remote.git")
	git(t, root, "clone", "-q", "--bare", seed, remote)
	git(t, root, "clone", "-q", remote, filepath.Join(root, "landing-tree"))
	writeTestFile(t, filepath.Join(seed, "landing.txt"), "second\n")
	git(t, seed, "add", "landing.txt")
	git(t, seed, "commit", "-q", "-m", "second")
	landed := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "push", "-q", remote, "HEAD:refs/heads/master")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	writeIndexConfig(t, config, strings.Replace(string(body), "tree: tree\n", "tree: landing-tree\n", 1)+
		"remote: "+remote+"\nbuild: [\"true\"]\nrepo: r\nregistry: registry\n"+
		"contracts:\n  paths: [\"Nowhere/**\"]\n  kinds: [def]\n")
	if r := invoke(t, "--config", config, "refresh"); r.code != 0 {
		t.Fatalf("initial refresh: %+v", r)
	}
	contracts := filepath.Join(root, "contracts.json")
	writeTestFile(t, contracts, "[]\n")
	placeholders := map[string]string{
		"<cfg>":        config,
		"<contracts>":  contracts,
		"<operations>": filepath.Join(root, "operations.jsonl"),
	}
	// The hook's command line carries no connection string: the hook's environment gives it in DECLINDEX_DSN.
	raw, err := os.ReadFile(indexDSNFile(config))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(raw))
	env := func(k string) string {
		switch k {
		case "LAND_COMMIT":
			return landed
		case "DECLINDEX_DSN":
			return dsn
		}
		return ""
	}
	for _, command := range hook {
		args := make([]string, len(command))
		for i, a := range command {
			args[i] = a
			if strings.HasPrefix(a, "<") {
				v, ok := placeholders[a]
				if !ok {
					t.Fatalf("the hook's placeholder %s names nothing this test provides", a)
				}
				args[i] = v
			}
		}
		// The hook's arguments as written, not withIndexDSN's: the commands reach the index through DECLINDEX_DSN
		// alone, so this test proves that the variable works.
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, env, &out, &errOut); code != 0 {
			t.Fatalf("declindex %s exited %d: %s", strings.Join(command, " "), code, errOut.String())
		}
	}
	assertIndexCommit(t, config, landed)
}

// documentedHook reads the landing hook's command line from declindex/README.md: each command that it joins with
// &&, as its arguments after `declindex`.
func documentedHook(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "declindex", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(raw), "The landing hook invokes:\n\n```text\n")
	if !ok {
		t.Fatal("declindex/README.md documents no landing hook")
	}
	line, _, _ := strings.Cut(block, "\n")
	var commands [][]string
	for _, command := range strings.Split(line, " && ") {
		fields := strings.Fields(command)
		if len(fields) < 2 || fields[0] != "declindex" {
			t.Fatalf("the hook's command %q does not run declindex", command)
		}
		commands = append(commands, fields[1:])
	}
	return commands
}
