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

// specAnswer is one item that a spec revision answers, and the edge it offers, when it offers one.
type specAnswer struct {
	item string
	open bool
	edge string
}

// offer writes the owner's spec revision n. The revision answers each item of answers:
//   - a need to the owner: it answers that need, and the source gets offer:<edge>@r<n>;
//   - a reply about an edge whose target is the owner: it answers the reply, lists it in resolves, and the source
//     gets offer:<edge>@r<n> for the edge's current need;
//   - a reply that names the owner its premise owner: it answers the reply and lists it in resolves.
//
// The revision's commit carries an Owner-Item trailer for each open answered item and for the owner's open start
// item, and each completes with that commit.
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

	var items []specAnswer
	offers := map[string]offered{}
	var edges []string
	offer := func(edge, source string) error {
		if _, ok := offers[edge]; ok {
			return nil
		}
		needData, _, err := s.needAt(source, p.ID)
		if err != nil {
			return err
		}
		offers[edge] = offered{edge: edge, source: source, needHash: ownerpkg.ContentHash(needData)}
		edges = append(edges, edge)
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
		item := specAnswer{item: answer, open: current.State == "open"}
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
			item.edge = ref.Subject
		case source == p.ID:
			return nil, refusef("%s is a reply about %s's own need to %s; answer it with owner need %s <file> --answers %s", answer, p.ID, target, target, answer)
		case target == p.ID:
			if err := offer(ref.Subject, source); err != nil {
				return nil, err
			}
			item.edge = ref.Subject
			resolves = appendNew(resolves, answer)
		default: // the owner is the premise owner the reply names
			resolves = appendNew(resolves, answer)
		}
		items = append(items, item)
	}
	versions := []string{}
	for _, edge := range edges {
		versions = append(versions, "need:"+edge+"@"+offers[edge].needHash)
	}

	recordPath := ownerpkg.SpecRecord(s.config.RecordsRoot, p.ID)
	previous, found, err := p.tree.BlobAtHead(recordPath)
	if err != nil {
		return nil, err
	}
	front := ownerpkg.SpecFront{Owner: p.ID, Epoch: p.epoch, Complete: draft.Complete, Answers: versions, Resolves: resolves, AuthorModel: draft.AuthorModel}
	var prevFront ownerpkg.SpecFront
	var prevClaims []ownerpkg.Claim
	if found {
		if prevFront, prevClaims, err = ownerpkg.ParseSpec(previous); err != nil {
			return nil, fmt.Errorf("read the previous spec %s: %w", recordPath, err)
		}
	}
	if lint := ownerpkg.LintClaims(draft.Claims, prevFront.Retired); len(lint) > 0 {
		return nil, refusef("spec claim tree: %v", lint[0])
	}
	front.Retired = ownerpkg.RetiredIDs(prevFront.Retired, prevClaims, draft.Claims)
	front.Revision = 1
	if found {
		front.Revision = prevFront.Revision
		if same, err := marshalSpec(front, draft.Body); err != nil || !bytes.Equal(same, previous) {
			front.Revision = prevFront.Revision + 1
		}
	}
	data, err := marshalSpec(front, draft.Body)
	if err != nil {
		return nil, err
	}
	if _, _, err := ownerpkg.ParseSpec(data); err != nil {
		return nil, fmt.Errorf("the spec revision does not parse: %w", err)
	}
	specHash := ownerpkg.ContentHash(data)
	linked, err := s.changesLinkedClaim(p, draft.Claims, prevClaims)
	if err != nil {
		return nil, err
	}
	reviewers, err := s.reviewers(p.ID, "spec", front.Revision)
	if err != nil {
		return nil, err
	}

	var open []specAnswer
	var answeredItems []string
	for _, item := range items {
		answeredItems = append(answeredItems, item.item)
		if item.open {
			open = append(open, item)
		}
	}
	result := stepResult{Answered: answeredItems, Record: recordPath, Revision: front.Revision}
	subject := fmt.Sprintf("owner: spec r%d", front.Revision)
	if len(items) > 0 && len(open) == 0 {
		// Every answered item is done. A run that repeats the revision that answered them repeats its result. A
		// changed draft would be a new revision that answers none of them, so it is refused, never dropped.
		if !found || !bytes.Equal(data, previous) {
			return nil, refusef("%s: every --answers item is done, and this draft differs from the current spec r%d; "+
				"to write it as a new revision, run owner offer %s without the done items", strings.Join(answeredItems, ", "), prevFront.Revision, file)
		}
		result.AlreadyDone = true
		result.Commit, err = s.lastCommit(branch(p.ID), recordPath)
		return result, err
	}
	// A spec revision is the owner's first spec while its start item is open, so it also answers that item.
	start := "start:" + p.ID + "@1"
	answerStart := false
	switch item, err := store.Item(s.ctx, s.repo, start); {
	case isNotFound(err):
	case err != nil:
		return nil, err
	default:
		answerStart = item.State == "open" && item.Owner == p.ID
	}
	// The revision is the record of every open item it answers and of the open start item, so its commit carries an
	// Owner-Item trailer for each, and each of those items completes with that commit.
	var attributed []string
	for _, item := range open {
		attributed = append(attributed, item.item)
		if err := s.checkNotHeldByOther(store, item.item); err != nil {
			return nil, err
		}
	}
	if answerStart {
		attributed = append(attributed, start)
	}
	if _, err := p.tree.CommitFile(s.ctx, recordPath, data, subject, map[string][]string{"Owner-Item": attributed}); err != nil {
		return nil, err
	}
	commit, err := p.tree.LastCommitTouching(recordPath)
	if err != nil {
		return nil, err
	}
	result.Commit = commit
	models := map[string]any{"author": draft.AuthorModel, "reviewers": reviewers}

	// One step for each open answered item. The first open item of each offered edge queues the edge's offer for
	// its source and publishes its notice.
	var steps []ownerpkg.Step
	queued := map[string]bool{}
	for _, item := range open {
		step := ownerpkg.Step{Answers: item.item, Path: recordPath, Data: data, Subject: subject}
		if item.edge != "" && !queued[item.edge] {
			queued[item.edge] = true
			edge := offers[item.edge]
			offerItem := fmt.Sprintf("offer:%s@r%d", edge.edge, front.Revision)
			step.FollowUps = []ownerpkg.FollowUp{{Item: offerItem, Owner: edge.source}}
			step.Notices = ownerpkg.Notices(p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteOffer, Source: edge.source, Target: p.ID, TargetKind: p.Kind, Linked: linked}), ownerpkg.NoticeFields{
				Type: "offer", Epoch: p.epoch, From: p.ID, To: edge.source, Edge: edge.edge, Item: offerItem, Record: recordPath, Commit: commit,
				Versions: map[string]any{"need": edge.needHash, "offer": specHash}, Models: models,
			})
			result.FollowUps = append(result.FollowUps, offerItem)
		}
		steps = append(steps, step)
	}
	if answerStart {
		steps = append(steps, ownerpkg.Step{Answers: start, Path: recordPath, Data: data, Subject: subject})
		result.Answered = append(result.Answered, start)
	}
	if len(queued) == 0 {
		// The revision offers no edge: its notice goes to the users of the owner's spec, on the first step, or on a
		// step that answers nothing. It queues no item for them, so it names none.
		notices := ownerpkg.Notices(p.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteOffer, Target: p.ID, TargetKind: p.Kind, Linked: linked}), ownerpkg.NoticeFields{
			Type: "offer", Epoch: p.epoch, From: p.ID, Version: fmt.Sprintf("r%d", front.Revision), Record: recordPath, Commit: commit,
			Versions: map[string]any{"offer": specHash}, Models: models,
		})
		if len(steps) == 0 {
			steps = append(steps, ownerpkg.Step{Path: recordPath, Data: data, Subject: subject})
		}
		steps[0].Notices = notices
	}
	for _, step := range steps {
		outcome, err := s.runStep(p, step)
		if err != nil {
			return nil, err
		}
		result.Undelivered = append(result.Undelivered, outcome.Undelivered...)
	}
	return result, nil
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
