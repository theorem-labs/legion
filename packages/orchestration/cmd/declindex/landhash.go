package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// cmdLandHash is the landing program's statement hasher. It reads LAND_TREE (a git tree id) and LAND_NAMES (a
// JSON array of fully qualified names) and prints exactly `name<TAB>hash`, one line per requested name. The hash
// is the text statement hash at LAND_TREE. The index has it for a declaration whose source file is the same blob
// at LAND_TREE as at the index's commit; a file the candidate changed has no hash before it is built. Any name
// that is missing, declared twice, requested twice, in a changed file, or without a hash makes the whole call
// unable to check (exit 3), and nothing is printed.
func cmdLandHash(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"land-hash takes no arguments; it reads LAND_TREE and LAND_NAMES"})
	}
	tree := strings.TrimSpace(e.getenv("LAND_TREE"))
	var names []string
	if tree == "" {
		return fail(e.stderr, &unableError{fmt.Errorf("LAND_TREE is empty")})
	}
	if err := json.Unmarshal([]byte(e.getenv("LAND_NAMES")), &names); err != nil {
		return fail(e.stderr, &unableError{fmt.Errorf("LAND_NAMES is not a JSON array of names: %w", err)})
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	commit, err := e.store.Meta(ctx, "commit")
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	type hashed struct{ name, hash, path string }
	var out []hashed
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return fail(e.stderr, &unableError{fmt.Errorf("%s is requested twice", n)})
		}
		seen[n] = true
		found, err := e.store.Resolve(ctx, n)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		var exact []hashed
		for _, f := range found {
			if f.Name == n {
				exact = append(exact, hashed{n, f.StmtHash, f.Path})
			}
		}
		switch {
		case len(exact) == 0:
			return fail(e.stderr, &unableError{fmt.Errorf("no declaration %s in the index at %s", n, short(commit))})
		case len(exact) > 1:
			return fail(e.stderr, &unableError{fmt.Errorf("%s is declared %d times", n, len(exact))})
		case exact[0].hash == "":
			return fail(e.stderr, &unableError{fmt.Errorf("%s has no statement hash: its source file is missing or newer than its build", n)})
		}
		out = append(out, exact[0])
	}
	// One git process compares every file's blob at the candidate tree with the one the index read.
	var q strings.Builder
	for _, h := range out {
		fmt.Fprintf(&q, "%s:%s\n%s:%s\n", tree, h.path, commit, h.path)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", e.cfg.Tree, "cat-file", "--batch-check=%(objectname)")
	cmd.Stdin = strings.NewReader(q.String())
	raw, err := cmd.Output()
	if err != nil {
		return fail(e.stderr, &unableError{fmt.Errorf("git cat-file in %s: %w", e.cfg.Tree, err)})
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for _, h := range out {
		var atTree, atIndex string
		if sc.Scan() {
			atTree = sc.Text()
		}
		if sc.Scan() {
			atIndex = sc.Text()
		}
		if strings.HasSuffix(atTree, "missing") || strings.HasSuffix(atIndex, "missing") || atTree == "" || atTree != atIndex {
			return fail(e.stderr, &unableError{fmt.Errorf("%s: %s differs at %s from the index's commit %s, so its hash is "+
				"unknown before a build", h.name, h.path, tree, short(commit))})
		}
	}
	for _, h := range out {
		fmt.Fprintf(e.stdout, "%s\t%s\n", h.name, h.hash)
	}
	return 0
}
