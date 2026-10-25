package landing

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

func TestRunReturnsLedgerRefusalBeforeBuild(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{"reject": "stale"}}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-keep": {Green: true}}}
	runner := testRunner(ledger, builder)

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("keep"), member("reject")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Green; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := ledger.returned, []returned{{migration: "reject", reason: "stale"}}; !sameReturned(got, want) {
		t.Fatalf("returned = %#v, want %#v", got, want)
	}
	if got, want := builder.trees, []blame.Tree{"tree-keep"}; !sameTrees(got, want) {
		t.Fatalf("build trees = %#v, want %#v", got, want)
	}
}

func TestRunMapsPrecheckFindingToChangingMember(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-keep": {Green: true}}}
	check := checkerFake{name: "files", findingsByTree: map[Tree][]Finding{"tree-keep+reject": {{Path: "bad", Reason: "forbidden"}}}}
	runner := testRunner(ledger, builder)
	runner.Checks = []Checker{check}
	runner.Git.(*gitFake).changes["reject"] = gitx.Change{Files: []string{"bad"}}

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("keep"), member("reject")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Green; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := ledger.returned, []returned{{migration: "reject", reason: "forbidden"}}; !sameReturned(got, want) {
		t.Fatalf("returned = %#v, want %#v", got, want)
	}
	if got, want := builder.trees, []blame.Tree{"tree-keep"}; !sameTrees(got, want) {
		t.Fatalf("build trees = %#v, want %#v", got, want)
	}
}

func TestRunConfirmsEvidenceBackedCulpritAndRebuilds(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{
		"tree-bad+keep": {Green: false, Output: "✖ [1/1] Building Alpha.Beta\nerror: Alpha/Beta.lean:1:1: Unknown identifier `gone`\n"},
		"tree-bad":      {Green: false},
		"tree-base":     {Green: true},
		"tree-keep":     {Green: true},
	}}
	runner := testRunner(ledger, builder)
	runner.Git.(*gitFake).evidence["bad"] = blame.IndexDeclarations([]blame.Declaration{{Name: "gone", Text: "def gone := 0"}}, nil)

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("bad"), member("keep")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Green; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := ledger.returned, []returned{{migration: "bad", reason: "build: evidence"}}; !sameReturned(got, want) {
		t.Fatalf("returned = %#v, want %#v", got, want)
	}
	if got, want := batch.ActiveMembers, []Member{member("keep")}; !sameMembers(got, want) {
		t.Fatalf("active members = %#v, want %#v", got, want)
	}
}

func TestRunMarksBaseBrokenWithoutReturningMembers(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{
		"tree-bad":  {Green: false, Output: "✖ [1/1] Building Alpha.Beta\nerror: Alpha/Beta.lean:1:1: error without a named declaration\n"},
		"tree-base": {Green: false},
	}}
	runner := testRunner(ledger, builder)
	runner.ModuleRoots = []string{"Alpha"}

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("bad")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Failed; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := batch.Reason, "base-broken"; got != want {
		t.Fatalf("Run() reason = %q, want %q", got, want)
	}
	if len(ledger.returned) != 0 {
		t.Fatalf("returned = %#v, want no member returned", ledger.returned)
	}
}

func TestRunRetriesInfrastructureWithoutReturningMember(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-one": {InfraClass: "timeout"}}}
	runner := testRunner(ledger, builder)
	runner.InfraRetries = 1

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Failed; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := batch.Reason, "infra"; got != want {
		t.Fatalf("Run() reason = %q, want %q", got, want)
	}
	if got, want := len(builder.trees), 2; got != want {
		t.Fatalf("build count = %d, want %d", got, want)
	}
	if len(ledger.returned) != 0 {
		t.Fatalf("returned = %#v, want no member returned", ledger.returned)
	}
}

func TestDossierCase17FailsUnattributableCheckInsteadOfRepeating(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{}
	runner := testRunner(ledger, builder)
	runner.Checks = []Checker{checkerFake{name: "shape", findings: []Finding{{Path: "untouched", Reason: "unattributable"}}}}

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Failed; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := batch.Reason, "check-unable"; got != want {
		t.Fatalf("Run() reason = %q, want %q", got, want)
	}
	if len(builder.trees) != 0 || len(ledger.returned) != 0 {
		t.Fatalf("unattributable check built or returned members: builds=%#v returns=%#v", builder.trees, ledger.returned)
	}
}

func TestDossierCase19ReportsUnattributableProducerFindingWithoutKicking(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{}
	runner := testRunner(ledger, builder)
	runner.Checks = []Checker{checkerFake{name: "producer", findings: []Finding{{Path: "untouched", Reason: "new binder"}}}}

	batch, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := batch.State, Failed; got != want {
		t.Fatalf("Run() state = %q, want %q", got, want)
	}
	if got, want := batch.Reason, "check-unable"; got != want {
		t.Fatalf("Run() reason = %q, want %q", got, want)
	}
	if len(ledger.returned) != 0 {
		t.Fatalf("returned = %#v, want no member returned", ledger.returned)
	}
}

func TestRunPersistsTransitionsBeforeSideEffects(t *testing.T) {
	ledger := &ledgerFake{}
	builder := &builderFake{results: map[string]blame.BuildResult{"tree-one": {Green: true}}}
	store := &storeFake{}
	runner := testRunner(ledger, builder)
	runner.Store = store
	builder.before = func() {
		if !store.seen(Building) {
			t.Fatal("Build ran before the building transition was stored")
		}
	}

	_, err := runner.Run(context.Background(), Input{
		ID: "batch", Repository: "repo", Base: "base", Members: []Member{member("one")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, state := range []State{Frozen, Checked, Building, Green} {
		if !store.seen(state) {
			t.Fatalf("stored states = %#v, missing %q", store.states, state)
		}
	}
}

func testRunner(ledger *ledgerFake, builder *builderFake) Runner {
	return Runner{
		Store:        &storeFake{},
		Ledger:       ledger,
		Admission:    &batchLedgerFake{ledger: ledger},
		Builder:      builder,
		Hasher:       hasherFake{},
		Git:          newGitFake(),
		MaxRounds:    4,
		InfraRetries: 2,
		ConfigDigest: "digest",
		Toolchain: func(context.Context, Tree) (string, error) {
			return "tool", nil
		},
	}
}

func member(id string) Member {
	return Member{Migration: id, Generation: 1, Head: id + "-head"}
}

type ledgerFake struct {
	refusals map[string]string
	returned []returned
	landed   []string
	commits  map[string]string
}

type returned struct {
	migration string
	reason    string
}

func (f *ledgerFake) Check(_ context.Context, _ string, migration string, _ int64, _ []string, _ map[string]string) (CheckResult, error) {
	return CheckResult{Reason: f.refusals[migration]}, nil
}

func (f *ledgerFake) Land(_ context.Context, request LandRequest) error {
	f.landed = append(f.landed, request.Migration)
	if f.commits == nil {
		f.commits = make(map[string]string)
	}
	f.commits[request.Migration] = request.Commit
	return nil
}

func (f *ledgerFake) ReturnMember(_ context.Context, _ string, migration string, _ int64, reason string) error {
	f.returned = append(f.returned, returned{migration: migration, reason: reason})
	return nil
}

// MigrationState reports what Land recorded, as the real ledger does.
func (f *ledgerFake) MigrationState(_ context.Context, _ string, migration string) (string, string, error) {
	if commit, landed := f.commits[migration]; landed {
		return "landed", commit, nil
	}
	return "open", "", nil
}

type builderFake struct {
	results map[string]blame.BuildResult
	trees   []blame.Tree
	before  func()
}

func (f *builderFake) Build(_ context.Context, tree blame.Tree, _ []string) (blame.BuildResult, error) {
	if f.before != nil {
		f.before()
	}
	f.trees = append(f.trees, tree)
	return f.results[string(tree)], nil
}

type hasherFake struct{}

func (hasherFake) Hashes(context.Context, Tree, []string) (map[string]string, error) {
	return nil, nil
}

type checkerFake struct {
	name           string
	findings       []Finding
	findingsByTree map[Tree][]Finding
	err            error
}

func (f checkerFake) Name() string { return f.name }

func (f checkerFake) Check(_ context.Context, tree Tree) ([]Finding, error) {
	if f.findingsByTree != nil {
		return f.findingsByTree[tree], f.err
	}
	return f.findings, f.err
}

type gitFake struct {
	changes     map[string]gitx.Change
	evidence    map[string]blame.Evidence
	conflicts   map[string]bool
	changeBases []string
}

func newGitFake() *gitFake {
	return &gitFake{changes: map[string]gitx.Change{}, evidence: map[string]blame.Evidence{}, conflicts: map[string]bool{}}
}

func (f *gitFake) Resolve(_ context.Context, _ string, rev string) (string, error) {
	return rev, nil
}

func (f *gitFake) Candidate(_ context.Context, _ string, base string, members []Member) (gitx.Result, error) {
	ids := make([]string, 0, len(members))
	var dropped []gitx.Drop
	for _, member := range members {
		if f.conflicts[member.Migration] {
			dropped = append(dropped, gitx.Drop{ID: member.Migration, Reason: "conflict"})
			continue
		}
		ids = append(ids, member.Migration)
	}
	if len(ids) == 0 {
		return gitx.Result{Tree: "tree-base", Commit: base, Dropped: dropped}, nil
	}
	return gitx.Result{Tree: "tree-" + strings.Join(ids, "+"), Commit: strings.Join(ids, "+"), Dropped: dropped}, nil
}

func (f *gitFake) Rebuild(ctx context.Context, repository, base string, members []Member, without map[string]struct{}) (gitx.Result, error) {
	kept := make([]Member, 0, len(members))
	for _, member := range members {
		if _, excluded := without[member.Migration]; !excluded {
			kept = append(kept, member)
		}
	}
	return f.Candidate(ctx, repository, base, kept)
}

func (f *gitFake) ChangeSet(_ context.Context, _ string, base string, member Member) (gitx.Change, error) {
	f.changeBases = append(f.changeBases, base)
	return f.changes[member.Migration], nil
}

func (f *gitFake) DeclarationEvidence(_ context.Context, _ string, _ string, member Member) (blame.Evidence, error) {
	return f.evidence[member.Migration], nil
}

func (f *gitFake) TreeDeclarations(context.Context, string, Tree) ([]blame.Declaration, error) {
	return nil, nil
}

type storeFake struct {
	states []State
}

func (f *storeFake) Save(_ context.Context, batch *Batch) error {
	f.states = append(f.states, batch.State)
	return nil
}

func (f *storeFake) Load(context.Context, string) (Batch, bool, error) { return Batch{}, false, nil }

func (f *storeFake) Create(ctx context.Context, batch *Batch) (bool, error) {
	return true, f.Save(ctx, batch)
}
func (f *storeFake) ListActive(context.Context) ([]Batch, error) { return nil, nil }

func (f *storeFake) seen(want State) bool {
	for _, state := range f.states {
		if state == want {
			return true
		}
	}
	return false
}

func sameReturned(got, want []returned) bool {
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

func sameTrees(got, want []blame.Tree) bool {
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

func sameMembers(got, want []Member) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index].Migration != want[index].Migration || got[index].Generation != want[index].Generation || got[index].Head != want[index].Head || got[index].ComparisonBase != want[index].ComparisonBase {
			return false
		}
	}
	return true
}
