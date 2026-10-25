package landing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Heads brings a member's head into the program's repository by its SHA, from where owners push.
type Heads interface {
	FetchHead(ctx context.Context, repository, sha string) error
}

// headUnavailable is the reason a member is returned with when its head cannot be fetched.
const headUnavailable = "head_unavailable"

// defaultMaxMembers caps a batch formed from the ready list when the configuration sets no cap.
const defaultMaxMembers = 16

// ErrNoReadyMembers says that the repository has no ready migration, so there is no batch to form now.
var ErrNoReadyMembers = fmt.Errorf("no migration is ready")

// ReadyInput is the next batch to form from the ledger: the repository's ready migrations in ready order, capped at
// maxMembers, on the current tip of the base branch. A serving program never has a configured batch: its members
// come from the ledger, each frozen at the ready record the ledger holds for it, and its base is where the branch
// stands now. The id is derived from the base and the ready list as read, so a crash before anything was stored
// forms the same batch again instead of a second one.
//
// Each head is fetched by its SHA (Heads). A member whose head cannot be fetched is returned to its owner with
// head_unavailable, at the round key of the batch the ready list names, and the batch forms from the others. When
// none is left, the round forms nothing (ErrNoReadyMembers).
func (r Runner) ReadyInput(ctx context.Context, repository, baseBranch string, maxMembers int) (Input, error) {
	if r.Admission == nil {
		return Input{}, fmt.Errorf("a batch ledger is required to read the ready migrations")
	}
	if r.Git == nil {
		return Input{}, fmt.Errorf("git is required to resolve the base branch")
	}
	members, err := r.Admission.ReadyMembers(ctx, repository)
	if err != nil {
		return Input{}, fmt.Errorf("list the ready migrations of %q: %w", repository, err)
	}
	if len(members) == 0 {
		return Input{}, ErrNoReadyMembers
	}
	if maxMembers <= 0 {
		maxMembers = defaultMaxMembers
	}
	if len(members) > maxMembers {
		members = members[:maxMembers]
	}
	base, err := r.Git.Resolve(ctx, repository, baseBranch)
	if err != nil {
		return Input{}, fmt.Errorf("resolve the base branch %q: %w", baseBranch, err)
	}
	id := readyBatchID(base, members)
	fetched, err := r.fetchHeads(ctx, repository, id, members)
	if err != nil {
		return Input{}, err
	}
	if len(fetched) == 0 {
		return Input{}, ErrNoReadyMembers
	}
	return Input{
		ID:         id,
		Repository: repository,
		Base:       base,
		BaseBranch: baseBranch,
		Members:    fetched,
	}, nil
}

// fetchHeads fetches each member's head by its SHA and returns the members whose head arrived. A member whose head
// cannot be fetched is returned with head_unavailable. Its batch does not exist yet, so the return is the ledger's
// round directly rather than a drop of a stored batch, at the same key a drop would use (<batch>/<member>/<generation>),
// so a crash that forms the batch again replays it. A refusal because the member moved on (stale_generation,
// not_open) completes the return; one because another admitted batch holds the member (batch_admitted) leaves the
// member out of this round only. Without Heads every member is kept as read.
func (r Runner) fetchHeads(ctx context.Context, repository, batch string, members []Member) ([]Member, error) {
	if r.Heads == nil {
		return members, nil
	}
	kept := make([]Member, 0, len(members))
	for _, member := range members {
		fetchErr := r.Heads.FetchHead(ctx, repository, member.Head)
		if fetchErr == nil {
			kept = append(kept, member)
			continue
		}
		if r.Ledger == nil {
			return nil, fmt.Errorf("fetch head %s of %q: %w; and no ledger to return it to", member.Head, member.Migration, fetchErr)
		}
		operation := Operation{Kind: "return", Batch: batch, Repository: repository, Member: member.Migration,
			Generation: member.Generation, Reason: headUnavailable, Detail: fetchErr.Error()}
		if err := r.Ledger.ReturnMember(ctx, roundRequest(operation, member)); err != nil {
			var refusal *LedgerRefusal
			if errors.As(err, &refusal) && (refusal.Reason == "stale_generation" || refusal.Reason == "not_open" || refusal.Reason == "batch_admitted") {
				continue
			}
			return nil, fmt.Errorf("return %q with %s: %w", member.Migration, headUnavailable, err)
		}
	}
	return kept, nil
}

// readyBatchID names a batch by what it is formed from: the base commit and each member's migration, generation
// and ready record. Two rounds that read the same ready list at the same base therefore name the same batch, and a
// round that reads another list names another one.
func readyBatchID(base string, members []Member) string {
	type identity struct {
		Migration  string `json:"migration"`
		Generation int64  `json:"generation"`
		ReadyEvent int64  `json:"ready_event"`
	}
	identities := make([]identity, 0, len(members))
	for _, member := range members {
		identities = append(identities, identity{member.Migration, member.Generation, member.ReadyEvent})
	}
	body, _ := json.Marshal(struct {
		Base    string     `json:"base"`
		Members []identity `json:"members"`
	}{base, identities})
	sum := sha256.Sum256(body)
	return "batch-" + hex.EncodeToString(sum[:])[:16]
}
