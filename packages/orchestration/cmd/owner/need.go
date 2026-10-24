package main

import (
	"bytes"
	"fmt"
	"slices"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// need writes the owner's need to target, a new revision when its bytes change, queues need:<edge>@r<n> for the
// target and publishes its notice. The revision answers each reply item of answers, which the target sent about
// this need: it lists them in resolves, its commit carries their Owner-Item trailers, and each completes with it.
func (s *session) need(target, file string, outside bool, answers []string) (any, error) {
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	kind, err := s.targetKind(target)
	if err != nil {
		return nil, err
	}
	if target == p.ID {
		return nil, refusef("an owner sends no need to itself")
	}
	active, err := s.active()
	if err != nil {
		return nil, err
	}
	inPart := kind == "" || active[target]
	switch {
	case !inPart && !outside:
		return nil, refusef("%s is outside the active set %s; record the edge with --outside-part", target, s.config.ActiveFile)
	case inPart && outside:
		return nil, refusef("%s is in the active set, so the edge is not outside the part", target)
	}
	draft, err := readNeedDraft(file)
	if err != nil {
		return nil, err
	}
	edge := ownerpkg.EdgeID(p.ID, target)
	replies, err := s.answeredReplies(p, edge, answers)
	if err != nil {
		return nil, err
	}
	targetSpec, _, targetClaims, specFound, err := s.spec(target)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, claim := range targetClaims {
		known[claim.ID] = true
	}
	for _, cite := range draft.Cites {
		if !known[cite] {
			return nil, refusef("the need cites %s, which %s's current spec does not have", cite, target)
		}
	}

	resolves := draft.Resolves
	for _, reply := range replies {
		resolves = appendNew(resolves, reply.item)
	}
	recordPath := ownerpkg.NeedRecord(s.config.RecordsRoot, p.ID, target)
	front := ownerpkg.NeedFront{
		Edge: edge, Source: p.ID, Target: target, Origin: ownerpkg.OriginNeighbor, Epoch: p.epoch,
		Cites: draft.Cites, Resolves: resolves, AuthorModel: draft.AuthorModel,
	}
	previous, found, err := p.tree.BlobAtHead(recordPath)
	if err != nil {
		return nil, err
	}
	var prevFront ownerpkg.NeedFront
	var prevClaims []ownerpkg.Claim
	front.Revision = 1
	if found {
		var prevBody []byte
		if prevFront, prevBody, err = parseNeedRecord(previous); err != nil {
			return nil, fmt.Errorf("read the previous need %s: %w", recordPath, err)
		}
		if prevClaims, err = parseClaimTree(prevBody); err != nil {
			return nil, fmt.Errorf("read the previous need %s: %w", recordPath, err)
		}
	}
	if lint := ownerpkg.LintClaims(draft.Claims, prevFront.Retired); len(lint) > 0 {
		return nil, refusef("need claim tree: %v", lint[0])
	}
	front.Retired = ownerpkg.RetiredIDs(prevFront.Retired, prevClaims, draft.Claims)
	if found {
		front.Revision = prevFront.Revision
		if same, err := marshalNeedRecord(front, draft.Body); err != nil || !bytes.Equal(same, previous) {
			front.Revision = prevFront.Revision + 1
		}
	}
	data, err := marshalNeedRecord(front, draft.Body)
	if err != nil {
		return nil, err
	}
	needHash := ownerpkg.ContentHash(data)

	if outside {
		if _, err := p.tree.CommitFile(s.ctx, recordPath, data, "owner: need to "+target+" outside the part", nil); err != nil {
			return nil, err
		}
		edgePath := s.config.Paths.EdgeRecord(p.ID, target)
		record := ownerpkg.EdgeRecord{Edge: edge, Source: p.ID, Target: target, Status: "outside_part", Need: ownerpkg.EdgeNeed{Path: recordPath, Hash: needHash}}
		edgeData, err := record.Marshal()
		if err != nil {
			return nil, err
		}
		commit, err := p.tree.CommitFile(s.ctx, edgePath, edgeData, "owner: edge to "+target+" is outside the part", nil)
		if err != nil {
			return nil, err
		}
		return stepResult{Record: edgePath, Revision: front.Revision, Commit: commit}, nil
	}

	reviewers, err := s.reviewers(p.ID, "need", front.Revision)
	if err != nil {
		return nil, err
	}
	subject := fmt.Sprintf("owner: need to %s r%d", target, front.Revision)
	var attributed []string
	for _, reply := range replies {
		if reply.open {
			attributed = append(attributed, reply.item)
		}
	}
	if _, err := p.tree.CommitFile(s.ctx, recordPath, data, subject, map[string][]string{"Owner-Item": attributed}); err != nil {
		return nil, err
	}
	commit, err := p.tree.LastCommitTouching(recordPath)
	if err != nil {
		return nil, err
	}
	item := fmt.Sprintf("need:%s@r%d", edge, front.Revision)
	var offerVersion any
	if specFound {
		offerVersion = ownerpkg.ContentHash(targetSpec)
	}
	notices := ownerpkg.Notices(p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteNeed, Source: p.ID, Target: target, TargetKind: kind}), ownerpkg.NoticeFields{
		Type: "need", Epoch: p.epoch, From: p.ID, To: target, Edge: edge, Item: item, Record: recordPath, Commit: commit,
		Versions: map[string]any{"need": needHash, "offer": offerVersion},
		Models:   map[string]any{"author": draft.AuthorModel, "reviewers": reviewers},
	})
	// The need's own step answers no item: it queues the need for the target and publishes its notice, journaling
	// it when the listener refuses it. Then each reply the revision answers completes with its commit.
	outcome, err := s.runStep(p, ownerpkg.Step{
		Path: recordPath, Data: data, Subject: subject,
		FollowUps: []ownerpkg.FollowUp{{Item: item, Owner: target}}, Notices: notices,
	})
	if err != nil {
		return nil, err
	}
	result := stepResult{Item: item, Record: recordPath, Revision: front.Revision, Commit: commit, FollowUps: []string{item}, Undelivered: outcome.Undelivered}
	for _, reply := range replies {
		if _, err := s.runStep(p, ownerpkg.Step{Answers: reply.item, Path: recordPath, Data: data, Subject: subject}); err != nil {
			return nil, err
		}
		result.Answered = append(result.Answered, reply.item)
	}
	return result, nil
}

// answeredReply is a reply item that a revision answers, and whether it is still open.
type answeredReply struct {
	item string
	open bool
}

// answeredReplies checks the reply items a need revision of edge answers: each is a reply about edge, in the
// queue of the edge's source, which is the session's owner.
func (s *session) answeredReplies(p party, edge string, answers []string) ([]answeredReply, error) {
	if len(answers) == 0 {
		return nil, nil
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	var replies []answeredReply
	for _, answer := range answers {
		ref, err := ownerpkg.ParseItemID(answer)
		if err != nil || ref.Type != "reply" {
			return nil, usagef("need --answers takes reply items, not %q", answer)
		}
		if ref.Subject != edge {
			return nil, refusef("%s is a reply about %s, not about the need %s", answer, ref.Subject, edge)
		}
		current, err := store.Item(s.ctx, s.repo, answer)
		if err != nil {
			return nil, err
		}
		if current.Owner != p.ID {
			return nil, refusef("%s is in the queue of %s, not of %s", answer, current.Owner, p.ID)
		}
		if current.State == "open" {
			if err := s.checkNotHeldByOther(store, answer); err != nil {
				return nil, err
			}
		}
		replies = append(replies, answeredReply{item: answer, open: current.State == "open"})
	}
	return replies, nil
}

// appendNew appends value unless values holds it.
func appendNew(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
