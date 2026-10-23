package declindex

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// readSources records the blob of every Lean source below the configured roots as it exists in tree.Source. Git's
// index names unchanged tracked files; hash-object gives the bytes refresh actually reads for modified and
// untracked files. A source directory that is not a Git work tree has no source-blob snapshot.
func readSources(tree Tree) (map[string]string, error) {
	check := exec.Command("git", "-C", tree.Source, "rev-parse", "--is-inside-work-tree")
	out, err := check.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		return nil, nil
	}
	if strings.TrimSpace(string(out)) != "true" {
		return nil, nil
	}

	paths := sourcePathspecs(tree.Roots)
	args := append([]string{"-C", tree.Source, "ls-files", "-s", "-z", "--"}, paths...)
	tracked, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked sources: %w", err)
	}
	sources, err := parseTrackedSources(tracked)
	if err != nil {
		return nil, err
	}

	args = append([]string{"-C", tree.Source, "status", "--porcelain", "-z", "--untracked-files=all", "--"}, paths...)
	status, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("list changed sources: %w", err)
	}
	changed, err := changedSourcePaths(status)
	if err != nil {
		return nil, err
	}
	if len(changed) == 0 {
		return sources, nil
	}

	toHash := make([]string, 0, len(changed))
	for _, p := range changed {
		if _, err := os.Stat(filepath.Join(tree.Source, filepath.FromSlash(p))); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				delete(sources, p)
				continue
			}
			return nil, fmt.Errorf("stat changed source %s: %w", p, err)
		}
		toHash = append(toHash, p)
	}
	if len(toHash) == 0 {
		return sources, nil
	}
	sort.Strings(toHash)
	cmd := exec.Command("git", "-C", tree.Source, "hash-object", "--stdin-paths")
	cmd.Stdin = strings.NewReader(strings.Join(toHash, "\n") + "\n")
	hashes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hash changed sources: %w", err)
	}
	ids := strings.Fields(string(hashes))
	if len(ids) != len(toHash) {
		return nil, fmt.Errorf("hash changed sources: got %d blob ids for %d paths", len(ids), len(toHash))
	}
	for i, p := range toHash {
		sources[p] = ids[i]
	}
	return sources, nil
}

func sourcePathspecs(roots Roots) []string {
	keys := make([]string, 0, len(roots))
	for root := range roots {
		if root != "" {
			keys = append(keys, root)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, root := range keys {
		out = append(out, root+"/", root+".lean")
	}
	return out
}

func parseTrackedSources(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, fmt.Errorf("git ls-files output is not NUL terminated")
		}
		record := string(raw[:end])
		raw = raw[end+1:]
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
		out[path] = parts[1]
	}
	return out, nil
}

func changedSourcePaths(raw []byte) ([]string, error) {
	set := map[string]bool{}
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, fmt.Errorf("git status output is not NUL terminated")
		}
		record := string(raw[:end])
		raw = raw[end+1:]
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("git status record %q is malformed", record)
		}
		if strings.HasSuffix(record[3:], ".lean") {
			set[record[3:]] = true
		}
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			end := bytes.IndexByte(raw, 0)
			if end < 0 {
				return nil, fmt.Errorf("git status rename record has no original path")
			}
			raw = raw[end+1:]
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
