package declindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// LandHashRequest is one request of the landing program's hasher (`declindex land-hash`): the statement hashes of
// Names as the Git tree TreeID has them.
type LandHashRequest struct {
	// GitDir is a Git directory that can read TreeID and its blobs (LAND_GIT_DIR).
	GitDir string
	// TreeID is the 40-hex id of the repository's root tree to answer (LAND_TREE), also when the indexed tree is a
	// directory below that root.
	TreeID string
	// Names are distinct, non-empty full declaration names.
	Names []string
	// BuildDir is the candidate build root (LAND_BUILD_DIR), the candidate's copy of the indexed tree. Only a
	// source that differs from the one the index read needs it.
	BuildDir string
	// LibRel is the library directory below BuildDir, and Roots the project's module roots: the configuration's.
	LibRel string
	Roots  Roots
}

// LandHashAnswer is the answer for one name: its statement hash and repository-relative source path; "absent" and
// "-" when TreeID has no such declaration; "ambiguous" and the sorted, comma-joined paths when it has several.
type LandHashAnswer struct {
	Name, Hash, Path string
}

// LandHash answers r at its Git tree, one answer per name in request order. A module whose source the tree has
// with the blob the index recorded comes from the index; a changed module comes only from verified candidate build
// output. The index keys sources and declarations relative to the indexed tree; the tree in r is the repository's,
// so an indexed tree below the repository's root is listed below the prefix that refresh recorded, and answers
// print paths with that prefix. It reads the index only, never the configured tree.
func (s *Store) LandHash(ctx context.Context, r LandHashRequest) ([]LandHashAnswer, error) {
	snapshot, err := s.LandSnapshot(ctx, r.Names)
	if err != nil {
		return nil, err
	}
	if !snapshot.HasSources {
		return nil, fmt.Errorf("the index has no git source blobs")
	}
	prefix := snapshot.Prefix
	candidate, err := gitTreeSources(ctx, r.GitDir, r.TreeID, prefix, sourcePathspecs(r.Roots))
	if err != nil {
		return nil, err
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
		if r.BuildDir == "" {
			return nil, fmt.Errorf("LAND_BUILD_DIR is required for changed source files")
		}
		info, err := os.Stat(r.BuildDir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("LAND_BUILD_DIR %s is not a directory", r.BuildDir)
		}
		for _, path := range changed {
			rel := strings.TrimSuffix(path, ".lean")
			blob, err := gitBlob(ctx, r.GitDir, candidate[path])
			if err != nil {
				return nil, fmt.Errorf("read %s from LAND_TREE: %w", prefix+path, err)
			}
			freshness, err := Fresh(r.BuildDir, r.LibRel, rel, blob)
			if err != nil {
				return nil, fmt.Errorf("check %s: %w", rel, err)
			}
			switch freshness.State {
			case FreshnessNotBuilt:
				continue
			case FreshnessStale:
				return nil, fmt.Errorf("%s: %s", rel, freshness.Reason)
			}
			ilean, err := os.ReadFile(filepath.Join(r.BuildDir, filepath.FromSlash(r.LibRel), filepath.FromSlash(rel)+".ilean"))
			if err != nil {
				return nil, fmt.Errorf("read candidate %s.ilean: %w", rel, err)
			}
			module, err := ParseModule(rel, ilean, blob, r.Roots)
			if err != nil {
				return nil, fmt.Errorf("parse candidate %s: %w", rel, err)
			}
			if module.Stale != 0 {
				return nil, fmt.Errorf("%s: %d declarations do not match the candidate source", rel, module.Stale)
			}
			for _, declaration := range module.Decls {
				changedDecls[declaration.Name] = append(changedDecls[declaration.Name], landDeclaration{
					path: prefix + path, hash: declaration.StmtHash,
				})
			}
		}
	}

	answers := make([]LandHashAnswer, 0, len(r.Names))
	for _, name := range r.Names {
		declarations := slices.Clone(changedDecls[name])
		for _, declaration := range snapshot.Declarations[name] {
			rel := strings.TrimSuffix(declaration.Path, ".lean")
			if _, indexed := snapshot.Modules[rel]; !indexed {
				continue
			}
			// The index answers only for a source the tree has with the blob the index read; a source missing
			// from both must not count as unchanged.
			indexedBlob, indexed := snapshot.Sources[declaration.Path]
			if blob, inTree := candidate[declaration.Path]; indexed && inTree && blob == indexedBlob {
				declarations = append(declarations, landDeclaration{path: prefix + declaration.Path, hash: declaration.StmtHash})
			}
		}
		answer, err := answerLandHash(name, declarations)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

// landDeclaration is one declaration an answer can print: path is repository-relative.
type landDeclaration struct{ path, hash string }

// answerLandHash gives name's answer line from the declarations found for it. A path the line prints must not
// contain the line's separators: a tab, carriage return or newline, or the comma that joins an ambiguous answer's
// paths. A path that no answer prints is not checked.
func answerLandHash(name string, declarations []landDeclaration) (LandHashAnswer, error) {
	if len(declarations) == 0 {
		return LandHashAnswer{Name: name, Hash: "absent", Path: "-"}, nil
	}
	for _, declaration := range declarations {
		if strings.ContainsAny(declaration.path, "\t\r\n,") {
			return LandHashAnswer{}, fmt.Errorf("%s: source path %q cannot be printed safely", name, declaration.path)
		}
	}
	if len(declarations) == 1 {
		if declarations[0].hash == "" {
			return LandHashAnswer{}, fmt.Errorf("%s has no statement hash", name)
		}
		return LandHashAnswer{Name: name, Hash: declarations[0].hash, Path: declarations[0].path}, nil
	}
	paths := make([]string, len(declarations))
	for i, declaration := range declarations {
		paths[i] = declaration.path
	}
	sort.Strings(paths)
	return LandHashAnswer{Name: name, Hash: "ambiguous", Path: strings.Join(paths, ",")}, nil
}

// gitTreeSources lists the Lean source blobs of the tree treeID that pathspecs select below prefix, the indexed
// tree's directory in the repository. It keys each blob by its path without prefix, as the index keys sources.
func gitTreeSources(ctx context.Context, gitDir, treeID, prefix string, pathspecs []string) (map[string]string, error) {
	if err := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "cat-file", "-e", treeID+"^{tree}").Run(); err != nil {
		return nil, fmt.Errorf("LAND_GIT_DIR cannot read LAND_TREE: %w", err)
	}
	args := []string{"--git-dir=" + gitDir, "ls-tree", "-r", "-z", "--full-tree", treeID, "--"}
	for _, pathspec := range pathspecs {
		args = append(args, prefix+pathspec)
	}
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
		if rel, inTree := strings.CutPrefix(path, prefix); inTree {
			out[rel] = parts[2]
		}
	}
	return out, nil
}

func gitBlob(ctx context.Context, gitDir, id string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "cat-file", "blob", id).Output()
}
