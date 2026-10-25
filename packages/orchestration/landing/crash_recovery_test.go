package landing

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

// crashStore panics right after its crashAt-th durable write, the way a process
// that dies between a save and its next side effect leaves the record.
type crashStore struct {
	MemoryStore
	crashAt int
	writes  int
}

type crash struct{}

func (s *crashStore) wrote() {
	s.writes++
	if s.writes == s.crashAt {
		panic(crash{})
	}
}

func (s *crashStore) Create(ctx context.Context, batch *Batch) (bool, error) {
	created, err := s.MemoryStore.Create(ctx, batch)
	if err == nil && created {
		s.wrote()
	}
	return created, err
}

func (s *crashStore) Save(ctx context.Context, batch *Batch) error {
	if err := s.MemoryStore.Save(ctx, batch); err != nil {
		return err
	}
	s.wrote()
	return nil
}

// runUntilCrash runs input and reports whether the store's crash interrupted it.
func runUntilCrash(runner Runner, input Input) (crashed bool) {
	defer func() {
		if value := recover(); value != nil {
			if _, ok := value.(crash); !ok {
				panic(value)
			}
			crashed = true
		}
	}()
	_, _ = runner.Run(context.Background(), input)
	return false
}

// recoveryScenario is a batch of a member that the program must return ("bad")
// and a survivor that passes every check ("keep").
type recoveryScenario struct {
	name   string
	runner func() (Runner, *ledgerFake)
	// returnedBeforeBuild is true when "bad" is returned whatever the build
	// says, so the survivor's receipt must never contain it.
	returnedBeforeBuild bool
}

func recoveryScenarios() []recoveryScenario {
	red := map[string]blame.BuildResult{
		"tree-bad+keep": {Output: "error: Alpha/A.lean:1:1: Unknown identifier `gone`"},
		"tree-base":     {Green: true},
		"tree-bad":      {},
		"tree-keep":     {Green: true},
	}
	green := map[string]blame.BuildResult{"tree-bad+keep": {Green: true}, "tree-keep": {Green: true}}
	return []recoveryScenario{
		{name: "ledger refusal", returnedBeforeBuild: true, runner: func() (Runner, *ledgerFake) {
			ledger := &ledgerFake{refusals: map[string]string{"bad": "stale"}}
			return testRunner(ledger, &builderFake{results: green}), ledger
		}},
		{name: "repository check", returnedBeforeBuild: true, runner: func() (Runner, *ledgerFake) {
			ledger := &ledgerFake{}
			runner := testRunner(ledger, &builderFake{results: green})
			runner.Git.(*gitFake).changes["bad"] = gitx.Change{Files: []string{"Bad.lean"}}
			runner.Checks = []Checker{checkerFake{name: "lint", findingsByTree: map[Tree][]Finding{"tree-bad+keep": {{Path: "Bad.lean", Reason: "lint"}}}}}
			return runner, ledger
		}},
		{name: "candidate conflict", returnedBeforeBuild: true, runner: func() (Runner, *ledgerFake) {
			ledger := &ledgerFake{}
			runner := testRunner(ledger, &builderFake{results: green})
			runner.Git.(*gitFake).conflicts["bad"] = true
			return runner, ledger
		}},
		{name: "build blame", runner: func() (Runner, *ledgerFake) {
			ledger := &ledgerFake{}
			runner := testRunner(ledger, &builderFake{results: red})
			runner.Git.(*gitFake).evidence["bad"] = blame.IndexDeclarations([]blame.Declaration{{Name: "gone", Text: "def gone := 0"}}, nil)
			return runner, ledger
		}},
	}
}

func recoveryInput() Input {
	return Input{ID: "batch", Repository: "repo", Base: "base", BaseBranch: "trunk", Members: []Member{member("bad"), member("keep")}}
}

// TestRecoveryAtEveryCrashBoundaryNeverReceiptsAReturnedMember crashes the run
// after each of its durable writes in turn, then reconciles with a builder that
// calls every tree green and a merge queue that merges what it is given, so a
// candidate that still held a returned member would be built, receipted and
// landed.
func TestRecoveryAtEveryCrashBoundaryNeverReceiptsAReturnedMember(t *testing.T) {
	for _, scenario := range recoveryScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			runner, _ := scenario.runner()
			counter := &crashStore{}
			runner.Store = counter
			if batch, err := runner.Run(context.Background(), recoveryInput()); err != nil || batch.State != Green || batch.Receipt.Tree != "tree-keep" {
				t.Fatalf("uninterrupted Run() = %q receipt %+v, %v; want green at tree-keep", batch.State, batch.Receipt, err)
			}
			for crashAt := 1; crashAt <= counter.writes; crashAt++ {
				t.Run(fmt.Sprintf("crash after write %d", crashAt), func(t *testing.T) {
					runner, ledger := scenario.runner()
					store := &crashStore{crashAt: crashAt}
					runner.Store = store
					if !runUntilCrash(runner, recoveryInput()) {
						t.Fatalf("run made fewer than %d writes", crashAt)
					}
					saved, _, _ := store.Load(context.Background(), "batch")
					store.crashAt = 0
					runner.GitHub = &mergingGitHub{}
					runner.Builder = &builderFake{results: map[string]blame.BuildResult{"tree-bad+keep": {Green: true}, "tree-keep": {Green: true}, "tree-bad": {Green: true}, "tree-base": {Green: true}}}

					resumed, err := runner.Reconcile(context.Background())
					if err != nil {
						t.Fatalf("Reconcile() error = %v (saved %s, active %v)", err, saved.State, batchMemberNames(saved.ActiveMembers))
					}
					got := resumed[0]
					if (got.State != Green && got.State != Landed) || got.Receipt == nil {
						t.Fatalf("resumed = %q/%q, want green or landed with a receipt", got.State, got.Reason)
					}
					receiptMembers := batchMemberNames(got.Receipt.Members)
					if want := "tree-" + strings.Join(receiptMembers, "+"); got.Receipt.Tree != want {
						t.Fatalf("receipt tree %q does not hold exactly its members %v", got.Receipt.Tree, receiptMembers)
					}
					if !contains(receiptMembers, "keep") {
						t.Fatalf("receipt members %v lost the survivor", receiptMembers)
					}
					returnedBad := containsDropped(got.Dropped, "bad")
					if returnedBad && contains(receiptMembers, "bad") {
						t.Fatalf("receipt %s holds returned member bad (saved %s, active %v)", got.Receipt.Tree, saved.State, batchMemberNames(saved.ActiveMembers))
					}
					if scenario.returnedBeforeBuild && got.Receipt.Tree != "tree-keep" {
						t.Fatalf("receipt tree = %q, want tree-keep (saved %s, active %v)", got.Receipt.Tree, saved.State, batchMemberNames(saved.ActiveMembers))
					}
					if got.State == Landed && !sameStrings(ledger.landed, receiptMembers) {
						t.Fatalf("landed %v, want the receipt's members %v", ledger.landed, receiptMembers)
					}
					if returnedBad && !ledgerReturned(ledger, "bad") {
						t.Fatalf("bad was dropped but the ledger never heard: %v", ledger.returned)
					}
					for _, operation := range got.Operations {
						if operation.State != OperationComplete {
							t.Fatalf("operation %q still pending after recovery", operation.Key)
						}
					}
					for _, member := range got.ActiveMembers {
						if member.LedgerKey != got.Receipt.Key {
							t.Fatalf("member %q ledger decision at %q, receipt at %q", member.Migration, member.LedgerKey, got.Receipt.Key)
						}
					}
				})
			}
		})
	}
}

func ledgerReturned(ledger *ledgerFake, migration string) bool {
	for _, entry := range ledger.returned {
		if entry.migration == migration {
			return true
		}
	}
	return false
}

// mergingGitHub merges whatever head it was last pushed.
type mergingGitHub struct{ head string }

func (g *mergingGitHub) Push(_ context.Context, _, _, commit string) error {
	g.head = commit
	return nil
}

func (g *mergingGitHub) FindPullRequest(context.Context, string, string) (PullRequest, bool, error) {
	return PullRequest{}, false, nil
}

func (g *mergingGitHub) Open(context.Context, string, string, string, string) (PullRequest, error) {
	return PullRequest{Number: 7, Head: g.head, State: PullRequestOpen}, nil
}

func (g *mergingGitHub) Enqueue(context.Context, string, int) error { return nil }

func (g *mergingGitHub) Wait(context.Context, string, int, string) (PullRequest, error) {
	return PullRequest{Number: 7, Head: g.head, MergedCommit: "merged-" + g.head, State: PullRequestMerged}, nil
}

// runAndPublishUntilCrash runs input and publishes the green batch, and reports
// whether the store's crash interrupted either.
func runAndPublishUntilCrash(runner Runner, input Input) (crashed bool) {
	defer func() {
		if value := recover(); value != nil {
			if _, ok := value.(crash); !ok {
				panic(value)
			}
			crashed = true
		}
	}()
	batch, err := runner.Run(context.Background(), input)
	if err == nil && batch.State == Green {
		_, _ = runner.Publish(context.Background(), batch)
	}
	return false
}

// TestPublishRecoveryAtEveryCrashBoundaryLandsTheSurvivorOnce crashes after
// each durable write from freezing through landing, then reconciles until the
// batch is terminal. The survivor lands exactly once, at the commit that
// merged, and the returned member never lands.
func TestPublishRecoveryAtEveryCrashBoundaryLandsTheSurvivorOnce(t *testing.T) {
	for _, scenario := range recoveryScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			runner, _ := scenario.runner()
			counter := &crashStore{}
			runner.Store = counter
			runner.GitHub = &mergingGitHub{}
			if runAndPublishUntilCrash(runner, recoveryInput()) {
				t.Fatal("uninterrupted run crashed")
			}
			for crashAt := 1; crashAt <= counter.writes; crashAt++ {
				t.Run(fmt.Sprintf("crash after write %d", crashAt), func(t *testing.T) {
					runner, ledger := scenario.runner()
					store := &crashStore{crashAt: crashAt}
					runner.Store = store
					github := &mergingGitHub{}
					runner.GitHub = github
					if !runAndPublishUntilCrash(runner, recoveryInput()) {
						t.Fatalf("run made fewer than %d writes", crashAt)
					}
					store.crashAt = 0
					var got Batch
					for range 3 {
						resumed, err := runner.Reconcile(context.Background())
						if err != nil {
							t.Fatalf("Reconcile() error = %v", err)
						}
						if len(resumed) == 0 {
							break
						}
						got = resumed[0]
						if got.State == Green {
							got, err = runner.Publish(context.Background(), got)
							if err != nil {
								t.Fatalf("Publish() error = %v", err)
							}
						}
					}
					final, _, _ := store.Load(context.Background(), "batch")
					if final.State != Landed {
						t.Fatalf("final state = %q/%q, want landed", final.State, final.Reason)
					}
					if !sameStrings(ledger.landed, []string{"keep"}) {
						t.Fatalf("lands = %v, want keep exactly once", ledger.landed)
					}
					if want := "merged-" + final.Candidate.Commit; ledger.commits["keep"] != want || final.Candidate.Tree != "tree-keep" {
						t.Fatalf("keep landed at %q from tree %q, want %q from tree-keep", ledger.commits["keep"], final.Candidate.Tree, want)
					}
				})
			}
		})
	}
}

// A survivor that the ledger refuses on recovery is returned as well, rather
// than built.
func TestFrozenRecoveryChecksSurvivorOnTheRebuiltCandidate(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{"bad": "stale"}}
	store := &crashStore{crashAt: 3}
	runner := testRunner(ledger, &builderFake{results: map[string]blame.BuildResult{"tree-keep": {Green: true}}})
	runner.Store = store
	if !runUntilCrash(runner, recoveryInput()) {
		t.Fatal("run did not crash")
	}
	store.crashAt = 0
	ledger.refusals["keep"] = "no lock"
	resumed, err := runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := resumed[0]; got.State != Failed || len(got.ActiveMembers) != 0 {
		t.Fatalf("resumed = %q with %v, want failed with no members", got.State, batchMemberNames(got.ActiveMembers))
	}
	if !ledgerReturned(ledger, "keep") {
		t.Fatalf("refused survivor not returned: %v", ledger.returned)
	}
}
