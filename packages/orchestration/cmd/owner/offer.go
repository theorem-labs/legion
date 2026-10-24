package main

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// offered is one edge that a spec revision offers to its source: the edge's current need, by its hash.
type offered struct {
	edge, source, needHash string
}

// offer writes the owner's spec revision n, as one step. The revision answers each item of answers:
//   - a need to the owner: it answers that need, and the source gets offer:<edge>@r<n>;
//   - a reply about an edge whose target is the owner: it answers the reply, lists it in resolves, and the source
//     gets offer:<edge>@r<n> for the edge's current need;
//   - a reply that names the owner its premise owner: it answers the reply and lists it in resolves.
//
// The revision lists each edge's need in answers and offers it to the need's source, also when the item is done
// already: a later revision offers itself to every source it lists, so a source whose offer it supersedes gets the
// new one. The revision's commit carries an Owner-Item trailer for each open answered item and for the owner's open
// start item, and each completes with that commit. When every answered item is done and the draft is the revision
// that answered them, the command repeats that revision.
func (s *session) offer(file string, answers []string) (any, error) {
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	draft, err := readSpecDraft(file, p.ID)
	if err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}

	var edges []offered
	offer := func(edge, source string) error {
		for _, o := range edges {
			if o.edge == edge {
				return nil
			}
		}
		needData, _, err := s.needAt(source, p.ID)
		if err != nil {
			return err
		}
		edges = append(edges, offered{edge: edge, source: source, needHash: ownerpkg.ContentHash(needData)})
		return nil
	}
	resolves := draft.Resolves
	for _, answer := range answers {
		ref, err := ownerpkg.ParseItemID(answer)
		if err != nil || (ref.Type != "need" && ref.Type != "reply") {
			return nil, usagef("--answers takes need and reply items, not %q", answer)
		}
		source, target, err := ownerpkg.ParseEdgeID(ref.Subject)
		if err != nil {
			return nil, err
		}
		current, err := store.Item(s.ctx, s.repo, answer)
		if err != nil {
			return nil, err
		}
		if current.Owner != p.ID {
			return nil, refusef("%s is in the queue of %s, not of %s", answer, current.Owner, p.ID)
		}
		switch {
		case ref.Type == "need":
			if target != p.ID {
				return nil, refusef("%s is a need to %s, not to %s", answer, target, p.ID)
			}
			_, needFront, err := s.needAt(source, p.ID)
			if err != nil {
				return nil, err
			}
			revision, _ := ownerpkg.ParseRevision(ref.Version)
			if needFront.Revision > revision {
				return nil, refusef("%s is superseded by r%d; run owner done %s --superseded r%d", answer, needFront.Revision, answer, needFront.Revision)
			}
			if needFront.Revision < revision {
				return nil, fmt.Errorf("%s names r%d, but %s's need record is r%d", answer, revision, source, needFront.Revision)
			}
			if err := offer(ref.Subject, source); err != nil {
				return nil, err
			}
		case source == p.ID:
			return nil, refusef("%s is a reply about %s's own need to %s; answer it with owner need %s <file> --answers %s", answer, p.ID, target, target, answer)
		case target == p.ID:
			if err := offer(ref.Subject, source); err != nil {
				return nil, err
			}
			resolves = appendNew(resolves, answer)
		default: // the owner is the premise owner the reply names
			resolves = appendNew(resolves, answer)
		}
	}
	versions := []string{}
	for _, edge := range edges {
		versions = append(versions, "need:"+edge.edge+"@"+edge.needHash)
	}

	// A spec revision is the owner's first spec while its start item is open, so it also answers that item.
	answered := append([]string(nil), answers...)
	start := "start:" + p.ID + "@1"
	switch item, err := store.Item(s.ctx, s.repo, start); {
	case isNotFound(err):
	case err != nil:
		return nil, err
	case item.State == "open" && item.Owner == p.ID:
		answered = append(answered, start)
	}

	recordPath := ownerpkg.SpecRecord(s.config.RecordsRoot, p.ID)
	result := stepResult{Answered: answered, Record: recordPath}
	build := func(previous []byte, found bool) (ownerpkg.Record, error) {
		front := ownerpkg.SpecFront{Owner: p.ID, Epoch: p.epoch, Complete: draft.Complete, Answers: versions, Resolves: resolves, AuthorModel: draft.AuthorModel, Revision: 1}
		var prevFront ownerpkg.SpecFront
		var prevClaims []ownerpkg.Claim
		if found {
			var err error
			if prevFront, prevClaims, err = ownerpkg.ParseSpec(previous); err != nil {
				return ownerpkg.Record{}, fmt.Errorf("read the previous spec %s: %w", recordPath, err)
			}
		}
		if lint := ownerpkg.LintClaims(draft.Claims, prevFront.Retired); len(lint) > 0 {
			return ownerpkg.Record{}, refusef("spec claim tree: %v", lint[0])
		}
		front.Retired = ownerpkg.RetiredIDs(prevFront.Retired, prevClaims, draft.Claims)
		if found {
			front.Revision = prevFront.Revision
			if same, err := marshalSpec(front, draft.Body); err != nil || !bytes.Equal(same, previous) {
				front.Revision = prevFront.Revision + 1
			}
		}
		data, err := marshalSpec(front, draft.Body)
		if err != nil {
			return ownerpkg.Record{}, err
		}
		if _, _, err := ownerpkg.ParseSpec(data); err != nil {
			return ownerpkg.Record{}, fmt.Errorf("the spec revision does not parse: %w", err)
		}
		specHash := ownerpkg.ContentHash(data)
		linked, err := s.changesLinkedClaim(p, draft.Claims, prevClaims)
		if err != nil {
			return ownerpkg.Record{}, err
		}
		reviewers, err := s.reviewers(p.ID, "spec", front.Revision)
		if err != nil {
			return ownerpkg.Record{}, err
		}
		models := map[string]any{"author": draft.AuthorModel, "reviewers": reviewers}
		result.Revision = front.Revision

		var followUps []ownerpkg.FollowUp
		offerItems := make([]string, len(edges))
		for index, edge := range edges {
			offerItems[index] = fmt.Sprintf("offer:%s@r%d", edge.edge, front.Revision)
			followUps = append(followUps, ownerpkg.FollowUp{Item: offerItems[index], Owner: edge.source})
		}
		result.FollowUps = offerItems
		notices := func(commit string) []ownerpkg.Notice {
			if len(edges) == 0 {
				// The revision offers no edge: its notice goes to the users of the owner's spec. It queues no item for
				// them, so it names none.
				return ownerpkg.Notices(p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteOffer, Target: p.ID, TargetKind: p.Kind, Linked: linked}), ownerpkg.NoticeFields{
					Type: "offer", Epoch: p.epoch, From: p.ID, Version: fmt.Sprintf("r%d", front.Revision), Record: recordPath, Commit: commit,
					Versions: map[string]any{"offer": specHash}, Models: models,
				})
			}
			var out []ownerpkg.Notice
			for index, edge := range edges {
				out = append(out, ownerpkg.Notices(p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteOffer, Source: edge.source, Target: p.ID, TargetKind: p.Kind, Linked: linked}), ownerpkg.NoticeFields{
					Type: "offer", Epoch: p.epoch, From: p.ID, To: edge.source, Edge: edge.edge, Item: offerItems[index], Record: recordPath, Commit: commit,
					Versions: map[string]any{"need": edge.needHash, "offer": specHash}, Models: models,
				})...)
			}
			return out
		}
		return ownerpkg.Record{Data: data, Subject: fmt.Sprintf("owner: spec r%d", front.Revision), FollowUps: followUps, Notices: notices}, nil
	}
	outcome, err := p.runner.Run(s.ctx, ownerpkg.Step{Path: recordPath, Answers: answered, Build: build})
	if err != nil {
		return nil, err
	}
	return result.of(outcome), nil
}

// changesLinkedClaim reports whether a data-structure or subsystem revision adds or changes a claim that uses a
// node its structure links join to it; such a revision also goes on node.<id>.linked.
func (s *session) changesLinkedClaim(p party, claims, previous []ownerpkg.Claim) (bool, error) {
	if p.Kind == "function" || p.Kind == "fact" || p.Kind == "role" {
		return false, nil
	}
	index, err := s.graphIndex()
	if err != nil {
		return false, err
	}
	linked := map[string]bool{}
	for _, id := range index.Linked(p.ID) {
		linked[id] = true
	}
	before := map[string]ownerpkg.Claim{}
	for _, claim := range previous {
		before[claim.ID] = claim
	}
	for _, claim := range claims {
		if old, ok := before[claim.ID]; ok && reflect.DeepEqual(old, claim) {
			continue
		}
		for _, use := range claim.Uses {
			if node, _, found := strings.Cut(use, "#"); found && linked[node] {
				return true, nil
			}
		}
	}
	return false, nil
}
