package main

import (
	"bytes"
	"fmt"
	"slices"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// need writes the owner's need to target, a new revision when its bytes change, queues need:<edge>@r<n> for the
// target and publishes its notice, as one step. The revision answers each reply item of answers, which the target sent
// about this need: it lists them in resolves, its commit carries their Owner-Item trailers, and each completes with it.
// The step picks the revision from the record it reads under the worktree's commit lock, so two needs of one owner
// are two revisions.
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
	if err := s.checkAnsweredReplies(p, edge, answers); err != nil {
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
	var offerVersion any
	if specFound {
		offerVersion = ownerpkg.ContentHash(targetSpec)
	}

	resolves := draft.Resolves
	for _, reply := range answers {
		resolves = appendNew(resolves, reply)
	}
	recordPath := ownerpkg.NeedRecord(s.config.RecordsRoot, p.ID, target)
	edgePath := s.config.Paths.EdgeRecord(p.ID, target)
	result := stepResult{Record: recordPath, Answered: answers}
	build := func(previous []byte, found bool) (ownerpkg.Record, error) {
		front := ownerpkg.NeedFront{
			Edge: edge, Source: p.ID, Target: target, Origin: ownerpkg.OriginNeighbor, Epoch: p.epoch,
			Cites: draft.Cites, Resolves: resolves, AuthorModel: draft.AuthorModel, Revision: 1,
		}
		var prevFront ownerpkg.NeedFront
		var prevClaims []ownerpkg.Claim
		if found {
			var prevBody []byte
			var err error
			if prevFront, prevBody, err = parseNeedRecord(previous); err != nil {
				return ownerpkg.Record{}, fmt.Errorf("read the previous need %s: %w", recordPath, err)
			}
			if prevClaims, err = ownerpkg.ParseClaims(prevBody); err != nil {
				return ownerpkg.Record{}, fmt.Errorf("read the previous need %s: %w", recordPath, err)
			}
		}
		if lint := ownerpkg.LintClaims(draft.Claims, prevFront.Retired); len(lint) > 0 {
			return ownerpkg.Record{}, refusef("need claim tree: %v", lint[0])
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
			return ownerpkg.Record{}, err
		}
		needHash := ownerpkg.ContentHash(data)
		result.Revision = front.Revision

		if outside {
			record := ownerpkg.EdgeRecord{Edge: edge, Source: p.ID, Target: target, Status: "outside_part", Need: ownerpkg.EdgeNeed{Path: recordPath, Hash: needHash}}
			edgeData, err := record.Marshal()
			if err != nil {
				return ownerpkg.Record{}, err
			}
			result.Record = edgePath
			return ownerpkg.Record{
				Data: data, Subject: "owner: need to " + target + " outside the part",
				Also: []ownerpkg.File{{Path: edgePath, Data: edgeData}},
			}, nil
		}

		reviewers, err := s.reviewers(p.ID, "need", front.Revision)
		if err != nil {
			return ownerpkg.Record{}, err
		}
		item := fmt.Sprintf("need:%s@r%d", edge, front.Revision)
		result.Item, result.FollowUps = item, []string{item}
		route := p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteNeed, Source: p.ID, Target: target, TargetKind: kind})
		return ownerpkg.Record{
			Data: data, Subject: fmt.Sprintf("owner: need to %s r%d", target, front.Revision),
			FollowUps: []ownerpkg.FollowUp{{Item: item, Owner: target}},
			Notices: func(commit string) []ownerpkg.Notice {
				return ownerpkg.Notices(route, ownerpkg.NoticeFields{
					Type: "need", Epoch: p.epoch, From: p.ID, To: target, Edge: edge, Item: item, Record: recordPath, Commit: commit,
					Versions: map[string]any{"need": needHash, "offer": offerVersion},
					Models:   map[string]any{"author": draft.AuthorModel, "reviewers": reviewers},
				})
			},
		}, nil
	}
	outcome, err := p.runner.Run(s.ctx, ownerpkg.Step{Path: recordPath, Answers: answers, Build: build})
	if err != nil {
		return nil, err
	}
	return result.of(outcome), nil
}

// checkAnsweredReplies checks the reply items a need revision of edge answers: each is a reply about edge, in the
// queue of the edge's source, which is the session's owner.
func (s *session) checkAnsweredReplies(p party, edge string, answers []string) error {
	if len(answers) == 0 {
		return nil
	}
	store, err := s.ledger()
	if err != nil {
		return err
	}
	for _, answer := range answers {
		ref, err := ownerpkg.ParseItemID(answer)
		if err != nil || ref.Type != "reply" {
			return usagef("need --answers takes reply items, not %q", answer)
		}
		if ref.Subject != edge {
			return refusef("%s is a reply about %s, not about the need %s", answer, ref.Subject, edge)
		}
		current, err := store.Item(s.ctx, s.repo, answer)
		if err != nil {
			return err
		}
		if current.Owner != p.ID {
			return refusef("%s is in the queue of %s, not of %s", answer, current.Owner, p.ID)
		}
	}
	return nil
}

// appendNew appends value unless values holds it.
func appendNew(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
