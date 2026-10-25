package landing

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// upstreamRepository is a repository whose root holds Base.lean, with four commits on it: the copy's main (base),
// an upstream commit that adds another file, a commit that conflicts with main, and an unrelated commit.
type upstreamRepository struct {
	path     string
	root     string
	base     string
	upstream string
	conflict string
	other    string
}

func newUpstreamRepository(t *testing.T) upstreamRepository {
	t.Helper()
	repo := upstreamRepository{path: t.TempDir()}
	gitRun(t, repo.path, "init", "-q")
	gitRun(t, repo.path, "config", "user.email", "test@example.invalid")
	gitRun(t, repo.path, "config", "user.name", "Test")
	writeSource(t, repo.path, "Base.lean", "def Base := 0\n")
	gitRun(t, repo.path, "add", "Base.lean")
	gitRun(t, repo.path, "commit", "-qm", "root")
	repo.root = gitRun(t, repo.path, "rev-parse", "HEAD")
	repo.base = repo.commit(t, repo.root, "main", "Base.lean", "def Base := 1\n")
	repo.upstream = repo.commit(t, repo.root, "upstream", "Upstream.lean", "def Upstream := 0\n")
	repo.conflict = repo.commit(t, repo.root, "conflict", "Base.lean", "def Base := 2\n")
	repo.other = repo.commit(t, repo.root, "other", "Other.lean", "def Other := 0\n")
	return repo
}

// commit writes contents to name on branch, from parent, and is the commit it makes.
func (r upstreamRepository) commit(t *testing.T, parent, branch, name, contents string) string {
	t.Helper()
	gitRun(t, r.path, "checkout", "-q", "-B", branch, parent)
	writeSource(t, r.path, name, contents)
	gitRun(t, r.path, "add", name)
	gitRun(t, r.path, "commit", "-qm", branch)
	return gitRun(t, r.path, "rev-parse", "HEAD")
}

// merged is the tree that merging left with right writes. The merge must be clean.
func (r upstreamRepository) merged(t *testing.T, left, right string) string {
	t.Helper()
	return gitRun(t, r.path, "merge-tree", "--write-tree", "--no-messages", left, right)
}

// tree is a commit's tree.
func (r upstreamRepository) tree(t *testing.T, commit string) string {
	t.Helper()
	return gitRun(t, r.path, "rev-parse", commit+"^{tree}")
}

// sync is a commit of tree with parents, in the shape an upstream sync's head has.
func (r upstreamRepository) sync(t *testing.T, tree string, parents ...string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", "upstream sync"}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	return gitRun(t, r.path, args...)
}

// upstreamRunner holds batch-1 at base with members, over the repository's own git.
func upstreamRunner(t *testing.T, repo upstreamRepository, base string, members ...Member) (*dropLedger, Runner, *Batch) {
	t.Helper()
	store := &admissionStore{admission: NotAdmitted}
	ledger := &dropLedger{refusals: map[string]error{}}
	batch := &Batch{
		ID: "batch-1", Repository: repo.path, Base: base, State: Checked,
		FrozenMembers: cloneMembers(members), ActiveMembers: cloneMembers(members),
	}
	if _, err := store.Create(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	return ledger, Runner{Store: store, Ledger: ledger, Admission: &dropBatchLedger{}, Git: RepositoryGit{}}, batch
}

// droppedReasonFor is the reason the batch recorded for a dropped member, and empty while it holds none.
func droppedReasonFor(batch *Batch, migration string) string {
	for _, dropped := range batch.Dropped {
		if dropped.Migration == migration {
			return dropped.Reason
		}
	}
	return ""
}

// A head whose first parent is the batch's base, whose second is the member's upstream commit, and whose tree is
// the tree of that merge, is the sync the ledger record claims: the member stays.
func TestUpstreamSyncOfTheBaseStaysActive(t *testing.T) {
	repo := newUpstreamRepository(t)
	head := repo.sync(t, repo.merged(t, repo.base, repo.upstream), repo.base, repo.upstream)
	ledger, runner, batch := upstreamRunner(t, repo, repo.base,
		Member{Migration: "sync", Generation: 1, Head: head, Upstream: repo.upstream})

	returned, err := runner.checkUpstream(context.Background(), batch)
	if err != nil {
		t.Fatalf("checkUpstream() error = %v", err)
	}
	if returned || len(batch.ActiveMembers) != 1 || len(ledger.returned) != 0 {
		t.Fatalf("a matching upstream sync was returned: returned=%v active=%+v ledger=%v", returned, batch.ActiveMembers, ledger.returned)
	}
}

// Every head that is not the merge the ledger's upstream record claims returns the member as upstream_mismatch.
func TestUpstreamHeadThatIsNotThatMergeIsReturned(t *testing.T) {
	repo := newUpstreamRepository(t)
	for _, test := range []struct {
		name string
		// head is the member's head and the upstream commit its ready record names.
		head func(t *testing.T, repo upstreamRepository) (string, string)
	}{
		{
			name: "the head is not a merge",
			head: func(_ *testing.T, repo upstreamRepository) (string, string) {
				return repo.other, repo.upstream
			},
		},
		{
			name: "the head has three parents",
			head: func(t *testing.T, repo upstreamRepository) (string, string) {
				return repo.sync(t, repo.merged(t, repo.base, repo.upstream), repo.base, repo.upstream, repo.other), repo.upstream
			},
		},
		{
			name: "the first parent is neither the base nor behind it",
			head: func(t *testing.T, repo upstreamRepository) (string, string) {
				return repo.sync(t, repo.merged(t, repo.other, repo.upstream), repo.other, repo.upstream), repo.upstream
			},
		},
		{
			name: "the second parent is not the upstream commit",
			head: func(t *testing.T, repo upstreamRepository) (string, string) {
				return repo.sync(t, repo.merged(t, repo.base, repo.other), repo.base, repo.other), repo.upstream
			},
		},
		{
			name: "the merge of the parents conflicts",
			head: func(t *testing.T, repo upstreamRepository) (string, string) {
				return repo.sync(t, repo.tree(t, repo.base), repo.base, repo.conflict), repo.conflict
			},
		},
		{
			name: "the tree is not the merge's tree",
			head: func(t *testing.T, repo upstreamRepository) (string, string) {
				return repo.sync(t, repo.tree(t, repo.base), repo.base, repo.upstream), repo.upstream
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			head, upstream := test.head(t, repo)
			ledger, runner, batch := upstreamRunner(t, repo, repo.base,
				Member{Migration: "sync", Generation: 1, Head: head, Upstream: upstream})

			returned, err := runner.checkUpstream(context.Background(), batch)
			if err != nil {
				t.Fatalf("checkUpstream() error = %v", err)
			}
			if !returned || len(batch.ActiveMembers) != 0 {
				t.Fatalf("the member stayed: returned=%v active=%+v", returned, batch.ActiveMembers)
			}
			if got := droppedReasonFor(batch, "sync"); got != "upstream_mismatch" {
				t.Fatalf("dropped reason = %q, want upstream_mismatch", got)
			}
			if len(ledger.returned) != 1 || ledger.returned[0] != "sync" {
				t.Fatalf("returned to the ledger = %v, want the sync member", ledger.returned)
			}
		})
	}
}

// A sync of a base the batch has moved past is not wrong, only behind: the member is returned as
// upstream_stale_base, which tells the sync to build its head again.
func TestUpstreamSyncOfAnOvertakenBaseIsStale(t *testing.T) {
	repo := newUpstreamRepository(t)
	moved := repo.commit(t, repo.base, "moved", "Moved.lean", "def Moved := 0\n")
	head := repo.sync(t, repo.merged(t, repo.base, repo.upstream), repo.base, repo.upstream)
	ledger, runner, batch := upstreamRunner(t, repo, moved,
		Member{Migration: "sync", Generation: 1, Head: head, Upstream: repo.upstream})

	returned, err := runner.checkUpstream(context.Background(), batch)
	if err != nil {
		t.Fatalf("checkUpstream() error = %v", err)
	}
	if !returned || len(batch.ActiveMembers) != 0 {
		t.Fatalf("the stale member stayed: returned=%v active=%+v", returned, batch.ActiveMembers)
	}
	if got := droppedReasonFor(batch, "sync"); got != "upstream_stale_base" {
		t.Fatalf("dropped reason = %q, want upstream_stale_base", got)
	}
	if len(ledger.returned) != 1 || ledger.returned[0] != "sync" {
		t.Fatalf("returned to the ledger = %v, want the sync member", ledger.returned)
	}
}

// A member without an upstream record is an ordinary migration: the check does not look at its head, which is no
// merge of the base at all.
func TestMemberWithoutAnUpstreamRecordIsNotChecked(t *testing.T) {
	repo := newUpstreamRepository(t)
	ledger, runner, batch := upstreamRunner(t, repo, repo.base,
		Member{Migration: "ordinary", Generation: 1, Head: repo.other})

	returned, err := runner.checkUpstream(context.Background(), batch)
	if err != nil {
		t.Fatalf("checkUpstream() error = %v", err)
	}
	if returned || len(batch.ActiveMembers) != 1 || len(ledger.returned) != 0 {
		t.Fatalf("an ordinary member was checked: returned=%v active=%+v ledger=%v", returned, batch.ActiveMembers, ledger.returned)
	}
}

// The upstream check comes before the ledger's Check: a member whose head is not the merge its record claims is
// returned without the ledger ever being asked about it.
func TestUpstreamCheckRunsBeforeTheLedgerCheck(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{}}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-keep": {Green: true}}}
	runner := testRunner(ledger, builder)
	runner.Git.(*gitFake).commits["sync-head"] = Commit{Tree: "tree-sync", Parents: []string{"base"}}

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base",
		Members: []Member{member("keep"), {Migration: "sync", Generation: 1, Head: "sync-head", Upstream: "upstream"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Green; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got := droppedReasonFor(&batch, "sync"); got != "upstream_mismatch" {
		t.Fatalf("dropped reason = %q, want upstream_mismatch", got)
	}
	if got, want := ledger.checked, []string{"keep"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ledger checked %v, want only %v", got, want)
	}
}
