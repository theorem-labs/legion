package declindex

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// readSources records the blob of every Lean source below the configured roots as it exists in tree.Source, keyed
// by its path relative to tree.Source, and tree.Source's prefix: its path below the root of its Git work tree,
// with a trailing slash, empty at that root. Git's index names unchanged tracked files; hash-object gives the bytes
// refresh actually reads for modified and untracked files. Git lists both by paths from the work tree's root, so
// a tree below that root strips its prefix. A source directory that is not a Git work tree has no source-blob
// snapshot.
func readSources(tree Tree) (blobs map[string]string, prefix string, err error) {
	check := exec.Command("git", "-C", tree.Source, "rev-parse", "--is-inside-work-tree")
	out, err := check.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, "", err
		}
		return nil, "", nil
	}
	if strings.TrimSpace(string(out)) != "true" {
		return nil, "", nil
	}

	top, prefix, err := workTreeLocation(tree.Source)
	if err != nil {
		return nil, "", err
	}
	paths := sourcePathspecs(tree.Roots)
	args := append([]string{"-C", tree.Source, "ls-files", "-s", "-z", "--full-name", "--"}, paths...)
	tracked, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, "", fmt.Errorf("list tracked sources: %w", err)
	}
	sources, err := parseTrackedSources(tracked, prefix)
	if err != nil {
		return nil, "", err
	}

	args = append([]string{"-C", tree.Source, "status", "--porcelain", "-z", "--untracked-files=all", "--"}, paths...)
	status, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, "", fmt.Errorf("list changed sources: %w", err)
	}
	changed, err := changedSourcePaths(status, prefix)
	if err != nil {
		return nil, "", err
	}
	toHash := make([]string, 0, len(changed))
	for _, p := range changed {
		ok, err := fileExists(filepath.Join(tree.Source, filepath.FromSlash(p)))
		if err != nil {
			return nil, "", fmt.Errorf("stat changed source %s: %w", p, err)
		}
		if !ok {
			delete(sources, p)
			continue
		}
		toHash = append(toHash, p)
	}
	if len(toHash) == 0 {
		return sources, prefix, nil
	}
	// hash-object resolves --stdin-paths from the work tree's root, not from a subdirectory given with -C, so it
	// runs at that root with the repository-relative paths.
	cmd := exec.Command("git", "-C", top, "hash-object", "--stdin-paths")
	var stdin strings.Builder
	for _, p := range toHash {
		stdin.WriteString(prefix + p + "\n")
	}
	cmd.Stdin = strings.NewReader(stdin.String())
	hashes, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("hash changed sources: %w", err)
	}
	ids := strings.Fields(string(hashes))
	if len(ids) != len(toHash) {
		return nil, "", fmt.Errorf("hash changed sources: got %d blob ids for %d paths", len(ids), len(toHash))
	}
	for i, p := range toHash {
		sources[p] = ids[i]
	}
	return sources, prefix, nil
}

// sourcePathspecs are the Git pathspecs, relative to the tree, of the Lean sources below roots: each root's
// directory and its own file. An empty root names no source and is dropped. Refresh records the sources these
// select, and land-hash lists the same ones in LAND_TREE.
func sourcePathspecs(roots Roots) []string {
	out := make([]string, 0, len(roots)*2)
	for _, root := range slices.Sorted(maps.Keys(roots)) {
		if root != "" {
			out = append(out, root+"/", root+".lean")
		}
	}
	return out
}

// workTreeLocation gives the root of dir's Git work tree and dir's prefix below it (see readSources).
func workTreeLocation(dir string) (top, prefix string, err error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel", "--show-prefix").Output()
	if err != nil {
		return "", "", fmt.Errorf("find %s in its Git work tree: %w", dir, err)
	}
	top, prefix, ok := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\n")
	if !ok || top == "" || (prefix != "" && !strings.HasSuffix(prefix, "/")) {
		return "", "", fmt.Errorf("find %s in its Git work tree: git rev-parse gave %q", dir, out)
	}
	return top, prefix, nil
}

// nulRecords yields the NUL-terminated records of the -z output of a git command in order, and an error instead of
// a last record that has no NUL.
func nulRecords(command string, raw []byte) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for len(raw) > 0 {
			end := bytes.IndexByte(raw, 0)
			if end < 0 {
				yield("", fmt.Errorf("%s output is not NUL terminated", command))
				return
			}
			if !yield(string(raw[:end]), nil) {
				return
			}
			raw = raw[end+1:]
		}
	}
}

// parseTrackedSources reads `git ls-files -s -z --full-name` output. It keys each stage-0 Lean source by its path
// without prefix and skips a path outside the tree.
func parseTrackedSources(raw []byte, prefix string) (map[string]string, error) {
	out := map[string]string{}
	for record, err := range nulRecords("git ls-files", raw) {
		if err != nil {
			return nil, err
		}
		fields, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-files record has no path")
		}
		parts := strings.Fields(fields)
		if len(parts) != 3 {
			return nil, fmt.Errorf("git ls-files record %q has %d fields", record, len(parts))
		}
		if parts[2] != "0" || !strings.HasSuffix(path, ".lean") {
			continue
		}
		if rel, inTree := strings.CutPrefix(path, prefix); inTree {
			out[rel] = parts[1]
		}
	}
	return out, nil
}

// changedSourcePaths reads `git status --porcelain -z` output, whose paths are from the repository's root. It gives
// each changed Lean source's path without prefix and skips a path outside the tree.
func changedSourcePaths(raw []byte, prefix string) ([]string, error) {
	set := map[string]bool{}
	// original is set when the next record is the original path of a rename or copy, which is skipped.
	original := false
	for record, err := range nulRecords("git status", raw) {
		if err != nil {
			return nil, err
		}
		if original {
			original = false
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("git status record %q is malformed", record)
		}
		if rel, inTree := strings.CutPrefix(record[3:], prefix); inTree && strings.HasSuffix(rel, ".lean") {
			set[rel] = true
		}
		original = record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C'
	}
	if original {
		return nil, fmt.Errorf("git status rename record has no original path")
	}
	return slices.Sorted(maps.Keys(set)), nil
}
