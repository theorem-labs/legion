package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// cmdLandHash answers a landing's declaration reads at one Git tree (declindex.Store.LandHash), from LAND_TREE,
// LAND_GIT_DIR, LAND_NAMES or LAND_NAMES_FILE, and LAND_BUILD_DIR. It reads the index read-only and never the
// configured tree.
func cmdLandHash(ctx context.Context, e *env, args []string) int {
	if len(args) != 0 {
		return fail(e.stderr, &usageError{"land-hash takes no arguments; it reads LAND_TREE, LAND_GIT_DIR, and LAND_NAMES"})
	}
	tree := strings.TrimSpace(e.getenv("LAND_TREE"))
	// LAND_TREE goes only to Git, which reads an object id in either case, so it may be uppercase; LAND_COMMIT is
	// compared as text with the commits the index records, so it must be lowercase.
	if !declindex.IsObjectID(strings.ToLower(tree)) {
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
	if err := e.openReadOnly(ctx, true); err != nil {
		return fail(e.stderr, err)
	}
	defer e.close()
	answers, err := e.store.LandHash(ctx, declindex.LandHashRequest{
		GitDir:   gitDir,
		Tree:     tree,
		Names:    names,
		BuildDir: strings.TrimSpace(e.getenv("LAND_BUILD_DIR")),
		LibRel:   e.cfg.LibRel,
		Roots:    e.cfg.TreeSpec().Roots,
	})
	if err != nil {
		return fail(e.stderr, &unableError{err})
	}
	for _, answer := range answers {
		fmt.Fprintf(e.stdout, "%s\t%s\t%s\n", answer.Name, answer.Hash, answer.Path)
	}
	return 0
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
