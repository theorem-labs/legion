package main

import (
	"strconv"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func (s *session) claim(item string) (any, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	if _, err := store.Item(s.ctx, s.repo, item); err != nil {
		return nil, err
	}
	return store.Claim(s.ctx, s.repo, item, s.owner, 0)
}

// done completes a need or an offer as superseded by the revision version, which must be later than the item's and
// must exist: the source's need record, or the target's spec, is at that revision or newer.
func (s *session) done(item string, ref ownerpkg.ItemRef, version string) (any, error) {
	revision, _ := ownerpkg.ParseRevision(ref.Version)
	newer, _ := ownerpkg.ParseRevision(version)
	if newer <= revision {
		return nil, refusef("%s can be superseded only by a later revision than %s, not %s", item, ref.Version, version)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	current, err := store.Item(s.ctx, s.repo, item)
	if err != nil {
		return nil, err
	}
	if current.Owner != s.owner {
		return nil, refusef("%s is in the queue of %s, not of %s", item, current.Owner, s.owner)
	}
	if current.State == "done" {
		if current.Result == "superseded:"+version {
			return current, nil
		}
		return nil, refusef("%s is already done with result %q", item, current.Result)
	}
	source, target, err := ownerpkg.ParseEdgeID(ref.Subject)
	if err != nil {
		return nil, err
	}
	latest, record := 0, ""
	if ref.Type == "need" {
		_, front, err := s.needAt(source, target)
		if err != nil {
			return nil, err
		}
		latest, record = front.Revision, source+"'s need to "+target
	} else {
		_, front, _, found, err := s.spec(target)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, refusef("%s has no spec, so no revision supersedes %s", target, item)
		}
		latest, record = front.Revision, target+"'s spec"
	}
	if latest < newer {
		return nil, refusef("%s is r%d: no %s supersedes %s", record, latest, version, item)
	}
	return s.completeSuperseded(item, version)
}

// completeSuperseded completes the session owner's item as superseded:<version>, with no record, once its caller
// has the evidence that the newer version exists.
func (s *session) completeSuperseded(item, version string) (any, error) {
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	if err := ownerpkg.Supersede(s.ctx, store, s.repo, s.owner, item, version); err != nil {
		return nil, err
	}
	return store.Item(s.ctx, s.repo, item)
}

func (s *session) returned(item string) (any, error) {
	ref, err := ownerpkg.ParseItemID(item)
	if err != nil || ref.Type != "returned" {
		return nil, usagef("returned <returned:<migration>@<event id>>")
	}
	event, err := strconv.ParseInt(ref.Version, 10, 64)
	if err != nil {
		return nil, usagef("returned item %s: %v", item, err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	events, err := store.Events(s.ctx, s.repo, event-1, ref.Subject)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || events[0].ID != event {
		return nil, refusef("migration %s has no event %d", ref.Subject, event)
	}
	return ownerpkg.ReturnFromEvent(events[0])
}
