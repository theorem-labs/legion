package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// cmdLandHash answers a landing's declaration reads at one Git tree. A module whose source LAND_TREE has with the
// blob the index recorded comes from the index; changed modules come only from verified candidate build output.
func cmdLandHash(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"land-hash takes no arguments; it reads LAND_TREE, LAND_GIT_DIR, and LAND_NAMES"})
	}
	tree := strings.TrimSpace(e.getenv("LAND_TREE"))
	if !validTreeID(tree) {
		return fail(e.stderr, &unableError{fmt.Errorf("LAND_TREE must be a 40-hex tree id")})
	}
	gitDir := strings.TrimSpace(e.getenv("LAND_GIT_DIR"))
	if gitDir == "" {
		return fail(e.stderr, &unableError{fmt.Errorf("LAND_GIT_DIR is empty")})
	}
	names, err := landNames(e.getenv)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if err := e.open(true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	snapshot, err := e.store.LandSnapshot(ctx, names)
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	if !snapshot.HasSources {
		return fail(e.stderr, &unableError{fmt.Errorf("the index has no git source blobs")})
	}
	roots := e.cfg.TreeSpec().Roots
	candidate, err := gitTreeSources(ctx, gitDir, tree, declindex.SourcePathspecs(roots))
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	changed := make([]string, 0)
	for path, blob := range candidate {
		if snapshot.Sources[path] != blob {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	changedDecls := map[string][]landDeclaration{}
	if len(changed) > 0 {
		buildDir := strings.TrimSpace(e.getenv("LAND_BUILD_DIR"))
		if buildDir == "" {
			return fail(e.stderr, &unableError{fmt.Errorf("LAND_BUILD_DIR is required for changed source files")})
		}
		info, err := os.Stat(buildDir)
		if err != nil || !info.IsDir() {
			return fail(e.stderr, &unableError{fmt.Errorf("LAND_BUILD_DIR %s is not a directory", buildDir)})
		}
		for _, path := range changed {
			rel := strings.TrimSuffix(path, ".lean")
			blob, err := gitBlob(ctx, gitDir, candidate[path])
			if err != nil {
				return fail(e.stderr, &unableError{fmt.Errorf("read %s from LAND_TREE: %w", path, err)})
			}
			freshness, err := declindex.Fresh(buildDir, e.cfg.LibRel, rel, blob)
			if err != nil {
				return fail(e.stderr, &unableError{fmt.Errorf("check %s: %w", rel, err)})
			}
			switch freshness.State {
			case declindex.FreshnessNotBuilt:
				continue
			case declindex.FreshnessStale:
				return fail(e.stderr, &unableError{fmt.Errorf("%s: %s", rel, freshness.Reason)})
			}
			ilean, err := os.ReadFile(filepath.Join(buildDir, filepath.FromSlash(e.cfg.LibRel), filepath.FromSlash(rel)+".ilean"))
			if err != nil {
				return fail(e.stderr, &unableError{fmt.Errorf("read candidate %s.ilean: %w", rel, err)})
			}
			module, err := declindex.ParseModule(rel, ilean, blob, roots)
			if err != nil {
				return fail(e.stderr, &unableError{fmt.Errorf("parse candidate %s: %w", rel, err)})
			}
			if module.Stale != 0 {
				return fail(e.stderr, &unableError{fmt.Errorf("%s: %d declarations do not match the candidate source", rel, module.Stale)})
			}
			for _, declaration := range module.Decls {
				changedDecls[declaration.Name] = append(changedDecls[declaration.Name], landDeclaration{
					path: path, hash: declaration.StmtHash,
				})
			}
		}
	}

	answers := make([]landAnswer, 0, len(names))
	for _, name := range names {
		declarations := append([]landDeclaration(nil), changedDecls[name]...)
		for _, declaration := range snapshot.Declarations[name] {
			rel := strings.TrimSuffix(declaration.Path, ".lean")
			if _, indexed := snapshot.Modules[rel]; !indexed {
				continue
			}
			// The index answers only for a source LAND_TREE has with the blob the index read; a source missing
			// from both must not count as unchanged.
			indexedBlob, indexed := snapshot.Sources[declaration.Path]
			if blob, inTree := candidate[declaration.Path]; indexed && inTree && blob == indexedBlob {
				declarations = append(declarations, landDeclaration{path: declaration.Path, hash: declaration.StmtHash})
			}
		}
		answer, err := answerLandHash(name, declarations)
		if err != nil {
			return fail(e.stderr, &unableError{err})
		}
		answers = append(answers, answer)
	}
	for _, answer := range answers {
		fmt.Fprintf(e.stdout, "%s\t%s\t%s\n", answer.name, answer.hash, answer.path)
	}
	return 0
}

type landDeclaration struct{ path, hash string }
type landAnswer struct{ name, hash, path string }

// answerLandHash gives name's answer line from the declarations found for it. A path the line prints must not
// contain the line's separators: a tab, carriage return or newline, or the comma that joins an ambiguous answer's
// paths. A path that no answer prints is not checked.
func answerLandHash(name string, declarations []landDeclaration) (landAnswer, error) {
	if len(declarations) == 0 {
		return landAnswer{name: name, hash: "absent", path: "-"}, nil
	}
	for _, declaration := range declarations {
		if strings.ContainsAny(declaration.path, "\t\r\n,") {
			return landAnswer{}, fmt.Errorf("%s: source path %q cannot be printed safely", name, declaration.path)
		}
	}
	if len(declarations) == 1 {
		if declarations[0].hash == "" {
			return landAnswer{}, fmt.Errorf("%s has no statement hash", name)
		}
		return landAnswer{name: name, hash: declarations[0].hash, path: declarations[0].path}, nil
	}
	paths := make([]string, len(declarations))
	for i, declaration := range declarations {
		paths[i] = declaration.path
	}
	sort.Strings(paths)
	return landAnswer{name: name, hash: "ambiguous", path: strings.Join(paths, ",")}, nil
}

func validTreeID(tree string) bool {
	if len(tree) != 40 {
		return false
	}
	_, err := hex.DecodeString(tree)
	return err == nil
}

func landNames(getenv func(string) string) ([]string, error) {
	fromEnv := []byte(getenv("LAND_NAMES"))
	file := strings.TrimSpace(getenv("LAND_NAMES_FILE"))
	hasEnv := len(fromEnv) != 0
	hasFile := file != ""
	if !hasEnv && !hasFile {
		return nil, fmt.Errorf("one of LAND_NAMES or LAND_NAMES_FILE is required")
	}
	var names []string
	var err error
	if hasEnv {
		names, err = parseLandNames(fromEnv)
		if err != nil {
			return nil, fmt.Errorf("LAND_NAMES: %w", err)
		}
	}
	if hasFile {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("LAND_NAMES_FILE: %w", err)
		}
		fromFile, err := parseLandNames(raw)
		if err != nil {
			return nil, fmt.Errorf("LAND_NAMES_FILE: %w", err)
		}
		if hasEnv && !reflect.DeepEqual(names, fromFile) {
			return nil, fmt.Errorf("LAND_NAMES and LAND_NAMES_FILE differ")
		}
		names = fromFile
	}
	return names, nil
}

func parseLandNames(raw []byte) ([]string, error) {
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("not a JSON array of names: %w", err)
	}
	if names == nil && string(bytes.TrimSpace(raw)) != "[]" {
		return nil, fmt.Errorf("not a JSON array of names")
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%q is not a non-empty name", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s is requested twice", name)
		}
		seen[name] = true
	}
	return names, nil
}

// gitTreeSources lists the Lean source blobs of LAND_TREE that pathspecs select, keyed by path.
func gitTreeSources(ctx context.Context, gitDir, tree string, pathspecs []string) (map[string]string, error) {
	if err := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "cat-file", "-e", tree+"^{tree}").Run(); err != nil {
		return nil, fmt.Errorf("LAND_GIT_DIR cannot read LAND_TREE: %w", err)
	}
	args := []string{"--git-dir=" + gitDir, "ls-tree", "-r", "-z", "--full-tree", tree, "--"}
	args = append(args, pathspecs...)
	raw, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("list LAND_TREE sources: %w", err)
	}
	out := map[string]string{}
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, fmt.Errorf("git ls-tree output is not NUL terminated")
		}
		record := string(raw[:end])
		raw = raw[end+1:]
		fields, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-tree record has no path")
		}
		parts := strings.Fields(fields)
		if len(parts) != 3 || parts[1] != "blob" || !strings.HasSuffix(path, ".lean") {
			continue
		}
		out[path] = parts[2]
	}
	return out, nil
}

func gitBlob(ctx context.Context, gitDir, id string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "cat-file", "blob", id).Output()
}
