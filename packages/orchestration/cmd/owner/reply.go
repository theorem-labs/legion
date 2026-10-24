package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func (s *session) accept(item string) (any, error) {
	ref, err := ownerpkg.ParseItemID(item)
	if err != nil || ref.Type != "offer" {
		return nil, usagef("accept takes an offer item, not %q", item)
	}
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	source, target, err := ownerpkg.ParseEdgeID(ref.Subject)
	if err != nil {
		return nil, err
	}
	if source != p.ID {
		return nil, refusef("%s is an offer to %s, not to %s", item, source, p.ID)
	}
	step, err := s.replyContext(p, item, ref, source, target)
	if err != nil || step.done {
		return step.result, err
	}
	// The offer's version is the spec revision that made it. A later revision supersedes it.
	offered, _ := ownerpkg.ParseRevision(ref.Version)
	switch {
	case !step.specFound || step.specFront.Revision < offered:
		return nil, refusef("%s offers r%d of %s's spec, which %s does not have", item, offered, target, target)
	case step.specFront.Revision > offered:
		return nil, refusef("%s's spec is r%d now; run owner done %s --superseded r%d", target, step.specFront.Revision, item, step.specFront.Revision)
	}
	want := "need:" + ref.Subject + "@" + step.needHash
	if !containsString(step.specFront.Answers, want) {
		return nil, refusef("%s's spec r%d does not answer the current need %s; reply superseded or wait for the new offer", target, step.specFront.Revision, want)
	}
	reply := ownerpkg.Reply{Kind: "accept", Edge: ref.Subject, Answers: map[string]string{"need": step.needHash, "offer": step.specHash}, Epoch: p.epoch, Text: "Accepted."}
	data, err := step.record(reply)
	if err != nil {
		return nil, err
	}
	edgePath := s.config.Paths.EdgeRecord(source, target)
	record := ownerpkg.EdgeRecord{Edge: ref.Subject, Source: source, Target: target}
	if existing, found, err := p.tree.BlobAtHead(edgePath); err != nil {
		return nil, err
	} else if found {
		if err := record.Unmarshal(existing); err != nil {
			return nil, fmt.Errorf("read the edge record %s: %w", edgePath, err)
		}
	}
	record.Status = "accepted"
	record.Need = ownerpkg.EdgeNeed{Path: ownerpkg.NeedRecord(s.config.RecordsRoot, source, target), Hash: step.needHash}
	record.Offer = ownerpkg.EdgeOffer{SpecHash: step.specHash}
	edgeData, err := record.Marshal()
	if err != nil {
		return nil, err
	}
	if _, err := p.tree.CommitFile(s.ctx, edgePath, edgeData, "owner: edge to "+target+" accepted", nil); err != nil {
		return nil, err
	}
	route := ownerpkg.Route{Step: ownerpkg.RouteAccept, Source: source, Target: target, TargetKind: step.targetKind}
	return s.finishReply(p, step, "accept", route, data, nil)
}

func (s *session) reply(item, reason, file, premiseOwner string) (any, error) {
	ref, err := ownerpkg.ParseItemID(item)
	if err != nil || (ref.Type != "need" && ref.Type != "offer") {
		return nil, usagef("reply takes a need or an offer item, not %q", item)
	}
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	m, err := s.manifest()
	if err != nil {
		return nil, err
	}
	rule, ok := m.Reasons[reason]
	if !ok {
		var reasons []string
		for name := range m.Reasons {
			reasons = append(reasons, name)
		}
		sort.Strings(reasons)
		return nil, usagef("reason %q is not one of %s", reason, strings.Join(reasons, ", "))
	}
	source, target, err := ownerpkg.ParseEdgeID(ref.Subject)
	if err != nil {
		return nil, err
	}
	if (ref.Type == "need" && target != p.ID) || (ref.Type == "offer" && source != p.ID) {
		return nil, refusef("%s is not addressed to %s", item, p.ID)
	}
	other := source
	if p.ID == source {
		other = target
	}
	recipients := map[string]bool{}
	escalate := false
	routesToPremise := false
	for _, to := range rule.To {
		switch to {
		case "source", "target", "lean_writer":
			recipients[other] = true
		case "premise_owner":
			routesToPremise = true
			if premiseOwner == "" {
				return nil, usagef("reason %s needs --premise-owner <id>", reason)
			}
			if _, err := s.identityOf(premiseOwner); err != nil {
				return nil, err
			}
			recipients[premiseOwner] = true
		case "escalations":
			escalate = true
		default:
			return nil, unable(fmt.Errorf("%s routes reason %s to %q, which this command does not know", s.config.ManifestFile, reason, to))
		}
	}
	if premiseOwner != "" && !routesToPremise {
		return nil, usagef("--premise-owner applies only to a reason that goes to the premise owner")
	}
	delete(recipients, p.ID)
	if len(recipients) > 1 {
		return nil, fmt.Errorf("reason %s goes to %d owners, but one reply item has one owner", reason, len(recipients))
	}
	draft, err := readReplyDraft(file)
	if err != nil {
		return nil, err
	}
	for _, field := range rule.Requires {
		if strings.TrimSpace(draft.fields[field]) == "" {
			return nil, refusef("reason %s requires %s in the reply's front matter", reason, field)
		}
	}

	step, err := s.replyContext(p, item, ref, source, target)
	if err != nil || step.done {
		return step.result, err
	}
	answers := map[string]string{"need": step.needHash}
	if step.specFound {
		answers["offer"] = step.specHash
	}
	reply := ownerpkg.Reply{Kind: "reply", Edge: ref.Subject, Reason: reason, PremiseOwner: premiseOwner, Answers: answers, Epoch: p.epoch, Text: draft.text}
	data, err := step.record(reply)
	if err != nil {
		return nil, err
	}
	replyID := replyItem(ref.Subject, data)
	var followUps []ownerpkg.FollowUp
	for recipient := range recipients {
		followUps = append(followUps, ownerpkg.FollowUp{Item: replyID, Owner: recipient})
	}
	if escalate {
		escalation := ownerpkg.Escalation{Subject: replyID, Record: step.path, Reason: reason, Counterexample: draft.fields["counterexample"], BugsRow: draft.fields["bugs_row"]}
		escalationData, err := escalation.Marshal()
		if err != nil {
			return nil, refusef("escalation record: %v", err)
		}
		escalationPath := ownerpkg.NodeRecord(s.config.RecordsRoot, p.ID, "escalations", ownerpkg.Hex16(replyID)+".yaml")
		if _, err := p.tree.CommitFile(s.ctx, escalationPath, escalationData, "owner: escalate "+reason+" on "+ref.Subject, nil); err != nil {
			return nil, err
		}
		followUps = append(followUps, ownerpkg.FollowUp{Item: "escalation:" + ownerpkg.Hex16(replyID), Owner: "escalations"})
	}
	row := ownerpkg.RouteReplyFromSource
	if p.ID == target {
		row = ownerpkg.RouteReplyFromTarget
	}
	route := ownerpkg.Route{Step: row, Source: source, Target: target, TargetKind: step.targetKind}
	return s.finishReply(p, step, "reply", route, data, followUps)
}

// replyStep is what accept and reply read before they write: the answered item, the need, the target's spec
// and the record path for this revision's reply.
type replyStep struct {
	item       string
	edge       string
	from, to   string
	targetKind string
	path       string
	existing   []byte
	needHash   string
	needFront  ownerpkg.NeedFront
	specFound  bool
	specHash   string
	specFront  ownerpkg.SpecFront
	done       bool
	result     any
}

func (s *session) replyContext(p party, item string, ref ownerpkg.ItemRef, source, target string) (replyStep, error) {
	store, err := s.ledger()
	if err != nil {
		return replyStep{}, err
	}
	current, err := store.Item(s.ctx, s.repo, item)
	if err != nil {
		return replyStep{}, err
	}
	if current.Owner != p.ID {
		return replyStep{}, refusef("%s is in the queue of %s, not of %s", item, current.Owner, p.ID)
	}
	other := source
	if p.ID == source {
		other = target
	}
	revision, _ := ownerpkg.ParseRevision(ref.Version)
	step := replyStep{item: item, edge: ref.Subject, from: p.ID, to: other,
		path: ownerpkg.ReplyRecord(s.config.RecordsRoot, p.ID, ref.Subject, revision)}
	if current.State == "done" {
		commit, err := s.lastCommit(branch(p.ID), step.path)
		step.done, step.result = true, stepResult{Answered: []string{item}, AlreadyDone: true, Record: step.path, Commit: commit}
		return step, err
	}
	if err := s.checkNotHeldByOther(store, item); err != nil {
		return replyStep{}, err
	}
	if step.targetKind, err = s.targetKind(target); err != nil {
		return replyStep{}, err
	}
	needData, needFront, err := s.needAt(source, target)
	if err != nil {
		return replyStep{}, err
	}
	step.needHash, step.needFront = ownerpkg.ContentHash(needData), needFront
	specData, specFront, _, found, err := s.spec(target)
	if err != nil {
		return replyStep{}, err
	}
	if found {
		step.specFound, step.specHash, step.specFront = true, ownerpkg.ContentHash(specData), specFront
	}
	if step.existing, _, err = p.tree.BlobAtHead(step.path); err != nil {
		return replyStep{}, err
	}
	return step, nil
}

// record keeps an earlier run's reply when it says the same, so a repeated step writes the same bytes.
func (step replyStep) record(reply ownerpkg.Reply) ([]byte, error) {
	if step.existing != nil {
		var earlier ownerpkg.Reply
		if err := earlier.Unmarshal(step.existing); err == nil {
			reply.At = earlier.At
			if reflect.DeepEqual(earlier, reply) {
				return step.existing, nil
			}
		}
		return nil, refusef("%s already holds a different reply to this revision", step.path)
	}
	reply.At = time.Now().UTC().Truncate(time.Second)
	return reply.Marshal()
}

func replyItem(edge string, data []byte) string {
	return "reply:" + edge + "@" + ownerpkg.Hex16(string(data))[:12]
}

func (s *session) finishReply(p party, step replyStep, kind string, route ownerpkg.Route, data []byte, followUps []ownerpkg.FollowUp) (any, error) {
	subject := fmt.Sprintf("owner: %s %s", kind, step.item)
	if _, err := p.tree.CommitFile(s.ctx, step.path, data, subject, map[string][]string{"Owner-Item": {step.item}}); err != nil {
		return nil, err
	}
	commit, err := p.tree.LastCommitTouching(step.path)
	if err != nil {
		return nil, err
	}
	replyID := replyItem(step.edge, data)
	versions := map[string]any{"need": step.needHash, "offer": nil}
	models := map[string]any{"need": step.needFront.AuthorModel, "offer": nil}
	if step.specFound {
		versions["offer"], models["offer"] = step.specHash, step.specFront.AuthorModel
	}
	notices := ownerpkg.Notices(p.routing.Topics(route), ownerpkg.NoticeFields{
		Type: kind, Epoch: p.epoch, From: step.from, To: step.to, Edge: step.edge, Item: replyID, Record: step.path, Commit: commit,
		Versions: versions, Models: models,
	})
	outcome, err := s.runStep(p, ownerpkg.Step{Answers: step.item, Path: step.path, Data: data, Subject: subject, FollowUps: followUps, Notices: notices})
	if err != nil {
		return nil, err
	}
	result := stepResult{Item: replyID, Answered: []string{step.item}, AlreadyDone: outcome.AlreadyDone, Record: step.path, Commit: commit, Undelivered: outcome.Undelivered}
	for _, followUp := range followUps {
		result.FollowUps = append(result.FollowUps, followUp.Item)
	}
	return result, nil
}
