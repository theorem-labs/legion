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
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

// Tree is an immutable source tree supplied to checks and builds.
type Tree = blame.Tree

// Builder builds a tree. Build removes its directory when it returns; it serves
// the blame and confirmation builds. BuildKept builds the candidate and keeps
// its directory until the caller releases it, so the hasher can read the build.
type Builder interface {
	blame.Builder
	BuildKept(context.Context, Tree) (KeptBuild, error)
}

// KeptBuild is a candidate build whose directory stays until Release. Release
// is never nil, and a second call does nothing.
type KeptBuild struct {
	Result    blame.BuildResult
	Directory string
	Release   func()
}

func noRelease() {}

// Ledger is the landing-facing contract with the durable ledger.
type Ledger interface {
	Check(context.Context, CheckRequest) (CheckResult, error)
	Land(context.Context, LandRequest) error
	ReturnMember(context.Context, RoundRequest) error
	Contracts(context.Context, string) ([]Contract, error)
	MigrationState(context.Context, string, string) (state string, landedCommit string, err error)
	// Escalate enqueues the work item for the owner. A repeated call for an
	// open item of the same owner does nothing.
	Escalate(ctx context.Context, repository, item, owner string) error
}

// Review is an owner's review of one contract's statement change at a head.
type Review struct {
	Contract string `json:"contract"`
	Reviewer string `json:"reviewer"`
	Head     string `json:"head"`
}

// LandRequest is the ledger's LandRequest, field for field: the candidate, the
// current hash of each read, and the member's statement records.
type LandRequest struct {
	Repo               string            `json:"repo"`
	Migration          string            `json:"migration"`
	Generation         int64             `json:"generation"`
	Commit             string            `json:"commit"`
	ChangedPaths       []string          `json:"changed_paths"`
	Current            map[string]string `json:"current"`
	ChangedContracts   map[string]string `json:"changed_contracts"`
	Head               string            `json:"head"`
	CompatibilityProof string            `json:"compatibility_proof"`
	ReviewedHead       string            `json:"reviewed_head"`
	Reviews            []Review          `json:"reviews"`
	ApprovedPinned     []string          `json:"approved_pinned"`
}

// CheckRequest is the ledger's CheckRequest. DeferReads asks for the check
// before the build, which compares no read hashes and needs no commit.
type CheckRequest struct {
	LandRequest
	DeferReads bool `json:"defer_reads"`
}

// The ledger's check outcomes. A deferred check that finds nothing is incomplete.
const (
	CheckPass       = "pass"
	CheckRefused    = "refused"
	CheckIncomplete = "incomplete"
)

// Violation is one reason the ledger gives that a candidate must not land.
type Violation struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// CheckResult is the ledger's decision on one member.
type CheckResult struct {
	Outcome    string      `json:"outcome"`
	Violations []Violation `json:"violations"`
}

// RoundRequest is the ledger's RoundRequest: one member sent back from one
// batch. ReadyEvent is the ready record the batch read, so the round clears
// that record and never a newer one.
type RoundRequest struct {
	Repo       string `json:"repo"`
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Reason     string `json:"reason"`
	Batch      string `json:"batch"`
	Key        string `json:"key"`
	ReadyEvent int64  `json:"ready_event"`
	Detail     string `json:"detail"`
}

// Contract is a registered contract, as the ledger's contract list prints it.
type Contract struct {
	Name          string `json:"name"`
	StatementHash string `json:"statement_hash"`
}

// OperationStatus lets an external dependency confirm a durable operation
// after a crash between the call and its local completion checkpoint.
type OperationStatus interface {
	OperationComplete(context.Context, Operation) (bool, error)
}

// HashInput asks for the statement hash of each name at a tree. BuildDirectory
// is the candidate's kept build; it is empty for the base tree.
type HashInput struct {
	Tree           Tree
	BuildDirectory string
	Names          []string
}

// StatementHash is one name's statement at a tree and the paths that declare
// it. No path means the name is absent, with the empty hash; more than one
// path means the name is ambiguous, and its hash cannot be used.
type StatementHash struct {
	Hash  string
	Paths []string
}

// Ambiguous reports that more than one path declares the name.
func (h StatementHash) Ambiguous() bool { return len(h.Paths) > 1 }

// Hasher gives statement hashes at a tree.
type Hasher interface {
	Hashes(context.Context, HashInput) (map[string]StatementHash, error)
}

// HashRetryError says the hasher could not reach a resource it needs, such as
// the index database (its exit 75). The step is retried later.
type HashRetryError struct{ Output string }

func (e HashRetryError) Error() string { return "hasher must retry: " + e.Output }

// HashUnableError says the hasher could not check a tree (its exit 3).
type HashUnableError struct{ Output string }

func (e HashUnableError) Error() string { return "hasher unable to check: " + e.Output }

// Git creates candidate commits and exposes declaration evidence.
type Git interface {
	Resolve(context.Context, string, string) (string, error)
	Candidate(context.Context, string, string, []Member) (gitx.Result, error)
	Rebuild(context.Context, string, string, []Member, map[string]struct{}) (gitx.Result, error)
	ChangeSet(context.Context, string, string, Member) (gitx.Change, error)
	DeclarationEvidence(context.Context, string, string, Member, blame.InstanceNames) (blame.Evidence, error)
	TreeDeclarations(context.Context, string, Tree) ([]blame.Declaration, error)
}

// CheckInput is what a configured check reads: the candidate tree and the
// batch's frozen base commit.
type CheckInput struct {
	Tree Tree   // the candidate tree
	Base string // the batch's base commit
}

// Checker reports what a configured policy rejects or flags in a candidate.
type Checker interface {
	Name() string
	Check(context.Context, CheckInput) ([]Finding, error)
}

// Finding is one check result. A findings-format line has only Path and
// Reason and is attributed by its changed path; a graph line names a
// declaration, attributed by the members' declaration evidence.
type Finding struct {
	Path        string     `json:"path"`
	Reason      string     `json:"reason"`                // "<rule>: <message>" for a graph finding
	Verdict     string     `json:"verdict"`               // VerdictRefuse or VerdictFlag; a findings-format line is VerdictRefuse
	Declaration string     `json:"declaration,omitempty"` // empty for a findings-format line
	Kind        blame.Kind `json:"kind,omitempty"`
	Line        int        `json:"line,omitempty"`
	Col         int        `json:"col,omitempty"`
	Note        string     `json:"note,omitempty"` // an unattributed finding: why SelectFor named nobody ("no evidence" or Pick's note)
}

// Refusal is one member a check returns, with the finding and evidence.
type Refusal struct {
	Member      string     `json:"member"`
	Declaration string     `json:"declaration,omitempty"`
	Kind        blame.Kind `json:"kind,omitempty"`
	Path        string     `json:"path"`
	Line        int        `json:"line,omitempty"`
	Strength    int        `json:"strength,omitempty"`
	Evidence    string     `json:"evidence,omitempty"`
	Reason      string     `json:"reason"`
}

// Flag is one flagged finding attributed to one member. A flag returns no
// member; it is carried in the receipt and the member's landing event.
type Flag struct {
	Check       string     `json:"check"`
	Member      string     `json:"member"`
	Declaration string     `json:"declaration"`
	Kind        blame.Kind `json:"kind,omitempty"`
	Path        string     `json:"path"`
	Line        int        `json:"line"`
	Reason      string     `json:"reason"`
}

// CheckUnableError is a check that could not decide (a graph check's exit
// 3). Its verdict is "unable" and the batch goes on to the build, which
// decides; any other check error fails the batch.
type CheckUnableError struct{ Check, Reason string }

func (e *CheckUnableError) Error() string {
	return fmt.Sprintf("check %q is unable: %s", e.Check, e.Reason)
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
	// Declined ends a batch that was admitted and did not land: its returned members are back with their owners and
	// its other members go to its successor.
	Declined State = "declined"
	// Empty ends a batch that lost every member.
	Empty State = "empty"
	// LandedUnrecorded ends a batch that GitHub merged and the ledger did not land. An operator resolves it.
	LandedUnrecorded State = "landed_unrecorded"
)

// terminal reports whether a state ends a batch: the runner never advances, admits or saves it again.
func (s State) terminal() bool {
	switch s {
	case Landed, Failed, Declined, Empty, LandedUnrecorded:
		return true
	}
	return false
}

// Member is the frozen identity of one proposed migration. Changed is read once,
// when the batch freezes. The statement records (ChangedContracts as the owner
// declares them, Reviews, ReviewedHead, CompatibilityProof and ApprovedPinned)
// and ReadyEvent come from the member's ready record and never change.
// CurrentHashes, DerivedContracts and the ledger decision are derived values,
// each stored with the batch key it was derived at.
type Member struct {
	Migration          string            `json:"migration"`
	Generation         int64             `json:"generation"`
	Head               string            `json:"head"`
	ReadyEvent         int64             `json:"ready_event"`
	ComparisonBase     string            `json:"comparisonBase"`
	Changed            []string          `json:"changed"`
	ReadNames          []string          `json:"readNames"`
	CurrentHashes      map[string]string `json:"currentHashes"`
	DerivedContracts   map[string]string `json:"derivedContracts"`
	HashesKey          string            `json:"hashesKey"`
	DeferredKey        string            `json:"deferredKey"`
	LedgerKey          string            `json:"ledgerKey"`
	ChangedContracts   map[string]string `json:"changedContracts"`
	Reviews            []Review          `json:"reviews"`
	ReviewedHead       string            `json:"reviewedHead"`
	CompatibilityProof string            `json:"compatibilityProof"`
	ApprovedPinned     []string          `json:"approvedPinned"`
}

// DroppedMember records why a frozen member left the candidate.
type DroppedMember struct {
	Migration string `json:"migration"`
	Reason    string `json:"reason"`
}

// CheckVerdict is a configured check's final result for the current candidate.
type CheckVerdict struct {
	Name         string    `json:"name"`
	Verdict      string    `json:"verdict"` // "pass", "fail" (members refused), "flag" (flags only), "unable"
	Reason       string    `json:"reason"`
	Refused      []Refusal `json:"refused,omitempty"`
	Flags        []Flag    `json:"flags,omitempty"`
	Unattributed []Finding `json:"unattributed,omitempty"` // findings no member's evidence selects: the check is unable
	Seconds      float64   `json:"seconds"`
}

// Receipt binds a green candidate to its policy and evidence. Key is the batch
// key the receipt was derived at.
type Receipt struct {
	Key               string         `json:"key"`
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
	Detail     string         `json:"detail,omitempty"`
	State      OperationState `json:"state"`
}

// Batch is the durable record of one landing batch. Its authoritative inputs are
// the base, the frozen members and the active (ordered) member set, the
// checking-policy digest and the phase (State). Every derived value (the
// candidate, each member's current hashes and ledger decision, the repository
// checks and the receipt) is stored with the key it was derived at, and it is
// recomputed before the next side effect whenever that key differs from the
// batch's current key. A batch that the ledger admitted is the one exception:
// it is in the merge queue, so it completes under the receipt, and so the
// policy, that its admission checked, whatever policy the runner has later.
type Batch struct {
	// SchemaVersion is the version of this record's shape, which the ledger stores as the program sends it.
	SchemaVersion int `json:"schema_version"`
	// RecordVersion is the ledger's version of the stored record. It orders every write: a save names the version it
	// read, and the store replaces it with the version the write produced. It is not part of the stored record.
	RecordVersion int64 `json:"record_version,omitempty"`
	// Successor is the batch that the members of a declined batch go to next.
	Successor string `json:"successor,omitempty"`
	// EscalateTo is the owner that the ledger escalates a landed_unrecorded batch to.
	EscalateTo string `json:"escalate_to,omitempty"`
	// PublishAttempts counts the failed enqueues and waits of an admitted batch.
	PublishAttempts int `json:"publish_attempts,omitempty"`
	// Withdraw is the publishing step whose failures started the batch's withdraw, and empty while none is started.
	Withdraw string `json:"withdraw,omitempty"`
	// Admission is the ledger's admission of this batch, which the store fills on every read. It is the ledger's
	// column, not part of the record.
	Admission string `json:"-"`
	// PostLandEffects are the effects that the landing records as intents in its own transaction.
	PostLandEffects []PostLandEffect `json:"post_land_effects,omitempty"`
	// PendingEffects are the keys of this batch's effects still to do, which the store fills on every read from the
	// ledger's own rows.
	PendingEffects    []string             `json:"-"`
	ID                string               `json:"id"`
	Repository        string               `json:"repository"`
	Base              string               `json:"base"`
	BaseBranch        string               `json:"baseBranch"`
	Branch            string               `json:"branch"`
	Candidate         gitx.Result          `json:"candidate"`
	CandidateKey      string               `json:"candidateKey"`
	InitialCandidate  gitx.Result          `json:"initialCandidate"`
	FrozenMembers     []Member             `json:"frozenMembers"`
	ActiveMembers     []Member             `json:"activeMembers"`
	Dropped           []DroppedMember      `json:"dropped"`
	Operations        map[string]Operation `json:"operations"`
	State             State                `json:"state"`
	Reason            string               `json:"reason"`
	Round             int                  `json:"round"`
	HashAttempts      int                  `json:"hashAttempts,omitempty"`
	CheckPolicyDigest string               `json:"checkPolicyDigest"`
	Checks            []CheckVerdict       `json:"checks"`
	ChecksKey         string               `json:"checksKey"`
	PullRequest       PullRequest          `json:"pullRequest"`
	Receipt           *Receipt             `json:"receipt"`
}

// batchKey identifies the inputs of every derived value: the base, the ordered
// active members and the checking policy.
func batchKey(base string, members []Member, policy string) string {
	type identity struct {
		Migration      string `json:"migration"`
		Generation     int64  `json:"generation"`
		Head           string `json:"head"`
		ComparisonBase string `json:"comparisonBase"`
	}
	identities := make([]identity, len(members))
	for index, member := range members {
		identities[index] = identity{Migration: member.Migration, Generation: member.Generation, Head: member.Head, ComparisonBase: member.ComparisonBase}
	}
	body, _ := json.Marshal(struct {
		Base    string     `json:"base"`
		Policy  string     `json:"policy"`
		Members []identity `json:"members"`
	}{Base: base, Policy: policy, Members: identities})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// key is the batch's current key under the runner's checking policy.
func (r Runner) key(batch Batch) string {
	return batchKey(batch.Base, batch.ActiveMembers, r.policyDigest())
}

// fresh reports whether every derived value (the candidate, each member's read
// hashes and ledger decision, the repository checks and the receipt) is at the
// batch's current key.
func (r Runner) fresh(batch Batch) bool {
	key := r.key(batch)
	if batch.Receipt == nil || batch.Receipt.Key != key || batch.CandidateKey != key || batch.ChecksKey != key {
		return false
	}
	for _, member := range batch.ActiveMembers {
		if member.LedgerKey != key || member.HashesKey != key {
			return false
		}
	}
	return true
}

// Input starts one frozen batch.
type Input struct {
	ID         string   `json:"id"`
	Repository string   `json:"repository"`
	Base       string   `json:"base"`
	BaseBranch string   `json:"baseBranch"`
	Members    []Member `json:"members"`
}

// BatchSchemaVersion is the version of the batch record that this program writes.
const BatchSchemaVersion = 1

// BatchStore persists each state transition before its following side effect. Create and Save take the batch so that
// the store can replace its record version with the version its write produced.
type BatchStore interface {
	Create(context.Context, *Batch) (bool, error)
	Save(context.Context, *Batch) error
	Load(context.Context, string) (Batch, bool, error)
	ListActive(context.Context) ([]Batch, error)
}

// Runner owns the dependencies and policy for one batch run.
type Runner struct {
	Store        BatchStore
	Ledger       Ledger
	Admission    BatchLedger
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
	// EscalateTo is the escalations owner. A step that is retried (a hasher
	// exit 75) is escalated to it when its attempts reach EscalateAfter
	// (default 10). RetryBackoff is the first wait between attempts (default
	// 10 seconds); each wait doubles, up to five minutes.
	EscalateTo    string
	EscalateAfter int
	RetryBackoff  time.Duration
	// PublishingRetryLimit is the number of failed enqueues and waits after which an admitted batch is withdrawn.
	PublishingRetryLimit int
	// PublishingBackoff is the first pause after a failed publishing step, doubled after each failure. Zero is 30
	// seconds.
	PublishingBackoff time.Duration
}

// Run freezes one batch and advances it until it is green or failed.
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
	if err := r.readChanges(ctx, input.Repository, input.Base, input.Members); err != nil {
		return Batch{}, err
	}
	batch := Batch{
		ID:                input.ID,
		Repository:        input.Repository,
		Base:              input.Base,
		BaseBranch:        input.BaseBranch,
		FrozenMembers:     cloneMembers(input.Members),
		ActiveMembers:     cloneMembers(input.Members),
		Operations:        make(map[string]Operation),
		CheckPolicyDigest: r.policyDigest(),
		State:             Frozen,
	}
	candidate, err := r.candidate(ctx, batch.Repository, batch.Base, batch.ActiveMembers)
	if err != nil {
		return Batch{}, fmt.Errorf("freeze candidate: %w", err)
	}
	r.setCandidate(&batch, candidate)
	batch.InitialCandidate = candidate
	created, err := r.Store.Create(ctx, &batch)
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
	return r.advance(ctx, &batch)
}

// advance brings every derived value to the batch's current key, in order: the
// candidate, each member's deferred ledger check, the repository checks, the
// candidate build, the statement hashes at the base and the candidate, each
// member's derived statement changes and its full ledger check, and the
// receipt. It builds until the candidate is green or the batch fails. A
// returned member changes the key, so the loop starts again from the candidate.
func (r Runner) advance(ctx context.Context, batch *Batch) (Batch, error) {
	if policy := r.policyDigest(); batch.CheckPolicyDigest != policy {
		batch.CheckPolicyDigest = policy
		if err := r.Store.Save(ctx, batch); err != nil {
			return *batch, fmt.Errorf("record check policy: %w", err)
		}
	}
	for {
		if err := r.ensureCandidate(ctx, batch); err != nil {
			if r.reloaded(ctx, batch, err) {
				return *batch, nil
			}
			return r.fail(ctx, batch, "candidate")
		}
		if batch.State.terminal() {
			return *batch, nil
		}
		returned, err := r.deferredCheck(ctx, batch)
		if err != nil {
			if r.reloaded(ctx, batch, err) {
				return *batch, nil
			}
			return r.fail(ctx, batch, "ledger")
		}
		if batch.State.terminal() {
			return *batch, nil
		}
		if returned {
			continue
		}
		returned, err = r.runChecks(ctx, batch)
		if err != nil {
			var unable checkUnable
			if errors.As(err, &unable) {
				return r.fail(ctx, batch, "check-unable")
			}
			if r.reloaded(ctx, batch, err) {
				return *batch, nil
			}
			return r.fail(ctx, batch, "return")
		}
		if batch.State.terminal() {
			return *batch, nil
		}
		if returned {
			continue
		}
		if r.fresh(*batch) {
			if batch.State != Green {
				if err := r.transition(ctx, batch, Green, ""); err != nil {
					return Batch{}, err
				}
			}
			return *batch, nil
		}
		if batch.Round >= r.roundLimit() {
			return r.fail(ctx, batch, "max-rounds")
		}
		build, infra, err := r.build(ctx, batch)
		if err != nil {
			return r.fail(ctx, batch, "build")
		}
		if infra {
			return r.fail(ctx, batch, "infra")
		}
		if build.Result.Green {
			returned, err := r.checkStatements(ctx, batch, build)
			if err != nil {
				var unable HashUnableError
				var unattributed statementUnattributed
				switch {
				case errors.Is(err, errHashRetryLater):
					return *batch, r.transition(ctx, batch, batch.State, "hash-retry")
				case errors.As(err, &unable):
					return r.fail(ctx, batch, "hash-unable")
				case errors.As(err, &unattributed):
					return r.fail(ctx, batch, "statement-unattributed")
				case r.reloaded(ctx, batch, err):
					return *batch, nil
				}
				return r.fail(ctx, batch, "ledger")
			}
			if batch.State.terminal() {
				return *batch, nil
			}
			if returned {
				continue
			}
			receipt, err := r.receipt(ctx, *batch, build.Result.Output)
			if err != nil {
				return r.fail(ctx, batch, "receipt")
			}
			batch.Receipt = &receipt
			if err := r.transition(ctx, batch, Green, ""); err != nil {
				return Batch{}, err
			}
			return *batch, nil
		}
		build.Release()
		if err := r.transition(ctx, batch, Blaming, ""); err != nil {
			return Batch{}, err
		}
		baseBroken, culprits, err := r.blame(ctx, batch, build.Result.Output)
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
		batch.Round++
		if err := r.dropMembers(ctx, batch, culprits); err != nil {
			if r.reloaded(ctx, batch, err) {
				return *batch, nil
			}
			return r.fail(ctx, batch, "return")
		}
		if batch.State.terminal() {
			return *batch, nil
		}
	}
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

// readChanges records each member's changed files once, when the batch
// freezes. They depend only on the member's head and its comparison base.
func (r Runner) readChanges(ctx context.Context, repository, base string, members []Member) error {
	for index := range members {
		member := &members[index]
		comparisonBase := member.ComparisonBase
		if comparisonBase == "" {
			comparisonBase = base
		}
		change, err := r.Git.ChangeSet(ctx, repository, comparisonBase, *member)
		if err != nil {
			return fmt.Errorf("read changed files for %q: %w", member.Migration, err)
		}
		member.Changed = append([]string(nil), change.Files...)
	}
	return nil
}

// setCandidate records a candidate with the key of the member set it contains:
// the active members less those that the candidate itself dropped.
func (r Runner) setCandidate(batch *Batch, candidate gitx.Result) {
	batch.Candidate = candidate
	batch.CandidateKey = batchKey(batch.Base, candidateMembers(*batch), r.policyDigest())
}

// ensureCandidate makes the candidate match the current key. It rebuilds from
// the base and the frozen members without the returned ones when the stored
// key differs, then returns each member that the candidate itself dropped (a
// conflict, a carrier, a duplicate or a member already in the base), which
// brings the current key to the candidate's.
func (r Runner) ensureCandidate(ctx context.Context, batch *Batch) error {
	if batch.CandidateKey != batchKey(batch.Base, candidateMembers(*batch), r.policyDigest()) {
		without := make(map[string]struct{}, len(batch.Dropped))
		for _, dropped := range batch.Dropped {
			without[dropped.Migration] = struct{}{}
		}
		candidate, err := r.Git.Rebuild(ctx, batch.Repository, batch.Base, batch.FrozenMembers, without)
		if err != nil {
			return fmt.Errorf("rebuild candidate: %w", err)
		}
		r.setCandidate(batch, candidate)
		if err := r.Store.Save(ctx, batch); err != nil {
			return fmt.Errorf("record rebuilt candidate: %w", err)
		}
	}
	drops := make(map[string]string, len(batch.Candidate.Dropped))
	for _, drop := range batch.Candidate.Dropped {
		drops[drop.ID] = drop.Reason
	}
	return r.dropMembers(ctx, batch, drops)
}

// candidateMembers is the active members less those the stored candidate dropped.
func candidateMembers(batch Batch) []Member {
	out := make([]Member, 0, len(batch.ActiveMembers))
	for _, member := range batch.ActiveMembers {
		dropped := false
		for _, drop := range batch.Candidate.Dropped {
			if drop.ID == member.Migration {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, member)
		}
	}
	return out
}

// deferredCheck runs the ledger's check before the build for each active member
// whose deferred decision is not at the current key: its changed paths, the
// contracts it declares as changed, its head and its statement records, with
// defer_reads. An incomplete outcome passes. A refused member is returned with
// the kinds of its violations, and deferredCheck reports whether it returned any.
func (r Runner) deferredCheck(ctx context.Context, batch *Batch) (bool, error) {
	key := r.key(*batch)
	returned := false
	for _, snapshot := range cloneMembers(batch.ActiveMembers) {
		if snapshot.DeferredKey == key {
			continue
		}
		request := CheckRequest{LandRequest: landRequest(batch.Repository, snapshot, "", nil, snapshot.ChangedContracts), DeferReads: true}
		result, err := r.Ledger.Check(ctx, request)
		if err != nil {
			return returned, fmt.Errorf("deferred check %q: %w", snapshot.Migration, err)
		}
		if result.Outcome == CheckRefused {
			reason, detail := refusal(result)
			if err := r.dropMember(ctx, batch, snapshot.Migration, reason, detail); err != nil {
				return returned, err
			}
			if batch.State.terminal() {
				return true, nil
			}
			returned = true
			continue
		}
		if result.Outcome != CheckIncomplete && result.Outcome != CheckPass {
			return returned, fmt.Errorf("deferred check %q: outcome %q", snapshot.Migration, result.Outcome)
		}
		if index := memberIndex(batch.ActiveMembers, snapshot.Migration); index >= 0 {
			batch.ActiveMembers[index].DeferredKey = key
			if err := r.Store.Save(ctx, batch); err != nil {
				return returned, fmt.Errorf("record deferred decision for %q: %w", snapshot.Migration, err)
			}
		}
	}
	return returned, nil
}

// statementUnattributed is a statement change that no member's diff touches.
type statementUnattributed struct{ name string }

func (e statementUnattributed) Error() string {
	return fmt.Sprintf("statement of %q changed, but no member changes a path that declares it", e.name)
}

// checkStatements runs after a green candidate build. It hashes every
// registered contract and every member's read at the batch's base tree and at
// the candidate tree with the kept build, releases the build when the
// candidate hasher exits, and derives each member's statement changes: a
// contract whose hash differs, attributed to the member whose diff touches a
// path that declares it. A member whose derived changes differ from the ones it
// declares is returned with undeclared_statement_change, and a member that reads
// or changes an ambiguous name with hash_unable. Each remaining member gets the
// full ledger check: a read of a contract it changes is current at the base,
// every other read at the candidate. A refused member is returned with the kinds
// of its violations. checkStatements reports whether it returned any member.
func (r Runner) checkStatements(ctx context.Context, batch *Batch, build KeptBuild) (bool, error) {
	defer build.Release()
	contracts, err := r.Ledger.Contracts(ctx, batch.Repository)
	if err != nil {
		return false, fmt.Errorf("list contracts: %w", err)
	}
	names := statementNames(contracts, batch.ActiveMembers)
	base, err := r.candidate(ctx, batch.Repository, batch.Base, nil)
	if err != nil {
		return false, fmt.Errorf("read base tree: %w", err)
	}
	before, err := r.hashes(ctx, batch, HashInput{Tree: Tree(base.Tree), Names: names})
	if err != nil {
		return false, fmt.Errorf("hash base tree: %w", err)
	}
	after, err := r.hashes(ctx, batch, HashInput{Tree: Tree(batch.Candidate.Tree), BuildDirectory: build.Directory, Names: names})
	build.Release()
	if err != nil {
		return false, fmt.Errorf("hash candidate tree: %w", err)
	}
	derived := make(map[string]map[string]string, len(batch.ActiveMembers))
	unable := make(map[string]string)
	for _, contract := range contracts {
		name := contract.Name
		was, now := before[name], after[name]
		paths := append(append([]string(nil), was.Paths...), now.Paths...)
		if was.Ambiguous() || now.Ambiguous() {
			for _, member := range batch.ActiveMembers {
				if touchesAny(member.Changed, paths) {
					unable[member.Migration] = name
				}
			}
			continue
		}
		if was.Hash == now.Hash {
			continue
		}
		attributed := false
		for _, member := range batch.ActiveMembers {
			if touchesAny(member.Changed, paths) {
				if derived[member.Migration] == nil {
					derived[member.Migration] = make(map[string]string)
				}
				derived[member.Migration][name] = now.Hash
				attributed = true
			}
		}
		if !attributed {
			return false, statementUnattributed{name: name}
		}
	}
	for _, member := range batch.ActiveMembers {
		for _, name := range member.ReadNames {
			if before[name].Ambiguous() || after[name].Ambiguous() {
				unable[member.Migration] = name
			}
		}
	}
	key := r.key(*batch)
	returned := false
	for _, snapshot := range cloneMembers(batch.ActiveMembers) {
		if name, found := unable[snapshot.Migration]; found {
			if err := r.dropMember(ctx, batch, snapshot.Migration, "hash_unable", fmt.Sprintf("%s is declared at more than one path", name)); err != nil {
				return returned, err
			}
			if batch.State.terminal() {
				return true, nil
			}
			returned = true
			continue
		}
		changes := derived[snapshot.Migration]
		if declared, found := sortedKeys(snapshot.ChangedContracts), sortedKeys(changes); !equalStrings(declared, found) {
			detail := fmt.Sprintf("declared %s; derived %s", formatSet(declared), formatSet(found))
			if err := r.dropMember(ctx, batch, snapshot.Migration, "undeclared_statement_change", detail); err != nil {
				return returned, err
			}
			if batch.State.terminal() {
				return true, nil
			}
			returned = true
			continue
		}
		current := make(map[string]string, len(snapshot.ReadNames))
		for _, name := range snapshot.ReadNames {
			if _, changes := changes[name]; changes {
				current[name] = before[name].Hash
			} else {
				current[name] = after[name].Hash
			}
		}
		result, err := r.Ledger.Check(ctx, CheckRequest{LandRequest: landRequest(batch.Repository, snapshot, batch.Candidate.Commit, current, changes)})
		if err != nil {
			return returned, fmt.Errorf("check %q: %w", snapshot.Migration, err)
		}
		if result.Outcome != CheckPass {
			if result.Outcome != CheckRefused {
				return returned, fmt.Errorf("check %q: outcome %q", snapshot.Migration, result.Outcome)
			}
			reason, detail := refusal(result)
			if err := r.dropMember(ctx, batch, snapshot.Migration, reason, detail); err != nil {
				return returned, err
			}
			if batch.State.terminal() {
				return true, nil
			}
			returned = true
			continue
		}
		if index := memberIndex(batch.ActiveMembers, snapshot.Migration); index >= 0 {
			member := &batch.ActiveMembers[index]
			member.CurrentHashes = current
			member.DerivedContracts = cloneHashes(changes)
			member.HashesKey = key
			member.LedgerKey = key
			if err := r.Store.Save(ctx, batch); err != nil {
				return returned, fmt.Errorf("record ledger decision for %q: %w", snapshot.Migration, err)
			}
		}
	}
	return returned, nil
}

// errHashRetryLater stops a run whose hasher keeps asking for a retry (exit
// 75) after the escalation threshold: the batch stays active, and the next
// reconcile tries again.
var errHashRetryLater = errors.New("hasher retry pending")

// hashes calls the hasher and retries an exit 75 with a doubling backoff. Each
// failed attempt is counted on the batch and saved. When the count reaches the
// escalation threshold, the batch is escalated to the escalations owner as
// landing:<batch>:hash@<threshold>, once, and the run stops with
// errHashRetryLater; after that, each run makes one attempt. A hasher that
// answers resets the count.
func (r Runner) hashes(ctx context.Context, batch *Batch, input HashInput) (map[string]StatementHash, error) {
	for {
		hashes, err := r.Hasher.Hashes(ctx, input)
		var retry HashRetryError
		if !errors.As(err, &retry) {
			if err == nil && batch.HashAttempts != 0 {
				batch.HashAttempts = 0
				if err := r.Store.Save(ctx, batch); err != nil {
					return nil, fmt.Errorf("record hasher attempts: %w", err)
				}
			}
			return hashes, err
		}
		batch.HashAttempts++
		if err := r.Store.Save(ctx, batch); err != nil {
			return nil, fmt.Errorf("record hasher attempt: %w", err)
		}
		threshold := r.escalateAfter()
		if batch.HashAttempts >= threshold {
			item := fmt.Sprintf("landing:%s:hash@%d", batch.ID, threshold)
			if err := r.Ledger.Escalate(ctx, batch.Repository, item, r.EscalateTo); err != nil {
				return nil, fmt.Errorf("escalate hasher retries: %w", err)
			}
			return nil, errHashRetryLater
		}
		wait := r.retryBackoff() << (batch.HashAttempts - 1)
		if limit := 5 * time.Minute; wait > limit || wait <= 0 {
			wait = limit
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r Runner) escalateAfter() int {
	if r.EscalateAfter > 0 {
		return r.EscalateAfter
	}
	return 10
}

func (r Runner) retryBackoff() time.Duration {
	if r.RetryBackoff > 0 {
		return r.RetryBackoff
	}
	return 10 * time.Second
}

// landRequest is the ledger request for a member: its changed paths, the given
// commit, current hashes and changed contracts, its head and its statement
// records.
func landRequest(repository string, member Member, commit string, current, changed map[string]string) LandRequest {
	return LandRequest{
		Repo:               repository,
		Migration:          member.Migration,
		Generation:         member.Generation,
		Commit:             commit,
		ChangedPaths:       append([]string{}, member.Changed...),
		Current:            cloneHashes(current),
		ChangedContracts:   cloneHashes(changed),
		Head:               member.Head,
		CompatibilityProof: member.CompatibilityProof,
		ReviewedHead:       member.ReviewedHead,
		Reviews:            append([]Review(nil), member.Reviews...),
		ApprovedPinned:     append([]string(nil), member.ApprovedPinned...),
	}
}

// refusal is a refused check's return reason, the kinds of its violations in
// order without repeats, and its detail, their messages.
func refusal(result CheckResult) (string, string) {
	var kinds, messages []string
	for _, violation := range result.Violations {
		if !contains(kinds, violation.Kind) {
			kinds = append(kinds, violation.Kind)
		}
		messages = append(messages, violation.Message)
	}
	if len(kinds) == 0 {
		return "refused", ""
	}
	return strings.Join(kinds, ","), strings.Join(messages, "; ")
}

// statementNames is every registered contract and every member's read, sorted.
func statementNames(contracts []Contract, members []Member) []string {
	seen := make(map[string]struct{})
	for _, contract := range contracts {
		seen[contract.Name] = struct{}{}
	}
	for _, member := range members {
		for _, name := range member.ReadNames {
			seen[name] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func formatSet(names []string) string {
	return "[" + strings.Join(names, ",") + "]"
}

func touchesAny(changed, paths []string) bool {
	for _, path := range paths {
		if contains(changed, path) {
			return true
		}
	}
	return false
}

// roundRequest is the ledger round for a return: its key is
// <batch>/<member>/<generation>, since a member leaves one batch at most once
// at one generation, so a retry after a crash replays the same round.
func roundRequest(operation Operation, member Member) RoundRequest {
	return RoundRequest{
		Repo:       operation.Repository,
		ID:         member.Migration,
		Generation: member.Generation,
		Reason:     operation.Reason,
		Batch:      operation.Batch,
		Key:        fmt.Sprintf("%s/%s/%d", operation.Batch, member.Migration, member.Generation),
		ReadyEvent: member.ReadyEvent,
		Detail:     operation.Detail,
	}
}

// checkUnable is a configured check that failed, so the batch fails.
type checkUnable struct{ error }

func (e checkUnable) Unwrap() error { return e.error }

// runChecks runs the configured repository checks on the candidate when their
// stored result is not at the current key. The members the first failing
// check refuses are returned, and runChecks reports that it returned them.
func (r Runner) runChecks(ctx context.Context, batch *Batch) (bool, error) {
	key := r.key(*batch)
	if batch.ChecksKey == key {
		return false, nil
	}
	verdicts, culprits, err := r.evaluateChecks(ctx, batch, false)
	batch.Checks = verdicts
	if err != nil {
		if saveErr := r.Store.Save(ctx, batch); saveErr != nil {
			return false, fmt.Errorf("record failed check: %w", saveErr)
		}
		return false, err
	}
	if len(culprits) != 0 {
		if err := r.dropMembers(ctx, batch, culprits); err != nil {
			return false, err
		}
		return true, nil
	}
	batch.ChecksKey = key
	if err := r.transition(ctx, batch, Checked, ""); err != nil {
		return false, err
	}
	return false, nil
}

// evaluateChecks runs the configured checks on the batch's candidate and
// returns their verdicts and the members to return, each with its reason. It
// saves nothing. It stops after the first check that refuses a member unless
// every is set. A finding with no declaration (a findings-format line)
// returns every active member that changed its path. A graph finding is
// attributed by blame.SelectFor; when it selects nobody for any finding, the
// check is unable and returns and flags nobody. A check that is unable lets
// the next check run; any other check error is a checkUnable, which fails the
// batch, returned with the verdicts so far.
func (r Runner) evaluateChecks(ctx context.Context, batch *Batch, every bool) ([]CheckVerdict, map[string]string, error) {
	verdicts := make([]CheckVerdict, 0, len(r.Checks))
	culprits := make(map[string]string)
	attribute := checkAttribution{runner: r, batch: batch}
	for index, checker := range r.Checks {
		name := checkName(checker, index)
		started := time.Now()
		findings, err := checker.Check(ctx, CheckInput{Tree: Tree(batch.Candidate.Tree), Base: batch.Base})
		verdict := CheckVerdict{Name: name, Seconds: time.Since(started).Seconds()}
		if err != nil {
			verdict.Verdict, verdict.Reason = "unable", err.Error()
			var unable *CheckUnableError
			if errors.As(err, &unable) {
				verdict.Reason = unable.Reason
				verdicts = append(verdicts, verdict)
				continue
			}
			return append(verdicts, verdict), nil, checkUnable{err}
		}
		checkCulprits := make(map[string]string)
		var unattributed []string
		for _, finding := range findings {
			if finding.Declaration == "" {
				matched := false
				for _, member := range batch.ActiveMembers {
					if contains(member.Changed, finding.Path) {
						addCulprit(checkCulprits, member.Migration, finding.Reason)
						verdict.Refused = append(verdict.Refused, Refusal{Member: member.Migration, Path: finding.Path, Reason: finding.Reason})
						matched = true
					}
				}
				if !matched {
					verdict.Verdict, verdict.Reason = "unable", finding.Reason
					return append(verdicts, verdict), nil, checkUnable{fmt.Errorf("check %q cannot attribute %q", name, finding.Path)}
				}
				continue
			}
			selection, err := attribute.selectFor(ctx, finding)
			if err != nil {
				verdict.Verdict, verdict.Reason = "unable", err.Error()
				return append(verdicts, verdict), nil, checkUnable{err}
			}
			if len(selection.Members) == 0 {
				note := selection.Note
				if note == "" {
					note = "no evidence"
				}
				finding.Note = note
				verdict.Unattributed = append(verdict.Unattributed, finding)
				unattributed = append(unattributed, fmt.Sprintf("%s at %s:%d: %s", finding.Declaration, finding.Path, finding.Line, note))
				continue
			}
			for _, row := range selection.Members {
				if finding.Verdict == VerdictFlag {
					verdict.Flags = append(verdict.Flags, Flag{Check: name, Member: row.Member, Declaration: finding.Declaration, Kind: finding.Kind, Path: finding.Path, Line: finding.Line, Reason: finding.Reason})
					continue
				}
				rule, _, _ := strings.Cut(finding.Reason, ":")
				addCulprit(checkCulprits, row.Member, fmt.Sprintf("check %s: %s %s (%s)", name, rule, finding.Declaration, row.Label))
				verdict.Refused = append(verdict.Refused, Refusal{Member: row.Member, Declaration: finding.Declaration, Kind: finding.Kind, Path: finding.Path, Line: finding.Line, Strength: row.Strength, Evidence: row.Label, Reason: finding.Reason})
			}
		}
		switch {
		case len(unattributed) != 0:
			verdict.Verdict, verdict.Reason = "unable", "no member selected for "+strings.Join(unattributed, "; ")
			verdict.Refused, verdict.Flags = nil, nil
		case len(checkCulprits) != 0:
			verdict.Verdict, verdict.Reason = "fail", verdict.Refused[0].Reason
			for member, reason := range checkCulprits {
				addCulprit(culprits, member, reason)
			}
		case len(verdict.Flags) != 0:
			verdict.Verdict, verdict.Reason = "flag", verdict.Flags[0].Reason
		default:
			verdict.Verdict = "pass"
		}
		verdicts = append(verdicts, verdict)
		if len(culprits) != 0 && !every {
			break
		}
	}
	return verdicts, culprits, nil
}

// CheckReport is what CheckCandidate found: the frozen base, the candidate
// tree, the members in it and every check's verdict.
type CheckReport struct {
	Base    string         `json:"base"`
	Tree    string         `json:"tree"`
	Members []string       `json:"members"`
	Checks  []CheckVerdict `json:"checks"`
}

// CheckCandidate freezes the input, builds its candidate and runs every
// configured check on it. It builds nothing, returns no member and saves
// nothing. A member the candidate itself drops (a conflict) is left out of
// the members. A check error other than a CheckUnableError is returned.
func (r Runner) CheckCandidate(ctx context.Context, input Input) (CheckReport, error) {
	input, err := r.freezeInput(ctx, input)
	if err != nil {
		return CheckReport{}, err
	}
	if err := r.readChanges(ctx, input.Repository, input.Base, input.Members); err != nil {
		return CheckReport{}, err
	}
	candidate, err := r.candidate(ctx, input.Repository, input.Base, input.Members)
	if err != nil {
		return CheckReport{}, fmt.Errorf("build candidate: %w", err)
	}
	batch := Batch{ID: input.ID, Repository: input.Repository, Base: input.Base, FrozenMembers: input.Members, Candidate: candidate,
		ActiveMembers: candidateMembers(Batch{ActiveMembers: cloneMembers(input.Members), Candidate: candidate})}
	verdicts, _, err := r.evaluateChecks(ctx, &batch, true)
	if err != nil {
		return CheckReport{}, err
	}
	report := CheckReport{Base: batch.Base, Tree: candidate.Tree, Members: make([]string, 0, len(batch.ActiveMembers)), Checks: verdicts}
	for _, member := range batch.ActiveMembers {
		report.Members = append(report.Members, member.Migration)
	}
	return report, nil
}

// addCulprit records a member's first reason.
func addCulprit(culprits map[string]string, member, reason string) {
	if _, found := culprits[member]; !found {
		culprits[member] = reason
	}
}

// checkAttribution reads, once per evaluation and only when a graph finding
// needs them, the active members' declaration evidence and the declarations
// of the base and candidate trees.
type checkAttribution struct {
	runner    Runner
	batch     *Batch
	loaded    bool
	evidence  map[string]blame.Evidence
	base      []blame.Declaration
	candidate []blame.Declaration
}

// selectFor attributes one graph finding through blame.SelectFor.
func (a *checkAttribution) selectFor(ctx context.Context, finding Finding) (blame.Selection, error) {
	if !a.loaded {
		base, err := a.runner.candidate(ctx, a.batch.Repository, a.batch.Base, nil)
		if err != nil {
			return blame.Selection{}, fmt.Errorf("build base candidate: %w", err)
		}
		// The trees are read first: their candidate names the generated
		// instance names that no declaration may keep, and a member's
		// evidence must be named by the same set as the trees.
		baseDeclarations, candidateDeclarations, ambiguous, err := a.runner.treeDeclarations(ctx, a.batch, Tree(base.Tree))
		if err != nil {
			return blame.Selection{}, err
		}
		evidence, err := a.runner.memberEvidence(ctx, a.batch, ambiguous)
		if err != nil {
			return blame.Selection{}, err
		}
		a.evidence, a.base, a.candidate, a.loaded = evidence, baseDeclarations, candidateDeclarations, true
	}
	lines := blame.TreeLines{Base: blame.DeclarationsFor(finding.Declaration, a.base), Candidate: blame.DeclarationsFor(finding.Declaration, a.candidate)}
	return blame.SelectFor(blame.Name{Value: finding.Declaration, Kind: finding.Kind}, a.evidence, lines), nil
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

// build builds the candidate and keeps its directory for the hasher. Every
// result it does not hand back is released here; the caller releases the one
// it gets.
func (r Runner) build(ctx context.Context, batch *Batch) (KeptBuild, bool, error) {
	for attempt := 0; attempt <= r.infraLimit(); attempt++ {
		if err := r.transition(ctx, batch, Building, ""); err != nil {
			return KeptBuild{Release: noRelease}, false, err
		}
		build, err := r.Builder.BuildKept(ctx, Tree(batch.Candidate.Tree))
		if build.Release == nil {
			build.Release = noRelease
		}
		if err != nil {
			build.Release()
			if attempt == r.infraLimit() {
				return KeptBuild{Release: noRelease}, true, nil
			}
			continue
		}
		if build.Result.InfraClass == "" {
			return build, false, nil
		}
		build.Release()
		if attempt == r.infraLimit() {
			return KeptBuild{Result: build.Result, Release: noRelease}, true, nil
		}
	}
	return KeptBuild{Release: noRelease}, true, nil
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
	report := blame.Parse(output, r.ModuleRoots)
	// The round's trees are read first: their candidate names the generated
	// instance names that no declaration may keep, and a member's evidence
	// must be named by the same set as the trees.
	lines, ambiguous, err := r.declarationLines(ctx, batch, Tree(base.Tree), report)
	if err != nil {
		return false, nil, err
	}
	evidence, err := r.memberEvidence(ctx, batch, ambiguous)
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

// memberEvidence reads each active member's declaration evidence, from its
// comparison base (the batch base when it has none) to its head, naming the
// declarations by the round's ambiguous generated instance names.
func (r Runner) memberEvidence(ctx context.Context, batch *Batch, ambiguous blame.InstanceNames) (map[string]blame.Evidence, error) {
	evidence := make(map[string]blame.Evidence, len(batch.ActiveMembers))
	for _, member := range batch.ActiveMembers {
		comparisonBase := member.ComparisonBase
		if comparisonBase == "" {
			comparisonBase = batch.Base
		}
		memberEvidence, err := r.Git.DeclarationEvidence(ctx, batch.Repository, comparisonBase, member, ambiguous)
		if err != nil {
			return nil, fmt.Errorf("read declaration evidence for %q: %w", member.Migration, err)
		}
		evidence[member.Migration] = memberEvidence
	}
	return evidence, nil
}

// treeDeclarations reads every declaration of the base tree and of the
// batch's candidate tree. It also returns the round's ambiguous generated
// instance names, which the candidate tree decides and which it has resolved
// in both trees: every scan of the round must use that same set, or one
// declaration would carry two names.
func (r Runner) treeDeclarations(ctx context.Context, batch *Batch, base Tree) ([]blame.Declaration, []blame.Declaration, blame.InstanceNames, error) {
	baseDeclarations, err := r.Git.TreeDeclarations(ctx, batch.Repository, base)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read declarations at base: %w", err)
	}
	candidateDeclarations, err := r.Git.TreeDeclarations(ctx, batch.Repository, Tree(batch.Candidate.Tree))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read declarations at candidate: %w", err)
	}
	ambiguous := blame.AmbiguousInstanceNames(candidateDeclarations)
	blame.ResolveInstanceNames(candidateDeclarations, ambiguous)
	blame.ResolveInstanceNames(baseDeclarations, ambiguous)
	return baseDeclarations, candidateDeclarations, ambiguous, nil
}

// declarationLines groups, by each name the report mentions, the declarations
// of the round's trees that carry it, and returns the round's ambiguous
// generated instance names with them.
func (r Runner) declarationLines(ctx context.Context, batch *Batch, base Tree, report blame.Report) (map[string]blame.TreeLines, blame.InstanceNames, error) {
	baseDeclarations, candidateDeclarations, ambiguous, err := r.treeDeclarations(ctx, batch, base)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[string]struct{})
	for _, block := range report.Blocks {
		for _, name := range blame.Names(block.Message) {
			names[name.Value] = struct{}{}
		}
	}
	declarations := make(map[string]blame.TreeLines, len(names))
	for name := range names {
		declarations[name] = blame.TreeLines{
			Base:      blame.DeclarationsFor(name, baseDeclarations),
			Candidate: blame.DeclarationsFor(name, candidateDeclarations),
		}
	}
	return declarations, ambiguous, nil
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
		Key:               r.key(batch),
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
	if err := r.Store.Save(ctx, batch); err != nil {
		return fmt.Errorf("store %s state: %w", state, err)
	}
	return nil
}

// policyDigest identifies the checking policy: the configuration digest and the
// configured checks, by name.
func (r Runner) policyDigest() string {
	names := make([]string, len(r.Checks))
	for index, checker := range r.Checks {
		names[index] = checkName(checker, index)
	}
	sort.Strings(names)
	body, _ := json.Marshal(struct {
		Configuration string   `json:"configuration"`
		Checks        []string `json:"checks"`
	}{Configuration: r.ConfigDigest, Checks: names})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func checkName(checker Checker, index int) string {
	if name := checker.Name(); name != "" {
		return name
	}
	return fmt.Sprintf("check-%d", index+1)
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
		out[index].ChangedContracts = cloneHashes(member.ChangedContracts)
		out[index].DerivedContracts = cloneHashes(member.DerivedContracts)
		out[index].Reviews = append([]Review(nil), member.Reviews...)
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
func (s *MemoryStore) Create(_ context.Context, batch *Batch) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches == nil {
		s.batches = make(map[string]Batch)
	}
	if stored, found := s.batches[batch.ID]; found {
		batch.RecordVersion = stored.RecordVersion
		return false, nil
	}
	batch.SchemaVersion, batch.RecordVersion = BatchSchemaVersion, 1
	s.batches[batch.ID] = cloneBatch(*batch)
	return true, nil
}

// Save writes a deep copy of a batch.
func (s *MemoryStore) Save(_ context.Context, batch *Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batches == nil {
		s.batches = make(map[string]Batch)
	}
	batch.SchemaVersion, batch.RecordVersion = BatchSchemaVersion, batch.RecordVersion+1
	s.batches[batch.ID] = cloneBatch(*batch)
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
		if !batch.State.terminal() || (batch.State == Declined && batch.Successor != "" && !s.has(batch.Successor)) {
			out = append(out, cloneBatch(batch))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// has reports whether the store holds the batch id. The caller holds the lock.
func (s *MemoryStore) has(id string) bool {
	_, found := s.batches[id]
	return found
}

func cloneBatch(batch Batch) Batch {
	batch.FrozenMembers = cloneMembers(batch.FrozenMembers)
	batch.ActiveMembers = cloneMembers(batch.ActiveMembers)
	batch.PostLandEffects = append([]PostLandEffect(nil), batch.PostLandEffects...)
	batch.PendingEffects = append([]string(nil), batch.PendingEffects...)
	batch.Dropped = append([]DroppedMember(nil), batch.Dropped...)
	batch.Candidate.Dropped = append([]gitx.Drop(nil), batch.Candidate.Dropped...)
	batch.InitialCandidate.Dropped = append([]gitx.Drop(nil), batch.InitialCandidate.Dropped...)
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
