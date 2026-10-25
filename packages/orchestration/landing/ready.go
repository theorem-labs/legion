package landing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// defaultMaxMembers caps a batch formed from the ready list when the configuration sets no cap.
const defaultMaxMembers = 16

// ErrNoReadyMembers says that the repository has no ready migration, so there is no batch to form now.
var ErrNoReadyMembers = fmt.Errorf("no migration is ready")

// ReadyInput is the next batch to form from the ledger: the repository's ready migrations in ready order, capped at
// maxMembers, on the current tip of the base branch. A serving program never has a configured batch: its members
// come from the ledger, each frozen at the ready record the ledger holds for it, and its base is where the branch
// stands now. The id is derived from the base and the frozen members, so a crash before anything was stored forms
// the same batch again instead of a second one.
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
	return Input{
		ID:         readyBatchID(base, members),
		Repository: repository,
		Base:       base,
		BaseBranch: baseBranch,
		Members:    members,
	}, nil
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
