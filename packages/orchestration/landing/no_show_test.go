package landing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// TestNoGitShowInEvidenceOrTreePath puts a wrapper git on PATH that rejects
// `git show` and any `git cat-file` without --batch, the per-file reads. It
// creates a repository where two members share one unchanged blob
// (Shared.lean) and each changes a different file. DeclarationEvidence and
// TreeDeclarations must succeed through the wrapper.
func TestNoGitShowInEvidenceOrTreePath(t *testing.T) {
	// Find the real git binary.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("cannot find git: %v", err)
	}
	// Write a wrapper that rejects show and cat-file without --batch.
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "git")
	script := `#!/bin/sh
show=no
catfile=no
batch=no
for arg in "$@"; do
  case "$arg" in
    show) show=yes ;;
    cat-file) catfile=yes ;;
    --batch) batch=yes ;;
  esac
done
if [ "$show" = yes ]; then
  echo "FATAL: git show is forbidden" >&2
  exit 1
fi
if [ "$catfile" = yes ] && [ "$batch" = no ]; then
  echo "FATAL: git cat-file without --batch is forbidden" >&2
  exit 1
fi
exec ` + realGit + ` "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	// Build a repository with a shared blob and two members.
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(realGit, append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	writeSource(t, repo, "Shared.lean", "def shared := 0\n")
	writeSource(t, repo, "Alpha.lean", "def alpha := 0\n")
	writeSource(t, repo, "Beta.lean", "def beta := 0\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	// Member A changes Alpha, keeps Shared.
	git("checkout", "-q", "-b", "member-a", base)
	writeSource(t, repo, "Alpha.lean", "def alpha := 1\n")
	git("commit", "-qam", "a")
	headA := git("rev-parse", "HEAD")

	// Member B changes Beta, keeps Shared.
	git("checkout", "-q", "-b", "member-b", base)
	writeSource(t, repo, "Beta.lean", "def beta := 1\n")
	git("commit", "-qam", "b")
	headB := git("rev-parse", "HEAD")

	ctx := context.Background()
	// The wrapper refuses the per-file reads it guards against.
	for _, args := range [][]string{{"show", base + ":Shared.lean"}, {"cat-file", "-p", base + ":Shared.lean"}, {"cat-file", "blob", base + ":Shared.lean"}} {
		if output, err := exec.Command(wrapper, append([]string{"-C", repo}, args...)...).CombinedOutput(); err == nil {
			t.Fatalf("wrapper ran git %s, want it refused:\n%s", strings.Join(args, " "), output)
		}
	}

	g := RepositoryGit{}

	// DeclarationEvidence must work through the wrapper (no git show).
	evidenceA, err := g.DeclarationEvidence(ctx, repo, base, Member{Migration: "a", Head: headA})
	if err != nil {
		t.Fatalf("DeclarationEvidence(a) error = %v", err)
	}
	if s, l := blame.EvidenceFor(blame.Name{Value: "alpha", Kind: blame.Changed}, evidenceA); s != 3 || l != "signature changed" {
		t.Fatalf("alpha evidence = (%d, %q), want (3, signature changed)", s, l)
	}
	evidenceB, err := g.DeclarationEvidence(ctx, repo, base, Member{Migration: "b", Head: headB})
	if err != nil {
		t.Fatalf("DeclarationEvidence(b) error = %v", err)
	}
	if s, l := blame.EvidenceFor(blame.Name{Value: "beta", Kind: blame.Changed}, evidenceB); s != 3 || l != "signature changed" {
		t.Fatalf("beta evidence = (%d, %q), want (3, signature changed)", s, l)
	}

	// TreeDeclarations must work through the wrapper.
	decls, err := g.TreeDeclarations(ctx, repo, Tree(base))
	if err != nil {
		t.Fatalf("TreeDeclarations() error = %v", err)
	}
	names := make(map[string]bool, len(decls))
	for _, d := range decls {
		names[d.Name] = true
	}
	for _, want := range []string{"shared", "alpha", "beta"} {
		if !names[want] {
			t.Errorf("TreeDeclarations() missing %q (got %v)", want, names)
		}
	}
}
