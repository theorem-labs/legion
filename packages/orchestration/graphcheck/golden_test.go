package graphcheck_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
)

// TestFixtureBuildMatchesGolden builds the fixture with the lake on PATH and compares every reference file byte
// for byte with the committed golden/ that the other tests index. It skips without lake, and fails without it
// when GRAPHCHECK_REQUIRE_LEAN is set, as in the orchestration-lean workflow.
func TestFixtureBuildMatchesGolden(t *testing.T) {
	if _, err := exec.LookPath("lake"); err != nil {
		if os.Getenv("GRAPHCHECK_REQUIRE_LEAN") != "" {
			t.Fatal("GRAPHCHECK_REQUIRE_LEAN is set and no lake is on PATH")
		}
		t.Skip("no lake on PATH: the golden test builds the fixture with Lean")
	}
	dir := graphchecktest.Testdata(t)
	out := filepath.Join(t.TempDir(), "golden")
	cmd := exec.Command("bash", filepath.Join(dir, "build.sh"), out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build.sh: %v\n%s", err, output)
	}
	want := files(t, filepath.Join(dir, "golden"))
	got := files(t, out)
	for rel, raw := range want {
		if !bytes.Equal(got[rel], raw) {
			t.Errorf("%s differs from golden/%s: rebuild golden/ with build.sh", rel, rel)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("the build wrote %s, which golden/ lacks", rel)
		}
	}
}

func files(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		out[rel] = raw
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
