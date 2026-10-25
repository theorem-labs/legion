package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateDropsConflicts(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	first := repo.branch(base, "value", "first\n")
	second := repo.branch(base, "value", "second\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{
		{ID: "one", Head: first},
		{ID: "two", Head: second},
	})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "two", Reason: "conflict"}}; !sameDrops(got, want) {
		t.Fatalf("Candidate() dropped = %#v, want %#v", got, want)
	}
	if got, want := repo.contents(result.Commit, "value"), "first\n"; got != want {
		t.Fatalf("candidate contents = %q, want %q", got, want)
	}
}

// A candidate commit is the program's own object: it builds in a repository
// and a home with no git identity. user.useConfigOnly makes git refuse the
// identity it would otherwise guess from the host name, as it does on a host
// whose name has no domain.
func TestCandidateNeedsNoGitIdentity(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	first := repo.branch(base, "other", "first\n")
	run(t, repo.path, "config", "--unset", "user.name")
	run(t, repo.path, "config", "--unset", "user.email")
	run(t, repo.path, "config", "user.useConfigOnly", "true")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	result, err := Candidate(context.Background(), repo.path, base, []Member{{ID: "one", Head: first}})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got := repo.contents(result.Commit, "other"); got != "first\n" {
		t.Fatalf("candidate contents = %q, want the member's file", got)
	}
}

func TestCandidateDropsCarrierOfRejectedMember(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	accepted := repo.branch(base, "value", "accepted\n")
	rejected := repo.branch(base, "value", "rejected\n")
	carrier := repo.branch(rejected, "other", "carried\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{
		{ID: "accepted", Head: accepted},
		{ID: "rejected", Head: rejected},
		{ID: "carrier", Head: carrier},
	})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "rejected", Reason: "conflict"}, {ID: "carrier", Reason: "carrier"}}; !sameDrops(got, want) {
		t.Fatalf("Candidate() dropped = %#v, want %#v", got, want)
	}
	if got, want := repo.contents(result.Commit, "other"), ""; got != want {
		t.Fatalf("candidate carrier contents = %q, want no file", got)
	}
}

func TestCandidateSkipsMemberAlreadyInBase(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{{ID: "contained", Head: base}})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "contained", Reason: "contained"}}; !sameDrops(got, want) {
		t.Fatalf("Candidate() dropped = %#v, want %#v", got, want)
	}
	if got, want := result.Tree, repo.tree(base); got != want {
		t.Fatalf("Candidate() tree = %q, want %q", got, want)
	}
}

func TestCandidateDropsDuplicatedMember(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	head := repo.branch(base, "next", "next\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{
		{ID: "once", Head: head},
		{ID: "again", Head: head},
	})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "again", Reason: "duplicate"}}; !sameDrops(got, want) {
		t.Fatalf("Candidate() dropped = %#v, want %#v", got, want)
	}
}

func TestChangeSetUsesMergeBase(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	member := repo.branch(base, "added", "member\n")

	set, err := ChangeSet(context.Background(), repo.path, base, Member{ID: "member", Head: member})
	if err != nil {
		t.Fatalf("ChangeSet() error = %v", err)
	}
	if got, want := set.Files, []string{"added"}; !sameStrings(got, want) {
		t.Fatalf("ChangeSet() files = %#v, want %#v", got, want)
	}
	if got, want := set.Ranges, []Range{{Base: base, Head: member}}; !sameRanges(got, want) {
		t.Fatalf("ChangeSet() ranges = %#v, want %#v", got, want)
	}
}

// A member that moves a file changes both paths, and a path with non-ASCII
// bytes is listed as it is, not quoted.
func TestChangeSetListsMovedAndNonASCIIPaths(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("Old.lean", "def kept := 0\ndef kept1 := 1\ndef kept2 := 2\ndef kept3 := 3\n")
	run(t, repo.path, "mv", "Old.lean", "New.lean")
	member := repo.commit("Alpha é.lean", "def added := 0\n")

	set, err := ChangeSet(context.Background(), repo.path, base, Member{ID: "member", Head: member})
	if err != nil {
		t.Fatalf("ChangeSet() error = %v", err)
	}
	if got, want := set.Files, []string{"Alpha é.lean", "New.lean", "Old.lean"}; !sameStrings(got, want) {
		t.Fatalf("ChangeSet() files = %#v, want %#v", got, want)
	}
}

func TestDossierCase1KeepsEachMemberChangeSet(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	first := repo.branch(base, "first", "one\n")
	second := repo.branch(base, "second", "two\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{{ID: "first", Head: first}, {ID: "second", Head: second}})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Fatalf("Candidate() dropped = %#v, want none", result.Dropped)
	}
	if got, want := repo.contents(result.Commit, "first"), "one\n"; got != want {
		t.Fatalf("first contents = %q, want %q", got, want)
	}
	if got, want := repo.contents(result.Commit, "second"), "two\n"; got != want {
		t.Fatalf("second contents = %q, want %q", got, want)
	}
}

func TestDossierCase5UsesMemberHeadsRatherThanStagingMergeParents(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	first := repo.branch(base, "first", "one\n")
	movedBase := repo.branch(base, "upstream", "new\n")
	member := repo.branch(movedBase, "member", "member\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{{ID: "first", Head: first}, {ID: "member", Head: member}})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Fatalf("Candidate() dropped = %#v, want none", result.Dropped)
	}
	if got, want := repo.contents(result.Commit, "member"), "member\n"; got != want {
		t.Fatalf("member contents = %q, want %q", got, want)
	}
}

func TestDossierCase6RebuildsFromBaseWithoutDroppedMember(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	keep := repo.branch(base, "keep", "keep\n")
	drop := repo.branch(base, "drop", "drop\n")

	result, err := Rebuild(context.Background(), repo.path, base, []Member{{ID: "keep", Head: keep}, {ID: "drop", Head: drop}}, map[string]struct{}{"drop": struct{}{}})
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got, want := repo.contents(result.Commit, "keep"), "keep\n"; got != want {
		t.Fatalf("keep contents = %q, want %q", got, want)
	}
	if got, want := repo.contents(result.Commit, "drop"), ""; got != want {
		t.Fatalf("drop contents = %q, want no file", got)
	}
}

func TestDossierCase7DropsDuplicatedHeadWithoutBroadAttribution(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	head := repo.branch(base, "member", "member\n")

	result, err := Candidate(context.Background(), repo.path, base, []Member{{ID: "one", Head: head}, {ID: "two", Head: head}})
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	if got, want := result.Dropped, []Drop{{ID: "two", Reason: "duplicate"}}; !sameDrops(got, want) {
		t.Fatalf("Candidate() dropped = %#v, want %#v", got, want)
	}
}

func TestDossierCase18ComparesAgainstCurrentBase(t *testing.T) {
	repo := newRepository(t)
	base := repo.commit("base", "base\n")
	current := repo.commit("upstream", "new\n")
	member := repo.branch(current, "member", "member\n")

	change, err := ChangeSet(context.Background(), repo.path, current, Member{ID: "member", Head: member})
	if err != nil {
		t.Fatalf("ChangeSet() error = %v", err)
	}
	if got, want := change.Files, []string{"member"}; !sameStrings(got, want) {
		t.Fatalf("ChangeSet() files = %#v, want %#v (initial base %q)", got, want, base)
	}
}

type repository struct {
	t    *testing.T
	path string
}

func newRepository(t *testing.T) repository {
	t.Helper()
	path := t.TempDir()
	run(t, path, "init", "-q")
	run(t, path, "config", "user.email", "test@example.invalid")
	run(t, path, "config", "user.name", "Test")
	return repository{t: t, path: path}
}

func (r repository) commit(file, contents string) string {
	r.t.Helper()
	path := filepath.Join(r.path, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, r.path, "add", file)
	run(r.t, r.path, "commit", "-qm", "change")
	return run(r.t, r.path, "rev-parse", "HEAD")
}

func (r repository) branch(base, file, contents string) string {
	r.t.Helper()
	run(r.t, r.path, "checkout", "-q", "--detach", base)
	return r.commit(file, contents)
}

func (r repository) tree(commit string) string {
	r.t.Helper()
	return run(r.t, r.path, "rev-parse", commit+"^{tree}")
}

func (r repository) contents(commit, file string) string {
	r.t.Helper()
	out, err := exec.Command("git", "-C", r.path, "show", commit+":"+file).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func sameDrops(got, want []Drop) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameRanges(got, want []Range) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
