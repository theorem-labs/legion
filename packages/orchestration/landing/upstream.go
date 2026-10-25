package landing

import (
	"context"
	"fmt"
)

// upstreamMismatch is the return reason for a member whose head is not the merge its ready record claims. The batch
// cannot carry what it cannot confirm.
const upstreamMismatch = "upstream_mismatch"

// upstreamStaleBase is the return reason for a sync of a base the batch has moved past. The sync is not wrong, only
// behind: it builds its head again, from the base the batch holds.
const upstreamStaleBase = "upstream_stale_base"

// checkUpstream verifies every active member that carries an upstream commit against the repository, before the
// ledger checks the members, and returns the ones the repository does not confirm. It reports whether it returned
// any, which changes the batch's key, so the caller starts again from the candidate. A member without an upstream
// commit is an ordinary migration and is not looked at.
func (r Runner) checkUpstream(ctx context.Context, batch *Batch) (bool, error) {
	drops := make(map[string]string)
	for _, member := range cloneMembers(batch.ActiveMembers) {
		if member.Upstream == "" {
			continue
		}
		reason, err := r.upstreamReason(ctx, batch.Repository, batch.Base, member)
		if err != nil {
			return false, err
		}
		if reason != "" {
			drops[member.Migration] = reason
		}
	}
	if len(drops) == 0 {
		return false, nil
	}
	if err := r.dropMembers(ctx, batch, drops); err != nil {
		return false, err
	}
	return true, nil
}

// upstreamReason is the reason to return the member, and empty while its head H is the sync its ready record
// claims: H has exactly two parents, H^1 is the batch's base, H^2 is the upstream commit of the record, the merge
// of the two is clean, and the tree that merge writes is H's tree. A first parent the base has moved past is a
// stale base, which the sync answers with a new head; everything else is a mismatch, a git that cannot answer
// about the member among it.
func (r Runner) upstreamReason(ctx context.Context, repository, base string, member Member) (string, error) {
	head, err := r.Git.ReadCommit(ctx, repository, member.Head)
	if err != nil {
		return unconfirmed(ctx, err)
	}
	if len(head.Parents) != 2 {
		return upstreamMismatch, nil
	}
	if head.Parents[0] != base {
		behind, err := r.Git.Ancestor(ctx, repository, head.Parents[0], base)
		if err != nil {
			return unconfirmed(ctx, err)
		}
		if behind {
			return upstreamStaleBase, nil
		}
		return upstreamMismatch, nil
	}
	upstream, err := r.Git.Resolve(ctx, repository, member.Upstream)
	if err != nil {
		return unconfirmed(ctx, err)
	}
	if head.Parents[1] != upstream {
		return upstreamMismatch, nil
	}
	merged, err := r.Git.MergeTree(ctx, repository, head.Parents[0], head.Parents[1])
	if err != nil {
		return unconfirmed(ctx, err)
	}
	if merged != head.Tree {
		return upstreamMismatch, nil
	}
	return "", nil
}

// unconfirmed turns a git failure into a mismatch: a head git cannot confirm is one the batch cannot carry. A run
// whose context ended confirmed nothing and returns nobody, so its failure stays an error.
func unconfirmed(ctx context.Context, err error) (string, error) {
	if ctx.Err() != nil {
		return "", fmt.Errorf("check upstream sync: %w", err)
	}
	return upstreamMismatch, nil
}
