package declindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config.go documents an empty lean.cc as "LEAN_CC, else the toolchain's clang, else cc", so BuildExporter must give
// Lake the caller's LEAN_CC: with only the toolchain environment, Lake compiles with `cc`, which a machine with a
// toolchain built from source (no bundled clang) and no `cc` lacks. The fake toolchain's lake records the
// environment it receives.
func TestBuildExporterGivesLakeTheEnvironmentsLEAN_CC(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "lake-env")
	lake := "#!/bin/sh\nenv > '" + record + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "lake"), []byte(lake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEAN_CC", "/opt/toolchain-cc/bin/clang")
	_, _ = BuildExporter(context.Background(), Lean{Prefix: prefix}, "", t.TempDir())
	env, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("lake did not run: %v", err)
	}
	if !strings.Contains("\n"+string(env), "\nLEAN_CC=/opt/toolchain-cc/bin/clang\n") {
		t.Fatalf("lake ran without the environment's LEAN_CC (config.go: empty cc is LEAN_CC); its environment:\n%s", env)
	}
}
