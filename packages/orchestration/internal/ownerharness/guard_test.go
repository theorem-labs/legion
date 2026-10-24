package ownerharness

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const importPath = "github.com/sjawhar/legion/orchestration/internal/ownerharness"

// The harness is test support only: no command or library of the module builds it in, so nothing that ships can
// run it in place of the daemon, omp or Envoy. Only _test.go files outside this package import it.
func TestOnlyTestFilesImportTheHarness(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == here {
			return nil
		}
		parsed, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			if value, _ := strconv.Unquote(spec.Path.Value); value == importPath {
				t.Errorf("%s imports the test-only harness", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(here, "cmd")); !os.IsNotExist(err) {
		t.Errorf("the harness has a command directory, which builds it as an executable: %v", err)
	}
}

// Asked for its version, each stand-in says what it is, so a report of a run against it never passes for a run
// against the daemon or omp.
func TestStandInsNameThemselvesAsTestOnly(t *testing.T) {
	for _, call := range []struct {
		name string
		args []string
	}{{"legion", []string{"version"}}, {"omp", []string{"--version"}}} {
		var out bytes.Buffer
		if code := Main(call.name, call.args, &out); code != 0 || !strings.Contains(out.String(), "test-only stand-in") {
			t.Errorf("%s %v = exit %d, %q; want a test-only stand-in label", call.name, call.args, code, out.String())
		}
	}
}
