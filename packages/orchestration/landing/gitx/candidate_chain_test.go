package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixedDates makes every commit of a test, the candidate's merges included,
// independent of the clock, so two candidates of one batch have one commit id.
func fixedDates(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")
}

// batchFixture is a batch of many members: clean ones, one that builds on an
// earlier member, one that renames a file another member edits, a conflicting
// one, a carrier of the conflicting one, a duplicate and one already in base.
func batchFixture(t *testing.T) (repository, string, []Member) {
	t.Helper()
	fixedDates(t)
	repo := newRepository(t)
	repo.commit("old", "old\n")
	inBase := run(t, repo.path, "rev-parse", "HEAD")
	repo.commit("shared", "one\ntwo\nthree\n")
	base := repo.commit("moved", "alpha\nbeta\ngamma\ndelta\n")
	var members []Member
	for index := 0; index < 24; index++ {
		members = append(members, Member{ID: fmt.Sprintf("clean-%02d", index), Head: repo.branch(base, fmt.Sprintf("file-%02d", index), fmt.Sprintf("%d\n", index))})
	}
	edit := repo.branch(base, "shared", "one\nTWO\nthree\n")
	members = append(members, Member{ID: "edit", Head: edit})
	stacked := repo.branch(members[3].Head, "stacked", "on clean-03\n")
	members = append(members, Member{ID: "stacked", Head: stacked})
	run(t, repo.path, "checkout", "-q", "--detach", base)
	run(t, repo.path, "mv", "moved", "renamed")
	run(t, repo.path, "commit", "-qm", "rename")
	members = append(members, Member{ID: "rename", Head: run(t, repo.path, "rev-parse", "HEAD")})
	members = append(members, Member{ID: "edit-moved", Head: repo.branch(base, "moved", "alpha\nBETA\ngamma\ndelta\n")})
	conflict := repo.branch(base, "shared", "one\nzwei\nthree\n")
	members = append(members, Member{ID: "conflict", Head: conflict})
	members = append(members, Member{ID: "carrier", Head: repo.branch(conflict, "carried", "carried\n")})
	members = append(members, Member{ID: "duplicate", Head: members[5].Head})
	members = append(members, Member{ID: "in-base", Head: inBase})
	for index := 24; index < 32; index++ {
		members = append(members, Member{ID: fmt.Sprintf("clean-%02d", index), Head: repo.branch(base, fmt.Sprintf("file-%02d", index), fmt.Sprintf("%d\n", index))})
	}
	return repo, base, members
}

// The candidate is byte-identical to the one the one-process-per-member
// algorithm formed: the same tree, the same merge commits and the same drops,
// for a whole batch and for a rebuild without a member.
func TestCandidateMatchesOneProcessPerMember(t *testing.T) {
	repo, base, members := batchFixture(t)
	ctx := context.Background()

	want, err := referenceCandidate(ctx, repo.path, base, members, nil)
	if err != nil {
		t.Fatalf("referenceCandidate() error = %v", err)
	}
	wantDrops := []Drop{{ID: "conflict", Reason: "conflict"}, {ID: "carrier", Reason: "carrier"}, {ID: "duplicate", Reason: "duplicate"}, {ID: "in-base", Reason: "contained"}}
	if !sameDrops(want.Dropped, wantDrops) {
		t.Fatalf("reference dropped = %#v, want %#v", want.Dropped, wantDrops)
	}
	if got := repo.contents(want.Commit, "renamed"); got != "alpha\nBETA\ngamma\ndelta\n" {
		t.Fatalf("reference renamed = %q, want the rename and the edit together", got)
	}
	got, err := Candidate(ctx, repo.path, base, members)
	if err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	sameCandidate(t, "Candidate", got, want)

	without := map[string]struct{}{"clean-03": {}, "edit": {}}
	wantRebuild, err := referenceRebuild(ctx, repo.path, base, members, without)
	if err != nil {
		t.Fatalf("referenceRebuild() error = %v", err)
	}
	if len(wantRebuild.Dropped) == 0 || wantRebuild.Dropped[0] != (Drop{ID: "stacked", Reason: "carrier"}) {
		t.Fatalf("reference rebuild dropped = %#v, want stacked first as the carrier of clean-03", wantRebuild.Dropped)
	}
	gotRebuild, err := Rebuild(ctx, repo.path, base, members, without)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	sameCandidate(t, "Rebuild", gotRebuild, wantRebuild)
}

// A rebuild whose excluded head is already in base drops every member that
// descends from it, as the per-member ancestor check did: with the head in
// base's history, that is every member.
func TestRebuildFindsCarrierOfExcludedHeadInBase(t *testing.T) {
	fixedDates(t)
	repo := newRepository(t)
	repo.commit("value", "base\n")
	old := run(t, repo.path, "rev-parse", "HEAD")
	base := repo.commit("next", "next\n")
	members := []Member{
		{ID: "old", Head: old},
		{ID: "carrier", Head: repo.branch(old, "other", "other\n")},
		{ID: "clean", Head: repo.branch(base, "clean", "clean\n")},
	}
	without := map[string]struct{}{"old": {}}
	want, err := referenceRebuild(context.Background(), repo.path, base, members, without)
	if err != nil {
		t.Fatalf("referenceRebuild() error = %v", err)
	}
	if !sameDrops(want.Dropped, []Drop{{ID: "carrier", Reason: "carrier"}, {ID: "clean", Reason: "carrier"}}) {
		t.Fatalf("reference dropped = %#v", want.Dropped)
	}
	got, err := Rebuild(context.Background(), repo.path, base, members, without)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	sameCandidate(t, "Rebuild", got, want)
}

// The chain merges the members in the batch's order: the second parents along
// the candidate's first-parent chain are the merged heads in that order, and of
// two members that conflict the later one is the one dropped.
func TestCandidateMergesMembersInOrder(t *testing.T) {
	fixedDates(t)
	repo := newRepository(t)
	base := repo.commit("value", "base\n")
	var members []Member
	for index := 0; index < 12; index++ {
		members = append(members, Member{ID: fmt.Sprintf("m%02d", index), Head: repo.branch(base, fmt.Sprintf("f%02d", index), "x\n")})
	}
	reversed := make([]Member, len(members))
	for index, member := range members {
		reversed[len(members)-1-index] = member
	}
	for _, order := range [][]Member{members, reversed} {
		result, err := Candidate(context.Background(), repo.path, base, order)
		if err != nil {
			t.Fatalf("Candidate() error = %v", err)
		}
		var merged []string
		for commit := result.Commit; commit != base; commit = run(t, repo.path, "rev-parse", commit+"^1") {
			merged = append([]string{run(t, repo.path, "rev-parse", commit+"^2")}, merged...)
		}
		var want []string
		for _, member := range order {
			want = append(want, member.Head)
		}
		if !sameStrings(merged, want) {
			t.Fatalf("merged heads = %q, want the members' order %q", merged, want)
		}
	}

	first := repo.branch(base, "value", "first\n")
	second := repo.branch(base, "value", "second\n")
	for _, test := range []struct {
		order   []Member
		dropped string
	}{
		{[]Member{{ID: "first", Head: first}, {ID: "second", Head: second}}, "second"},
		{[]Member{{ID: "second", Head: second}, {ID: "first", Head: first}}, "first"},
	} {
		result, err := Candidate(context.Background(), repo.path, base, test.order)
		if err != nil {
			t.Fatalf("Candidate() error = %v", err)
		}
		if want := []Drop{{ID: test.dropped, Reason: "conflict"}}; !sameDrops(result.Dropped, want) {
			t.Fatalf("Candidate(%s first) dropped = %#v, want %#v", test.order[0].ID, result.Dropped, want)
		}
	}
}

// A candidate starts a fixed number of git processes, not some per member:
// one to resolve the heads, one for every ancestor question, and, where git
// answers merge-tree --stdin line by line, one for every merge.
func TestCandidateStartsGitProcessesPerBatchNotPerMember(t *testing.T) {
	repo, base, members := batchFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "git.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := Candidate(context.Background(), repo.path, base, members); err != nil {
		t.Fatalf("Candidate() error = %v", err)
	}
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	count := func(fragment string) int {
		return strings.Count(string(body), fragment)
	}
	if got := count("merge-base --is-ancestor"); got != 0 {
		t.Errorf("git merge-base --is-ancestor ran %d times, want 0: one rev-list answers every ancestor question", got)
	}
	if got := count("rev-list"); got != 1 {
		t.Errorf("git rev-list ran %d times, want 1", got)
	}
	if got := count("rev-parse --verify"); got != 1 {
		t.Errorf("git rev-parse --verify ran %d times, want 1 (the base): the heads resolve in one cat-file --batch-check", got)
	}
	streams, err := mergeTreeStreams()
	if err != nil {
		t.Fatal(err)
	}
	if !streams {
		t.Logf("this git answers merge-tree --stdin only at end of input; the merges run one process each")
		return
	}
	if got, stdin := count("merge-tree"), count("merge-tree --stdin"); got != 1 || stdin != 1 {
		t.Errorf("git merge-tree ran %d times (%d with --stdin), want one --stdin process for the whole chain", got, stdin)
	}
}

// Both ways of merging give one candidate: the --stdin process and one
// process per merge, which a git before 2.49 needs.
func TestCandidateMergersAgree(t *testing.T) {
	repo, base, members := batchFixture(t)
	ctx := context.Background()
	want, err := referenceCandidate(ctx, repo.path, base, members, nil)
	if err != nil {
		t.Fatalf("referenceCandidate() error = %v", err)
	}
	got, err := chain(ctx, repo.path, base, members, nil, processMerges)
	if err != nil {
		t.Fatalf("chain(processMerges) error = %v", err)
	}
	sameCandidate(t, "one process per merge", got, want)
	streams, err := mergeTreeStreams()
	if err != nil {
		t.Fatal(err)
	}
	if !streams {
		t.Logf("this git answers merge-tree --stdin only at end of input; the --stdin merger is not run")
		return
	}
	got, err = chain(ctx, repo.path, base, members, nil, streamMerges)
	if err != nil {
		t.Fatalf("chain(streamMerges) error = %v", err)
	}
	sameCandidate(t, "one --stdin process", got, want)
}

func TestParseGitVersion(t *testing.T) {
	for _, test := range []struct {
		output       string
		major, minor int
	}{
		{"git version 2.53.0", 2, 53},
		{"git version 2.39.3 (Apple Git-146)", 2, 39},
		{"git version 2.45.1.windows.1", 2, 45},
	} {
		major, minor, err := parseGitVersion(test.output)
		if err != nil || major != test.major || minor != test.minor {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v; want %d, %d", test.output, major, minor, err, test.major, test.minor)
		}
	}
	if _, _, err := parseGitVersion("not git"); err == nil {
		t.Errorf("parseGitVersion(not git) error = nil, want one")
	}
}

func sameCandidate(t *testing.T, name string, got, want Result) {
	t.Helper()
	if got.Tree != want.Tree || got.Commit != want.Commit || !sameDrops(got.Dropped, want.Dropped) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
}

// referenceCandidate is the candidate algorithm as it was before the merges
// shared one process: per member, one rev-parse, one is-ancestor check per
// rejected head and one against base, one merge-tree and one commit-tree. It
// is the oracle the faster algorithm must equal.
func referenceCandidate(ctx context.Context, repo, base string, members []Member, excludedHeads []string) (Result, error) {
	base, err := revision(ctx, repo, base)
	if err != nil {
		return Result{}, fmt.Errorf("resolve base: %w", err)
	}
	tree, err := git(ctx, repo, "rev-parse", base+"^{tree}")
	if err != nil {
		return Result{}, fmt.Errorf("read base tree: %w", err)
	}
	result := Result{Tree: tree, Commit: base}
	seen := make(map[string]struct{}, len(members))
	rejected := append([]string(nil), excludedHeads...)
	for _, member := range members {
		head, err := revision(ctx, repo, member.Head)
		if err != nil {
			return Result{}, fmt.Errorf("resolve member %q: %w", member.ID, err)
		}
		if _, ok := seen[head]; ok {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "duplicate"})
			continue
		}
		seen[head] = struct{}{}
		carrier := false
		for _, rejectedHead := range rejected {
			if contained, err := ancestor(ctx, repo, rejectedHead, head); err == nil && contained {
				carrier = true
				break
			}
		}
		if carrier {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "carrier"})
			continue
		}
		contained, err := ancestor(ctx, repo, head, base)
		if err != nil {
			return Result{}, fmt.Errorf("check member %q against base: %w", member.ID, err)
		}
		if contained {
			result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "contained"})
			continue
		}
		mergedTree, err := git(ctx, repo, "merge-tree", "--write-tree", "--no-messages", result.Commit, head)
		if err != nil {
			if exitCode(err) == 1 {
				result.Dropped = append(result.Dropped, Drop{ID: member.ID, Reason: "conflict"})
				rejected = append(rejected, head)
				continue
			}
			return Result{}, fmt.Errorf("merge member %q: %w", member.ID, err)
		}
		commit, err := gitAs(ctx, repo, candidateIdentity, "commit-tree", mergedTree, "-p", result.Commit, "-p", head, "-m", "candidate merge")
		if err != nil {
			return Result{}, fmt.Errorf("commit member %q: %w", member.ID, err)
		}
		result.Tree = mergedTree
		result.Commit = commit
	}
	return result, nil
}

// referenceRebuild is Rebuild over referenceCandidate.
func referenceRebuild(ctx context.Context, repo, base string, members []Member, without map[string]struct{}) (Result, error) {
	var kept []Member
	var excludedHeads []string
	for _, member := range members {
		if _, excluded := without[member.ID]; excluded {
			head, err := revision(ctx, repo, member.Head)
			if err != nil {
				return Result{}, err
			}
			excludedHeads = append(excludedHeads, head)
			continue
		}
		kept = append(kept, member)
	}
	return referenceCandidate(ctx, repo, base, kept, excludedHeads)
}

// ancestor is the per-pair ancestor check of the reference algorithm.
func ancestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant)
	if err := command.Run(); err != nil {
		if exitCode(err) == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	return true, nil
}
