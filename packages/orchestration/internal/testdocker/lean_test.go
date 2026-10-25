package testdocker

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// toolchain is the prefix of the `lean` on PATH; the test skips without one, unless GRAPHCHECK_REQUIRE_LEAN or
// DECLINDEX_REQUIRE_LEAN is set, as in the orchestration-lean workflow.
func toolchain(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("lean", "--print-prefix").Output()
	if err != nil {
		if os.Getenv("GRAPHCHECK_REQUIRE_LEAN") != "" || os.Getenv("DECLINDEX_REQUIRE_LEAN") != "" {
			t.Fatalf("no lean on PATH: %v", err)
		}
		t.Skip("no lean on PATH: the Lean image test runs the host's toolchain in it")
	}
	return strings.TrimSpace(string(out))
}

// exporterScript does in the image what declindex.BuildExporter does to build the index's exporter: Lean compiles a
// module to C, `cc` compiles the C, and `cc` links the object against the toolchain's shared libraries with the
// exporter's own link flags. The program it links then runs.
const exporterScript = `set -e
/tc/bin/lean --version
cd /work
/tc/bin/lean -c main.c Main.lean
cc -c -I/tc/include -fPIC -O1 -o main.o main.c
libs=
for l in leanshared Init_shared leanshared_1 leanshared_2; do
  if [ -f "/tc/lib/lean/lib$l.so" ]; then libs="$libs -l$l"; fi
done
cc -o main main.o -rdynamic -L/tc/lib/lean -Wl,-rpath,/tc/lib/lean $libs -lm -ldl -pthread
./main
`

// The project's toolchain runs in Image, mounted read-only as a check's mount is: `lean --version` answers, and a
// Lean program compiles and links there as the index's exporter does, which a check that reads compiled constants
// needs.
func TestImageRunsTheToolchain(t *testing.T) {
	prefix := toolchain(t)
	user := Require(t)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "Main.lean"), []byte("def main : IO Unit := IO.println \"linked\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "--rm", "--network=none", "--user", user, "-v", prefix+":/tc:ro",
		"-v", work+":/work", Image, "sh", "-c", exporterScript).CombinedOutput()
	if err != nil {
		if missing := glibcMissing.FindAllString(string(out), -1); len(missing) != 0 {
			t.Fatalf("%s's C library is older than the one the toolchain at %s was built against: it needs %s. "+
				"Rebase recipe on a release with that glibc.\n%s",
				Image, prefix, strings.Join(slices.Compact(slices.Sorted(slices.Values(missing))), ", "), out)
		}
		t.Fatalf("toolchain in %s: %v\n%s", Image, err, out)
	}
	if !strings.Contains(string(out), "Lean (version") || !strings.HasSuffix(strings.TrimSpace(string(out)), "linked") {
		t.Fatalf("toolchain in %s printed %q, want Lean's version and the linked program's line", Image, out)
	}
}

// glibcMissing matches the C library versions a binary needs that the loader did not find.
var glibcMissing = regexp.MustCompile(`GLIBC(XX)?_[0-9.]+`)
