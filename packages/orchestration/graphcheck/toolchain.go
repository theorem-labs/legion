package graphcheck

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// leanToolchain gives the Lean toolchain that compiles the candidate, which must be the one the base was built
// with: the index configuration's lean.toolchain when it is set; else the toolchain the tree's lean-toolchain file
// names, run straight from elan's toolchains directory, so that the check never depends on the PATH of whoever
// runs it; else, for a tree without that file, the lean on PATH. A lean-toolchain file that names a toolchain elan
// has not installed is an error: another toolchain's compile would not match the base build.
func leanToolchain(ctx context.Context, icfg declindex.Config) (declindex.Lean, error) {
	if icfg.Lean.Toolchain != "" {
		return declindex.FindLean(ctx, icfg.Tree, icfg.Lean.Toolchain)
	}
	raw, err := os.ReadFile(filepath.Join(icfg.Tree, "lean-toolchain"))
	switch {
	case err == nil:
		name := strings.TrimSpace(string(raw))
		prefix := filepath.Join(elanHome(), "toolchains", elanDirName(name))
		lean, err := declindex.FindLean(ctx, icfg.Tree, prefix)
		if err != nil {
			return declindex.Lean{}, fmt.Errorf("the tree's lean-toolchain names %s, which elan has not installed at %s: %w", name, prefix, err)
		}
		return lean, nil
	case errors.Is(err, fs.ErrNotExist):
		return declindex.FindLean(ctx, icfg.Tree, "")
	default:
		return declindex.Lean{}, err
	}
}

// elanHome is elan's directory: ELAN_HOME, else ~/.elan.
func elanHome() string {
	if home := os.Getenv("ELAN_HOME"); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".elan")
}

// elanDirName is the directory elan installs a toolchain into: `owner/repo:release` becomes
// `owner--repo---release`.
func elanDirName(name string) string {
	return strings.NewReplacer("/", "--", ":", "---").Replace(name)
}
