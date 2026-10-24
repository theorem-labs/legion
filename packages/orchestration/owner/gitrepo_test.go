package owner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestRepoWorktreeCreatesReusesAndDisablesAutoGC(t *testing.T) {
	repo := newTestRepo(t)
	dir := filepath.Join(t.TempDir(), "owner")

	first, err := repo.Worktree(context.Background(), dir, "owner/one", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Worktree(context.Background(), dir, "owner/one", "main")
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != dir || second.Path != dir {
		t.Fatalf("worktree paths = %q, %q; want %q", first.Path, second.Path, dir)
	}
	if got := gitOutput(t, repo.Path, nil, "config", "--get", "gc.auto"); got != "0" {
		t.Fatalf("gc.auto = %q, want 0", got)
	}
}

func TestRepoWorktreeCreatesAndReusesInParallel(t *testing.T) {
	repo := newTestRepo(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, branch := range []string{"owner/one", "owner/two"} {
		wait.Add(1)
		go func(branch string) {
			defer wait.Done()
			<-start
			dir := filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-"))
			first, err := repo.Worktree(context.Background(), dir, branch, "main")
			if err != nil {
				results <- err
				return
			}
			second, err := repo.Worktree(context.Background(), dir, branch, "main")
			if err == nil && (first.Path != dir || second.Path != dir) {
				err = fmt.Errorf("worktree paths = %q, %q; want %q", first.Path, second.Path, dir)
			}
			results <- err
		}(branch)
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorktreesCommitInParallel(t *testing.T) {
	repo := newTestRepo(t)
	one := testWorktree(t, repo, "owner/one")
	two := testWorktree(t, repo, "owner/two")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, worktree := range []Worktree{one, two} {
		wait.Add(1)
		go func(worktree Worktree) {
			defer wait.Done()
			<-start
			_, err := worktree.CommitFile(context.Background(), "records/parallel.txt", []byte(worktree.Branch), "owner: parallel record", map[string][]string{"Owner-Item": {"need:parallel@r1"}})
			errs <- err
		}(worktree)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorktreeCommitFileIsIdempotentAndKeepsTrailers(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/idempotent")
	trailers := map[string][]string{"Owner-Item": {"need:function:a->struct:b@r1"}, "Owner": {"function:a"}}

	first, err := worktree.CommitFile(context.Background(), "records/need.yaml", []byte("need: first\n"), "owner: record need", trailers)
	if err != nil {
		t.Fatal(err)
	}
	second, err := worktree.CommitFile(context.Background(), "records/need.yaml", []byte("need: first\n"), "owner: record need", trailers)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second CommitFile = %s, want existing commit %s", second, first)
	}
	if got := gitOutput(t, repo.Path, nil, "rev-list", "--count", "main.."+worktree.Branch); got != "1" {
		t.Fatalf("commits ahead of main = %s, want 1", got)
	}
	got, found, err := worktree.BlobAtHead("records/need.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !found || string(got) != "need: first\n" {
		t.Fatalf("BlobAtHead = %q, %t; want %q, true", got, found, "need: first\n")
	}
	if _, found, err := worktree.BlobAtHead("records/missing.yaml"); err != nil || found {
		t.Fatalf("BlobAtHead missing = found %t, err %v; want false, nil", found, err)
	}
	if got, err := worktree.LastCommitTouching("records/need.yaml"); err != nil || got != first {
		t.Fatalf("LastCommitTouching = %q, %v; want %q, nil", got, err, first)
	}
	gotTrailers, err := repo.CommitTrailers(first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotTrailers, trailers) {
		t.Fatalf("CommitTrailers = %#v, want %#v", gotTrailers, trailers)
	}
}

// A record that answers several items carries one Owner-Item trailer for each, in the order given.
func TestCommitFileRecordsEveryValueOfARepeatedTrailer(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/several")
	items := []string{"need:function:a->struct:b@r1", "need:function:c->struct:b@r2", "start:struct:b@1"}

	commit, err := worktree.CommitFile(context.Background(), "records/spec.md", []byte("spec: r1\n"), "owner: spec r1", map[string][]string{"Owner-Item": items})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.CommitTrailers(commit)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string][]string{"Owner-Item": items}) {
		t.Fatalf("CommitTrailers = %#v, want Owner-Item %q", got, items)
	}
	if values := gitOutput(t, repo.Path, nil, "log", "-1", "--format=%(trailers:key=Owner-Item,valueonly)", commit); values != strings.Join(items, "\n") {
		t.Fatalf("git reads Owner-Item values %q, want %q", values, items)
	}
}

func TestRepoShowReadsFileFromTree(t *testing.T) {
	repo := newTestRepo(t)
	blob := gitOutput(t, repo.Path, []byte("from a tree\n"), "hash-object", "-w", "--stdin")
	tree := gitOutput(t, repo.Path, []byte(fmt.Sprintf("100644 blob %s\tfrom-tree.txt\n", blob)), "mktree")

	got, err := repo.Show(context.Background(), tree, "from-tree.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from a tree\n" {
		t.Fatalf("Show = %q, want %q", got, "from a tree\n")
	}
}

func TestRepoMergeTreeReportsConflictsAndCleanMerge(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		repo := newTestRepo(t)
		ours := testWorktree(t, repo, "owner/ours")
		theirs := testWorktree(t, repo, "owner/theirs")
		oursSHA, err := ours.CommitFile(context.Background(), "shared.txt", []byte("ours\n"), "owner: ours", nil)
		if err != nil {
			t.Fatal(err)
		}
		theirsSHA, err := theirs.CommitFile(context.Background(), "shared.txt", []byte("theirs\n"), "owner: theirs", nil)
		if err != nil {
			t.Fatal(err)
		}

		tree, conflicts, err := repo.MergeTree(context.Background(), oursSHA, theirsSHA)
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) != 40 {
			t.Fatalf("MergeTree tree = %q, want object id", tree)
		}
		if !reflect.DeepEqual(conflicts, []string{"shared.txt"}) {
			t.Fatalf("MergeTree conflicts = %#v, want []string{\"shared.txt\"}", conflicts)
		}
	})

	t.Run("clean", func(t *testing.T) {
		repo := newTestRepo(t)
		ours := testWorktree(t, repo, "owner/ours")
		theirs := testWorktree(t, repo, "owner/theirs")
		oursSHA, err := ours.CommitFile(context.Background(), "ours.txt", []byte("ours\n"), "owner: ours", nil)
		if err != nil {
			t.Fatal(err)
		}
		theirsSHA, err := theirs.CommitFile(context.Background(), "theirs.txt", []byte("theirs\n"), "owner: theirs", nil)
		if err != nil {
			t.Fatal(err)
		}

		tree, conflicts, err := repo.MergeTree(context.Background(), oursSHA, theirsSHA)
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) != 40 {
			t.Fatalf("MergeTree tree = %q, want object id", tree)
		}
		if len(conflicts) != 0 {
			t.Fatalf("MergeTree conflicts = %#v, want none", conflicts)
		}
	})
}

func TestRepoCommitTreeUsesEveryParentInOrder(t *testing.T) {
	repo := newTestRepo(t)
	first := testWorktree(t, repo, "owner/first")
	second := testWorktree(t, repo, "owner/second")
	firstSHA, err := first.CommitFile(context.Background(), "first.txt", []byte("first\n"), "owner: first", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondSHA, err := second.CommitFile(context.Background(), "second.txt", []byte("second\n"), "owner: second", nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, conflicts, err := repo.MergeTree(context.Background(), firstSHA, secondSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("MergeTree conflicts = %#v, want none", conflicts)
	}
	merge, err := repo.CommitTree(context.Background(), tree, []string{firstSHA, secondSHA}, "upstream: merge")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, repo.Path, nil, "rev-parse", merge+"^1"); got != firstSHA {
		t.Fatalf("first parent = %s, want %s", got, firstSHA)
	}
	if got := gitOutput(t, repo.Path, nil, "rev-parse", merge+"^2"); got != secondSHA {
		t.Fatalf("second parent = %s, want %s", got, secondSHA)
	}
}

func TestWorktreeMergeMain(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/merge")
	if _, err := worktree.CommitFile(context.Background(), "owner.txt", []byte("owner\n"), "owner: change", nil); err != nil {
		t.Fatal(err)
	}
	main, err := repo.Worktree(context.Background(), filepath.Join(t.TempDir(), "main"), "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	mainSHA, err := main.CommitFile(context.Background(), "main.txt", []byte("main\n"), "main: change", nil)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := worktree.MergeMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, repo.Path, nil, "merge-base", "--is-ancestor", mainSHA, merged); got != "" {
		t.Fatalf("main commit %s is not an ancestor of %s", mainSHA, merged)
	}
}

func TestWorktreeMergeMainFailsOnConflict(t *testing.T) {
	repo := newTestRepo(t)
	worktree := testWorktree(t, repo, "owner/conflict")
	ownerSHA, err := worktree.CommitFile(context.Background(), "shared.txt", []byte("owner\n"), "owner: change", nil)
	if err != nil {
		t.Fatal(err)
	}
	main, err := repo.Worktree(context.Background(), filepath.Join(t.TempDir(), "main"), "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := main.CommitFile(context.Background(), "shared.txt", []byte("main\n"), "main: change", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := worktree.MergeMain(context.Background()); err == nil {
		t.Fatal("MergeMain succeeded despite a conflict")
	}
	if got := gitOutput(t, worktree.Path, nil, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree state after failed merge = %q, want clean", got)
	}
	if got := gitOutput(t, worktree.Path, nil, "rev-parse", "HEAD"); got != ownerSHA {
		t.Fatalf("worktree head after failed merge = %s, want %s", got, ownerSHA)
	}
}

func newTestRepo(t *testing.T) Repo {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	repo := Repo{Path: filepath.Join(root, "repo.git")}
	gitOutput(t, "", nil, "init", "-q", "--initial-branch=main", seed)
	gitOutput(t, seed, nil, "config", "user.name", "Owner Test")
	gitOutput(t, seed, nil, "config", "user.email", "owner-test@example.invalid")
	if err := os.WriteFile(filepath.Join(seed, "shared.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, seed, nil, "add", "shared.txt")
	gitOutput(t, seed, nil, "commit", "-qm", "seed")
	gitOutput(t, "", nil, "clone", "-q", "--bare", seed, repo.Path)
	gitOutput(t, repo.Path, nil, "config", "user.name", "Owner Test")
	gitOutput(t, repo.Path, nil, "config", "user.email", "owner-test@example.invalid")
	return repo
}

func testWorktree(t *testing.T, repo Repo, branch string) Worktree {
	t.Helper()
	worktree, err := repo.Worktree(context.Background(), filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-")), branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	return worktree
}

func gitOutput(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Stdin = strings.NewReader(string(input))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
