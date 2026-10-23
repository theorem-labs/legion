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
