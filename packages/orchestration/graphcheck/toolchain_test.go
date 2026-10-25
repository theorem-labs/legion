package graphcheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// installToolchain makes an elan home holding a toolchain directory with a bin/lean, as elan installs one.
func installToolchain(t *testing.T, dir string) string {
	t.Helper()
	home := t.TempDir()
	prefix := filepath.Join(home, "toolchains", dir)
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "lean"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ELAN_HOME", home)
	return prefix
}

func treeWithToolchain(t *testing.T, name string) string {
	t.Helper()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "lean-toolchain"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return tree
}

// The toolchain comes from the tree's lean-toolchain file, run straight from elan's directory: no lean on PATH is
// needed, and a lean on PATH is not used.
func TestTheTreesLeanToolchainSelectsTheToolchainWithoutPath(t *testing.T) {
	prefix := installToolchain(t, "owner--lean4---release-v1")
	t.Setenv("PATH", t.TempDir())
	lean, err := leanToolchain(context.Background(), declindex.Config{Tree: treeWithToolchain(t, "owner/lean4:release-v1")})
	if err != nil || lean.Prefix != prefix {
		t.Fatalf("leanToolchain = %+v, %v; want prefix %s", lean, err, prefix)
	}
}

// A toolchain elan has not installed is an error naming it: compiling with another would not match the base build.
func TestALeanToolchainElanLacksIsAnError(t *testing.T) {
	installToolchain(t, "owner--lean4---other")
	_, err := leanToolchain(context.Background(), declindex.Config{Tree: treeWithToolchain(t, "owner/lean4:release-v1")})
	if err == nil || !strings.Contains(err.Error(), "owner/lean4:release-v1") {
		t.Fatalf("leanToolchain error %v, want one naming owner/lean4:release-v1", err)
	}
}

// The index configuration's lean.toolchain wins over the tree's file.
func TestTheIndexConfigurationsToolchainWins(t *testing.T) {
	prefix := installToolchain(t, "configured")
	lean, err := leanToolchain(context.Background(), declindex.Config{Tree: treeWithToolchain(t, "owner/lean4:release-v1"),
		Lean: declindex.LeanConfig{Toolchain: prefix}})
	if err != nil || lean.Prefix != prefix {
		t.Fatalf("leanToolchain = %+v, %v; want prefix %s", lean, err, prefix)
	}
}
