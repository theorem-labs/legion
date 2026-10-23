// Package landing runs an auditable candidate landing state machine.
package landing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

// Tree is an immutable source tree supplied to checks and builds.
type Tree = blame.Tree

// Builder builds a tree. An empty module list selects the configured roots.
type Builder = blame.Builder

// Ledger is the landing-facing contract with the durable ledger.
type Ledger interface {
	Check(context.Context, string, string, int64, []string, map[string]string) (CheckResult, error)
	Land(context.Context, LandRequest) error
	ReturnMember(context.Context, string, string, int64, string) error
	MigrationState(context.Context, string, string) (state string, landedCommit string, err error)
}

// LandRequest matches the ledger's complete landing transaction input.
type LandRequest struct {
	Repo             string            `json:"repo"`
	Migration        string            `json:"migration"`
	Generation       int64             `json:"generation"`
	Commit           string            `json:"commit"`
	ChangedPaths     []string          `json:"changed_paths"`
	Current          map[string]string `json:"current"`
	ChangedContracts map[string]string `json:"changed_contracts"`
	ApprovedPinned   []string          `json:"approved_pinned"`
}

// CheckResult describes one ledger decision.
type CheckResult struct {
	Refused bool
	Reason  string
}

// OperationStatus lets an external dependency confirm a durable operation
// after a crash between the call and its local completion checkpoint.
type OperationStatus interface {
	OperationComplete(context.Context, Operation) (bool, error)
}

// Hasher gives declaration hashes at a tree.
type Hasher interface {
	Hashes(context.Context, Tree, []string) (map[string]string, error)
}

// Git creates candidate commits and exposes their declaration evidence.
type Git interface {
	Resolve(context.Context, string, string) (string, error)
	Candidate(context.Context, string, string, []Member) (gitx.Result, error)
	Rebuild(context.Context, string, string, []Member, map[string]struct{}) (gitx.Result, error)
	ChangeSet(context.Context, string, string, Member) (gitx.Change, error)
	Diff(context.Context, string, string, Member) (string, error)
	DeclarationLines(context.Context, string, Tree, string) ([]string, error)
}

// Checker reports paths that a configured policy rejects in a candidate tree.
type Checker interface {
	Name() string
	Check(context.Context, Tree) ([]Finding, error)
}

// Finding is one policy violation attributed by its changed path.
type Finding struct {
	Path   string
	Reason string
}

// State is the durable state of a landing batch.
type State string

const (
	Frozen     State = "frozen"
	Checked    State = "checked"
	Building   State = "building"
	Blaming    State = "blaming"
	Green      State = "green"
	Publishing State = "publishing"
	Landed     State = "landed"
	Failed     State = "failed"
)

// Member is the frozen identity of one proposed migration.
type Member struct {
	Migration        string            `json:"migration"`
	Generation       int64             `json:"generation"`
	Head             string            `json:"head"`
	ComparisonBase   string            `json:"comparisonBase"`
	Changed          []string          `json:"changed"`
	ReadNames        []string          `json:"readNames"`
	CurrentHashes    map[string]string `json:"currentHashes"`
	HashesTree       string            `json:"hashesTree"`
	ChangedContracts map[string]string `json:"changedContracts"`
	ApprovedPinned   []string          `json:"approvedPinned"`
}

// DroppedMember records why a frozen member left the candidate.
type DroppedMember struct {
	Migration string `json:"migration"`
	Reason    string `json:"reason"`
}

// CheckVerdict is a configured check's final result for the current candidate.
type CheckVerdict struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// Receipt binds a green candidate to its policy and evidence.
type Receipt struct {
	Tree              string         `json:"tree"`
	Toolchain         string         `json:"toolchain"`
	ConfigurationHash string         `json:"configurationHash"`
	Checks            []CheckVerdict `json:"checks"`
	Members           []Member       `json:"members"`
	BuildLogDigest    string         `json:"buildLogDigest"`
}

// JSON returns the stable, machine-readable receipt body.
func (r Receipt) JSON() ([]byte, error) {
	return json.Marshal(r)
}

// OperationState tracks a durable intent before an external side effect and its
// completion only after that effect returns.
type OperationState string

const (
	OperationPending  OperationState = "pending"
	OperationComplete OperationState = "complete"
)

// Operation is the idempotency record for one member or batch side effect.
type Operation struct {
	Key        string         `json:"key"`
	Kind       string         `json:"kind"`
	Batch      string         `json:"batch"`
	Repository string         `json:"repository"`
	Member     string         `json:"member"`
	Generation int64          `json:"generation"`
	Commit     string         `json:"commit"`
	Reason     string         `json:"reason"`
	State      OperationState `json:"state"`
}

// Batch persists an immutable input set independently from members eventually
// landed after a publisher confirms the merge.
type Batch struct {
	ID                string               `json:"id"`
	Repository        string               `json:"repository"`
	Base              string               `json:"base"`
	BaseBranch        string               `json:"baseBranch"`
	Branch            string               `json:"branch"`
	Candidate         gitx.Result          `json:"candidate"`
	InitialCandidate  gitx.Result          `json:"initialCandidate"`
	CheckedTree       string               `json:"checkedTree"`
	CandidateDirty    bool                 `json:"candidateDirty"`
	FrozenMembers     []Member             `json:"frozenMembers"`
	ActiveMembers     []Member             `json:"activeMembers"`
	LandedMembers     []Member             `json:"landedMembers"`
	NotifiedMembers   []string             `json:"notifiedMembers"`
	Dropped           []DroppedMember      `json:"dropped"`
	Operations        map[string]Operation `json:"operations"`
	State             State                `json:"state"`
	Reason            string               `json:"reason"`
	Round             int                  `json:"round"`
	ChangesTree       string               `json:"changesTree"`
	ChangeMembers     []string             `json:"changeMembers"`
	LedgerTree        string               `json:"ledgerTree"`
	LedgerMembers     []string             `json:"ledgerMembers"`
	Checks            []CheckVerdict       `json:"checks"`
	CheckPolicyDigest string               `json:"checkPolicyDigest"`
	CheckPolicy       []string             `json:"checkPolicy"`
	CheckMembers      []string             `json:"checkMembers"`
	Score             CheckVerdict         `json:"score"`
	PullRequest       PullRequest          `json:"pullRequest"`
	MergeValidated    bool                 `json:"mergeValidated"`
	Receipt           *Receipt             `json:"receipt"`
}

// Input starts one frozen batch.
type Input struct {
	ID         string   `json:"id"`
	Repository string   `json:"repository"`
	Base       string   `json:"base"`
	BaseBranch string   `json:"baseBranch"`
	Members    []Member `json:"members"`
}

// BatchStore persists each state transition before its following side effect.
type BatchStore interface {
	Create(context.Context, Batch) (bool, error)
	Save(context.Context, Batch) error
	Load(context.Context, string) (Batch, bool, error)
	ListActive(context.Context) ([]Batch, error)
}

// Runner owns the dependencies and policy for one batch run.
type Runner struct {
	Store        BatchStore
	Ledger       Ledger
	Builder      Builder
	Hasher       Hasher
	Git          Git
	GitHub       GitHub
	Score        Scorer
	Events       EventPublisher
	ScorePath    string
	Checks       []Checker
	ModuleRoots  []string
	MaxRounds    int
	InfraRetries int
	ConfigDigest string
	Toolchain    func(context.Context, Tree) (string, error)
}

// Run freezes, checks, builds and attributes one batch until it is green or failed.
func (r Runner) Run(ctx context.Context, input Input) (Batch, error) {
	if err := r.valid(); err != nil {
		return Batch{}, err
	}
	frozenInput, err := r.freezeInput(ctx, input)
	if err != nil {
		return Batch{}, err
	}
	input = frozenInput
	if existing, found, err := r.Store.Load(ctx, input.ID); err != nil {
		return Batch{}, fmt.Errorf("load batch %q: %w", input.ID, err)
	} else if found {
		if !sameInput(existing, input) {
			return Batch{}, fmt.Errorf("batch %q already has a different immutable input", input.ID)
		}
		return existing, nil
	}
	batch := Batch{
		ID:                input.ID,
		Repository:        input.Repository,
		Base:              input.Base,
		BaseBranch:        input.BaseBranch,
		FrozenMembers:     cloneMembers(input.Members),
		ActiveMembers:     cloneMembers(input.Members),
		Operations:        make(map[string]Operation),
		CheckPolicyDigest: r.ConfigDigest,
		CheckPolicy:       r.checkPolicyNames(),
	}
	candidate, err := r.candidate(ctx, batch.Repository, batch.Base, batch.ActiveMembers)
	if err != nil {
		return Batch{}, fmt.Errorf("freeze candidate: %w", err)
	}
	batch.Candidate = candidate
	batch.InitialCandidate = candidate
	batch.State = Frozen
	created, err := r.Store.Create(ctx, batch)
	if err != nil {
		return Batch{}, fmt.Errorf("create batch %q: %w", batch.ID, err)
	}
	if !created {
		existing, found, err := r.Store.Load(ctx, batch.ID)
		if err != nil {
			return Batch{}, fmt.Errorf("load raced batch %q: %w", batch.ID, err)
		}
		if !found || !sameInput(existing, input) {
			return Batch{}, fmt.Errorf("batch %q already has a different immutable input", batch.ID)
		}
		return existing, nil
	}
	if err := r.applyCandidateDrops(ctx, &batch); err != nil {
		return Batch{}, err
	}
	if err := r.collectChanges(ctx, &batch); err != nil {
		return r.fail(ctx, &batch, "changes")
	}
	if err := r.checkLedger(ctx, &batch); err != nil {
		return r.fail(ctx, &batch, "ledger")
	}
	if err := r.rebuild(ctx, &batch); err != nil {
		return r.fail(ctx, &batch, "candidate")
	}
	if err := r.revalidateCandidate(ctx, &batch); err != nil {
		return r.fail(ctx, &batch, "ledger")
	}
	batch.Round = 0
	return r.drive(ctx, &batch, true)
}

func (r Runner) drive(ctx context.Context, batch *Batch, checkBeforeBuild bool) (Batch, error) {
	for ; batch.Round < r.roundLimit(); batch.Round++ {
		if checkBeforeBuild {
			ready, err := r.runChecks(ctx, batch)
			if err != nil {
				return r.fail(ctx, batch, "check-unable")
			}
			if !ready {
				if err := r.rebuild(ctx, batch); err != nil {
					return r.fail(ctx, batch, "candidate")
				}
				if err := r.revalidateCandidate(ctx, batch); err != nil {
					return r.fail(ctx, batch, "ledger")
				}
				batch.Round--
				continue
			}
			if err := r.transition(ctx, batch, Checked, ""); err != nil {
				return Batch{}, err
			}
		}
		checkBeforeBuild = true
		result, infra, err := r.build(ctx, batch)
		if err != nil {
			return r.fail(ctx, batch, "build")
		}
		if infra {
			return r.fail(ctx, batch, "infra")
		}
		if result.Green {
			receipt, err := r.receipt(ctx, *batch, result.Output)
			if err != nil {
				return r.fail(ctx, batch, "receipt")
			}
			batch.Receipt = &receipt
			if err := r.transition(ctx, batch, Green, ""); err != nil {
				return Batch{}, err
			}
			return *batch, nil
		}
		if err := r.transition(ctx, batch, Blaming, ""); err != nil {
			return Batch{}, err
		}
		baseBroken, culprits, err := r.blame(ctx, batch, result.Output)
		if err != nil {
			var infraFailure InfrastructureError
			if errors.As(err, &infraFailure) {
				return r.fail(ctx, batch, "infra")
			}
			return r.fail(ctx, batch, "blame")
		}
		if baseBroken || len(culprits) == 0 {
			return r.fail(ctx, batch, "base-broken")
		}
		if err := r.returnMembers(ctx, batch, culprits); err != nil {
			return r.fail(ctx, batch, "return")
		}
		if err := r.rebuild(ctx, batch); err != nil {
			return r.fail(ctx, batch, "candidate")
		}
		if err := r.revalidateCandidate(ctx, batch); err != nil {
			return r.fail(ctx, batch, "ledger")
		}
	}
	return r.fail(ctx, batch, "max-rounds")
}

func (r Runner) valid() error {
	switch {
	case r.Store == nil:
		return fmt.Errorf("landing store is required")
	case r.Ledger == nil:
		return fmt.Errorf("ledger is required")
	case r.Builder == nil:
		return fmt.Errorf("builder is required")
	case r.Hasher == nil:
		return fmt.Errorf("hasher is required")
	case r.Git == nil:
		return fmt.Errorf("git is required")
	}
	return nil
}

func (r Runner) candidate(ctx context.Context, repository, base string, members []Member) (gitx.Result, error) {
	return r.Git.Candidate(ctx, repository, base, members)
}

func (r Runner) collectChanges(ctx context.Context, batch *Batch) error {
	for index := range batch.ActiveMembers {
		member := &batch.ActiveMembers[index]
		comparisonBase := member.ComparisonBase
		if comparisonBase == "" {
			comparisonBase = batch.Base
		}
		change, err := r.Git.ChangeSet(ctx, batch.Repository, comparisonBase, *member)
		if err != nil {
			return fmt.Errorf("read changed files for %q: %w", member.Migration, err)
		}
		member.Changed = append([]string(nil), change.Files...)
	}
	batch.ChangesTree = batch.Candidate.Tree
	batch.ChangeMembers = memberIDs(batch.ActiveMembers)
	return r.Store.Save(ctx, *batch)
}

func (r Runner) checkLedger(ctx context.Context, batch *Batch) error {
	members := cloneMembers(batch.ActiveMembers)
	for _, snapshot := range members {
		index := memberIndex(batch.ActiveMembers, snapshot.Migration)
		if index < 0 {
			continue
		}
		member := &batch.ActiveMembers[index]
		hashes, err := r.Hasher.Hashes(ctx, Tree(batch.Candidate.Tree), member.ReadNames)
		if err != nil {
			return fmt.Errorf("hash reads for %q: %w", member.Migration, err)
		}
		member.CurrentHashes = cloneHashes(hashes)
		member.HashesTree = batch.Candidate.Tree
		if err := r.Store.Save(ctx, *batch); err != nil {
			return fmt.Errorf("record current hashes for %q: %w", member.Migration, err)
		}
		result, err := r.Ledger.Check(ctx, batch.Repository, member.Migration, member.Generation, member.Changed, hashes)
		if err != nil {
			return fmt.Errorf("check %q: %w", member.Migration, err)
		}
		if result.Refused || result.Reason != "" {
			reason := result.Reason
			if reason == "" {
				reason = "refused"
			}
			if err := r.returnMember(ctx, batch, member.Migration, reason); err != nil {
				return err
			}
		}
	}
	batch.LedgerTree = batch.Candidate.Tree
	batch.LedgerMembers = memberIDs(batch.ActiveMembers)
	if err := r.Store.Save(ctx, *batch); err != nil {
		return err
	}
	return nil
}

func (r Runner) revalidateCandidate(ctx context.Context, batch *Batch) error {
	for {
		dropped := len(batch.Dropped)
		if err := r.checkLedger(ctx, batch); err != nil {
			return err
		}
		if len(batch.Dropped) == dropped {
			return nil
		}
		if err := r.rebuild(ctx, batch); err != nil {
			return err
		}
	}
}

func (r Runner) applyCandidateDrops(ctx context.Context, batch *Batch) error {
	for _, drop := range batch.Candidate.Dropped {
		if err := r.returnMember(ctx, batch, drop.ID, drop.Reason); err != nil {
			return err
		}
	}
	return nil
}

func (r Runner) returnMembers(ctx context.Context, batch *Batch, culprits map[string]string) error {
	ids := make([]string, 0, len(culprits))
	for id := range culprits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.returnMember(ctx, batch, id, culprits[id]); err != nil {
			return err
		}
	}
	return nil
}

func (r Runner) returnMember(ctx context.Context, batch *Batch, migration, reason string) error {
	member, found := findMember(batch.ActiveMembers, migration)
	if !found {
		return nil
	}
	key := "return/" + migration
	if operation, found := batch.operation(key); found && operation.State == OperationComplete {
		return nil
	}
	batch.ActiveMembers = withoutMember(batch.ActiveMembers, migration)
	batch.CandidateDirty = true
	if !containsDropped(batch.Dropped, migration) {
		batch.Dropped = append(batch.Dropped, DroppedMember{Migration: migration, Reason: reason})
	}
	batch.startOperation(Operation{Key: key, Kind: "return", Batch: batch.ID, Repository: batch.Repository, Member: migration, Generation: member.Generation, Reason: reason})
	if err := r.Store.Save(ctx, *batch); err != nil {
		return fmt.Errorf("record return intent for %q: %w", migration, err)
	}
	if err := r.Ledger.ReturnMember(ctx, batch.Repository, member.Migration, member.Generation, reason); err != nil {
		return fmt.Errorf("return %q: %w", migration, err)
	}
	batch.completeOperation(key)
	if err := r.Store.Save(ctx, *batch); err != nil {
		return fmt.Errorf("record returned member %q: %w", migration, err)
	}
	return nil
}

func (r Runner) rebuild(ctx context.Context, batch *Batch) error {
	without := make(map[string]struct{}, len(batch.Dropped))
	for _, dropped := range batch.Dropped {
		without[dropped.Migration] = struct{}{}
	}
	candidate, err := r.Git.Rebuild(ctx, batch.Repository, batch.Base, batch.FrozenMembers, without)
	if err != nil {
		return fmt.Errorf("rebuild candidate: %w", err)
	}
	batch.Candidate = candidate
	batch.CandidateDirty = false
	batch.CheckedTree = ""
	for index := range batch.ActiveMembers {
		batch.ActiveMembers[index].CurrentHashes = nil
		batch.ActiveMembers[index].HashesTree = ""
	}
	if err := r.Store.Save(ctx, *batch); err != nil {
		return fmt.Errorf("record rebuilt candidate: %w", err)
	}
	return r.applyCandidateDrops(ctx, batch)
}

func (r Runner) runChecks(ctx context.Context, batch *Batch) (bool, error) {
	for index, checker := range r.Checks {
		name := checker.Name()
		if name == "" {
			name = fmt.Sprintf("check-%d", index+1)
		}
		findings, err := checker.Check(ctx, Tree(batch.Candidate.Tree))
		if err != nil {
			batch.setCheck(CheckVerdict{Name: name, Verdict: "unable", Reason: err.Error()})
			if saveErr := r.Store.Save(ctx, *batch); saveErr != nil {
				return false, fmt.Errorf("record unavailable check: %w", saveErr)
			}
			return false, err
		}
		if len(findings) == 0 {
			batch.setCheck(CheckVerdict{Name: name, Verdict: "pass"})
			continue
		}
		culprits := make(map[string]string)
		for _, finding := range findings {
			matched := false
			for _, member := range batch.ActiveMembers {
				if contains(member.Changed, finding.Path) {
					culprits[member.Migration] = finding.Reason
					matched = true
				}
			}
			if !matched {
				batch.setCheck(CheckVerdict{Name: name, Verdict: "unable", Reason: finding.Reason})
				if err := r.Store.Save(ctx, *batch); err != nil {
					return false, fmt.Errorf("record unattributable check: %w", err)
				}
				return false, fmt.Errorf("check %q cannot attribute %q", name, finding.Path)
			}
		}
		batch.setCheck(CheckVerdict{Name: name, Verdict: "fail", Reason: findings[0].Reason})
		if err := r.returnMembers(ctx, batch, culprits); err != nil {
			return false, err
		}
		return false, nil
	}
	batch.CheckedTree = batch.Candidate.Tree
	batch.CheckMembers = memberIDs(batch.ActiveMembers)
	if err := r.Store.Save(ctx, *batch); err != nil {
		return false, fmt.Errorf("record checks: %w", err)
	}
	return true, nil
}

func containsDropped(dropped []DroppedMember, migration string) bool {
	for _, member := range dropped {
		if member.Migration == migration {
			return true
		}
	}
	return false
}

func (b *Batch) operation(key string) (Operation, bool) {
	operation, found := b.Operations[key]
	return operation, found
}

func (b *Batch) startOperation(operation Operation) {
	if b.Operations == nil {
		b.Operations = make(map[string]Operation)
	}
	operation.State = OperationPending
	b.Operations[operation.Key] = operation
}

func (b *Batch) completeOperation(key string) {
	operation := b.Operations[key]
	operation.State = OperationComplete
	b.Operations[key] = operation
}

func (r Runner) build(ctx context.Context, batch *Batch) (blame.BuildResult, bool, error) {
	for attempt := 0; attempt <= r.infraLimit(); attempt++ {
		if err := r.transition(ctx, batch, Building, ""); err != nil {
			return blame.BuildResult{}, false, err
		}
		result, err := r.Builder.Build(ctx, Tree(batch.Candidate.Tree), nil)
		if err != nil {
			if attempt == r.infraLimit() {
				return blame.BuildResult{}, true, nil
			}
			continue
		}
		if result.InfraClass == "" {
			return result, false, nil
		}
		if attempt == r.infraLimit() {
			return result, true, nil
		}
	}
	return blame.BuildResult{}, true, nil
}

// InfrastructureError says a builder did not reach a source verdict.
type InfrastructureError struct {
	Class string
}

func (e InfrastructureError) Error() string {
	return "infrastructure failure: " + e.Class
}

func (r Runner) buildTree(ctx context.Context, tree Tree, modules []string) (blame.BuildResult, error) {
	var lastClass string
	for attempt := 0; attempt <= r.infraLimit(); attempt++ {
		result, err := r.Builder.Build(ctx, tree, modules)
		if err != nil {
			lastClass = "launch"
		} else if result.InfraClass != "" {
			lastClass = result.InfraClass
		} else {
			return result, nil
		}
	}
	return blame.BuildResult{}, InfrastructureError{Class: lastClass}
}

type retryingBuilder struct {
	runner Runner
}

func (b retryingBuilder) Build(ctx context.Context, tree blame.Tree, modules []string) (blame.BuildResult, error) {
	return b.runner.buildTree(ctx, Tree(tree), modules)
}

func (r Runner) blame(ctx context.Context, batch *Batch, output string) (bool, map[string]string, error) {
	base, err := r.candidate(ctx, batch.Repository, batch.Base, nil)
	if err != nil {
		return false, nil, fmt.Errorf("build base candidate: %w", err)
	}
	baseResult, err := r.buildTree(ctx, Tree(base.Tree), nil)
	if err != nil {
		return false, nil, fmt.Errorf("build base: %w", err)
	}
	if !baseResult.Green {
		return true, nil, nil
	}
	evidence := make(map[string]blame.Evidence, len(batch.ActiveMembers))
	for _, member := range batch.ActiveMembers {
		comparisonBase := member.ComparisonBase
		if comparisonBase == "" {
			comparisonBase = batch.Base
		}
		diff, err := r.Git.Diff(ctx, batch.Repository, comparisonBase, member)
		if err != nil {
			return false, nil, fmt.Errorf("read diff for %q: %w", member.Migration, err)
		}
		evidence[member.Migration] = blame.IndexDiff(diff)
	}
	report := blame.Parse(output, r.ModuleRoots)
	lines, err := r.declarationLines(ctx, batch, Tree(base.Tree), report)
	if err != nil {
		return false, nil, err
	}
	attributions := blame.IdentifyDetailed(report, evidence, lines)
	suspects := make(map[string][]string)
	for _, attribution := range attributions {
		suspects[attribution.Module] = append(suspects[attribution.Module], attribution.Member)
	}
	members := make([]blame.Member, 0, len(batch.ActiveMembers))
	for _, member := range batch.ActiveMembers {
		members = append(members, blame.Member{ID: member.Migration, Head: member.Head})
	}
	candidate := func(ctx context.Context, base string, selected []blame.Member) (blame.Tree, error) {
		selectedMembers := make([]Member, 0, len(selected))
		for _, selectedMember := range selected {
			member, found := findMember(batch.ActiveMembers, selectedMember.ID)
			if !found {
				return "", fmt.Errorf("unknown member %q", selectedMember.ID)
			}
			selectedMembers = append(selectedMembers, member)
		}
		result, err := r.candidate(ctx, batch.Repository, base, selectedMembers)
		if err != nil {
			return "", err
		}
		return blame.Tree(result.Tree), nil
	}
	confirmation, err := blame.ConfirmModules(ctx, batch.Base, report.RedModules, members, suspects, candidate, retryingBuilder{runner: r})
	if err != nil {
		return false, nil, err
	}
	if len(confirmation.BaseBroken) != 0 {
		return true, nil, nil
	}
	culprits := make(map[string]string, len(confirmation.Confirmed)+len(confirmation.Bisected))
	for _, moduleCulprits := range confirmation.ByModule {
		for _, culprit := range moduleCulprits {
			if culprit.Kind == "evidence" {
				culprits[culprit.Member] = "build: evidence"
			} else {
				culprits[culprit.Member] = "build: bisect"
			}
		}
	}
	return false, culprits, nil
}

func (r Runner) declarationLines(ctx context.Context, batch *Batch, base Tree, report blame.Report) (map[string]blame.TreeLines, error) {
	names := make(map[string]struct{})
	for _, block := range report.Blocks {
		for _, name := range blame.Names(block.Message) {
			names[name.Value] = struct{}{}
		}
	}
	lines := make(map[string]blame.TreeLines, len(names))
	for name := range names {
		baseLines, err := r.Git.DeclarationLines(ctx, batch.Repository, base, name)
		if err != nil {
			return nil, fmt.Errorf("read base declaration %q: %w", name, err)
		}
		candidateLines, err := r.Git.DeclarationLines(ctx, batch.Repository, Tree(batch.Candidate.Tree), name)
		if err != nil {
			return nil, fmt.Errorf("read candidate declaration %q: %w", name, err)
		}
		lines[name] = blame.TreeLines{Base: baseLines, Candidate: candidateLines}
	}
	return lines, nil
}

func attributionMembers(attributions []blame.Attribution) []string {
	members := make(map[string]struct{}, len(attributions))
	for _, attribution := range attributions {
		members[attribution.Member] = struct{}{}
	}
	out := make([]string, 0, len(members))
	for member := range members {
		out = append(out, member)
	}
	sort.Strings(out)
	return out
}

func (r Runner) receipt(ctx context.Context, batch Batch, output string) (Receipt, error) {
	if r.Toolchain == nil {
		return Receipt{}, fmt.Errorf("toolchain identity is required")
	}
	toolchain, err := r.Toolchain(ctx, Tree(batch.Candidate.Tree))
	if err != nil {
		return Receipt{}, fmt.Errorf("read toolchain: %w", err)
	}
	if toolchain == "" {
		return Receipt{}, fmt.Errorf("toolchain identity is empty")
	}
	if r.ConfigDigest == "" {
		return Receipt{}, fmt.Errorf("configuration hash is required")
	}
	digest := sha256.Sum256([]byte(output))
	return Receipt{
		Tree:              batch.Candidate.Tree,
		Toolchain:         toolchain,
		ConfigurationHash: r.ConfigDigest,
		Checks:            append([]CheckVerdict(nil), batch.Checks...),
		Members:           cloneMembers(batch.ActiveMembers),
		BuildLogDigest:    hex.EncodeToString(digest[:]),
	}, nil
}

func (r Runner) freezeInput(ctx context.Context, input Input) (Input, error) {
	base, err := r.Git.Resolve(ctx, input.Repository, input.Base)
	if err != nil {
		return Input{}, fmt.Errorf("resolve base: %w", err)
	}
	input.Base = base
	input.Members = cloneMembers(input.Members)
	for index := range input.Members {
		head, err := r.Git.Resolve(ctx, input.Repository, input.Members[index].Head)
		if err != nil {
			return Input{}, fmt.Errorf("resolve member %q: %w", input.Members[index].Migration, err)
		}
		input.Members[index].Head = head
		if input.Members[index].ComparisonBase != "" {
			comparisonBase, err := r.Git.Resolve(ctx, input.Repository, input.Members[index].ComparisonBase)
			if err != nil {
				return Input{}, fmt.Errorf("resolve comparison base for %q: %w", input.Members[index].Migration, err)
			}
			input.Members[index].ComparisonBase = comparisonBase
		}
	}
	return input, nil
}

func (r Runner) fail(ctx context.Context, batch *Batch, reason string) (Batch, error) {
	if err := r.transition(ctx, batch, Failed, reason); err != nil {
		return *batch, err
	}
	return *batch, nil
}

func (r Runner) transition(ctx context.Context, batch *Batch, state State, reason string) error {
	batch.State = state
	batch.Reason = reason
	if err := r.Store.Save(ctx, *batch); err != nil {
		return fmt.Errorf("store %s state: %w", state, err)
	}
	return nil
}

func (r Runner) checkPolicyNames() []string {
	names := make([]string, len(r.Checks))
	for index, checker := range r.Checks {
		name := checker.Name()
		if name == "" {
			name = fmt.Sprintf("check-%d", index+1)
		}
		names[index] = name
	}
	sort.Strings(names)
	return names
}

func memberIDs(members []Member) []string {
	ids := make([]string, len(members))
	for index, member := range members {
		ids[index] = member.Migration
	}
	return ids
}

func (r Runner) roundLimit() int {
	if r.MaxRounds > 0 {
		return r.MaxRounds
	}
	return 1
}

func (r Runner) infraLimit() int {
	if r.InfraRetries >= 0 {
		return r.InfraRetries
	}
	return 0
}

func (b *Batch) setCheck(verdict CheckVerdict) {
	for index, current := range b.Checks {
		if current.Name == verdict.Name {
			b.Checks[index] = verdict
			return
		}
	}
	b.Checks = append(b.Checks, verdict)
}

func sameInput(batch Batch, input Input) bool {
	if batch.ID != input.ID || batch.Repository != input.Repository || batch.Base != input.Base || batch.BaseBranch != input.BaseBranch || len(batch.FrozenMembers) != len(input.Members) {
		return false
	}
	for index, member := range batch.FrozenMembers {
		other := input.Members[index]
		if member.Migration != other.Migration || member.Generation != other.Generation || member.Head != other.Head || member.ComparisonBase != other.ComparisonBase {
			return false
		}
	}
	return true
}

func memberIndex(members []Member, migration string) int {
	for index, member := range members {
		if member.Migration == migration {
			return index
		}
	}
	return -1
}

func findMember(members []Member, migration string) (Member, bool) {
	for _, member := range members {
		if member.Migration == migration {
			return member, true
		}
	}
	return Member{}, false
}

func withoutMember(members []Member, migration string) []Member {
	out := members[:0]
	for _, member := range members {
		if member.Migration != migration {
			out = append(out, member)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cloneMembers(members []Member) []Member {
	out := make([]Member, len(members))
	for index, member := range members {
		out[index] = member
		out[index].Changed = append([]string(nil), member.Changed...)
		out[index].ReadNames = append([]string(nil), member.ReadNames...)
		out[index].ApprovedPinned = append([]string(nil), member.ApprovedPinned...)
		if member.CurrentHashes != nil {
			out[index].CurrentHashes = make(map[string]string, len(member.CurrentHashes))
			for name, hash := range member.CurrentHashes {
				out[index].CurrentHashes[name] = hash
			}
		}
		if member.ChangedContracts != nil {
			out[index].ChangedContracts = make(map[string]string, len(member.ChangedContracts))
			for name, hash := range member.ChangedContracts {
				out[index].ChangedContracts[name] = hash
			}
		}
	}
	return out
}

func cloneHashes(hashes map[string]string) map[string]string {
	if hashes == nil {
		return nil
	}
	out := make(map[string]string, len(hashes))
	for name, hash := range hashes {
		out[name] = hash
	}
	return out
}

// MemoryStore is a concurrency-safe in-memory store for tests and single-process callers.
type MemoryStore struct {
	mu      sync.Mutex
	batches map[string]Batch
}

// Create stores a batch only when its identity is not already present.
func (s *MemoryStore) Create(_ context.Context, batch Batch) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches == nil {
		s.batches = make(map[string]Batch)
	}
	if _, found := s.batches[batch.ID]; found {
		return false, nil
	}
	s.batches[batch.ID] = cloneBatch(batch)
	return true, nil
}

// Save writes a deep copy of a batch.
func (s *MemoryStore) Save(_ context.Context, batch Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches == nil {
		s.batches = make(map[string]Batch)
	}
	s.batches[batch.ID] = cloneBatch(batch)
	return nil
}

// Load reads a deep copy of a batch.
func (s *MemoryStore) Load(_ context.Context, id string) (Batch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch, found := s.batches[id]
	return cloneBatch(batch), found, nil
}

// ListActive returns every batch that is not terminal.
func (s *MemoryStore) ListActive(_ context.Context) ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Batch, 0, len(s.batches))
	for _, batch := range s.batches {
		if batch.State != Failed && batch.State != Landed {
			out = append(out, cloneBatch(batch))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func cloneBatch(batch Batch) Batch {
	batch.FrozenMembers = cloneMembers(batch.FrozenMembers)
	batch.ActiveMembers = cloneMembers(batch.ActiveMembers)
	batch.LandedMembers = cloneMembers(batch.LandedMembers)
	batch.NotifiedMembers = append([]string(nil), batch.NotifiedMembers...)
	batch.Dropped = append([]DroppedMember(nil), batch.Dropped...)
	batch.ChangeMembers = append([]string(nil), batch.ChangeMembers...)
	batch.LedgerMembers = append([]string(nil), batch.LedgerMembers...)
	batch.CheckPolicy = append([]string(nil), batch.CheckPolicy...)
	batch.CheckMembers = append([]string(nil), batch.CheckMembers...)
	batch.Checks = append([]CheckVerdict(nil), batch.Checks...)
	if batch.Operations != nil {
		operations := make(map[string]Operation, len(batch.Operations))
		for key, operation := range batch.Operations {
			operations[key] = operation
		}
		batch.Operations = operations
	}
	if batch.Receipt != nil {
		receipt := *batch.Receipt
		receipt.Members = cloneMembers(receipt.Members)
		receipt.Checks = append([]CheckVerdict(nil), receipt.Checks...)
		batch.Receipt = &receipt
	}
	return batch
}
