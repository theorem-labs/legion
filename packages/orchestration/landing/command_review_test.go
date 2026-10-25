package landing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCommandBuilderClassifiesKilledProcessAsInfrastructure(t *testing.T) {
	repo, commit := commandRepository(t, false)
	builder := CommandBuilder{RepositoryPath: repo, Command: []string{"sh", "-c", "kill -9 $$"}}

	result, err := builder.Build(context.Background(), Tree(commit), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if result.InfraClass == "" {
		t.Fatalf("Build() = %#v, want infrastructure class", result)
	}
}

func TestReviewCommandBuilderClassifiesNestedSignalAsInfrastructure(t *testing.T) {
	repo, commit := commandRepository(t, false)
	builder := CommandBuilder{RepositoryPath: repo, Command: []string{"sh", "-c", "sh -c 'kill -11 $$'"}}

	result, err := builder.Build(context.Background(), Tree(commit), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if result.InfraClass == "" {
		t.Fatalf("Build() = %#v, want nested signal infrastructure class", result)
	}
}

func TestReviewCommandBuilderMaterializesSymlink(t *testing.T) {
	repo, commit := commandRepository(t, true)
	builder := CommandBuilder{RepositoryPath: repo, Command: []string{"sh", "-c", "test -L Link.lean"}}

	result, err := builder.Build(context.Background(), Tree(commit), nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !result.Green {
		t.Fatalf("Build() = %#v, want symlink-preserving green result", result)
	}
}

func TestReviewCommandHasherRequiresEveryStructuredHash(t *testing.T) {
	hasher := CommandHasher{Command: []string{"sh", "-c", "printf 'Alpha.One\\thash-one\\tAlpha.lean\\n'"}}

	if _, err := hasher.Hashes(context.Background(), HashInput{Tree: Tree("tree"), Names: []string{"Alpha.One", "Alpha.Two"}}); err == nil {
		t.Fatal("Hashes() error = nil, want missing-name error")
	}
}

func commandRepository(t *testing.T, symlink bool) (string, string) {
	t.Helper()
	repo := t.TempDir()
	commandReviewGit(t, repo, "init", "-q")
	commandReviewGit(t, repo, "config", "user.email", "test@example.invalid")
	commandReviewGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "Target.lean"), []byte("def target := 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if symlink {
		if err := os.Symlink("Target.lean", filepath.Join(repo, "Link.lean")); err != nil {
			t.Fatal(err)
		}
	}
	commandReviewGit(t, repo, "add", ".")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	return repo, commandReviewGit(t, repo, "rev-parse", "HEAD")
}

func commandReviewGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// The names are always in LAND_NAMES_FILE, and also in LAND_NAMES only while
// the array fits in 131,072 bytes. A path of "-" is an absent name with the
// empty hash, and comma-joined paths are an ambiguous name.
func TestCommandHasherNamesAndPaths(t *testing.T) {
	repo, commit := commandRepository(t, false)
	script := `test -s "$LAND_NAMES_FILE" || exit 1
if [ -n "${LAND_NAMES+set}" ]; then echo set > "$1"; else echo unset > "$1"; fi
printf 'Absent\tx\t-\nAmbiguous\th\tA.lean,B.lean\n'`
	marker := filepath.Join(t.TempDir(), "names")
	hasher := CommandHasher{Command: []string{"sh", "-c", script, "hasher", marker}, Directory: repo}
	hashes, err := hasher.Hashes(context.Background(), HashInput{Tree: Tree(commit), Names: []string{"Absent", "Ambiguous"}})
	if err != nil {
		t.Fatalf("Hashes() error = %v", err)
	}
	if got := hashes["Absent"]; got.Hash != "" || len(got.Paths) != 0 {
		t.Fatalf("Absent = %+v, want the empty hash and no path", got)
	}
	if got := hashes["Ambiguous"]; !got.Ambiguous() {
		t.Fatalf("Ambiguous = %+v, want ambiguous", got)
	}
	if body, _ := os.ReadFile(marker); strings.TrimSpace(string(body)) != "set" {
		t.Fatalf("LAND_NAMES for a short list = %q, want set", body)
	}
	long := []string{"Absent", "Ambiguous", strings.Repeat("N", maxNamesEnvironment)}
	_, _ = hasher.Hashes(context.Background(), HashInput{Tree: Tree(commit), Names: long})
	if body, _ := os.ReadFile(marker); strings.TrimSpace(string(body)) != "unset" {
		t.Fatalf("LAND_NAMES for a long list = %q, want unset", body)
	}
}
