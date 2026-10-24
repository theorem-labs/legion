package main

import ownerpkg "github.com/sjawhar/legion/orchestration/owner"

type stepResult struct {
	Item        string   `json:"item,omitempty"`
	Answered    []string `json:"answered,omitempty"`
	AlreadyDone bool     `json:"already_done,omitempty"`
	Record      string   `json:"record"`
	Revision    int      `json:"revision,omitempty"`
	Commit      string   `json:"commit"`
	FollowUps   []string `json:"follow_ups,omitempty"`
	Undelivered []string `json:"undelivered,omitempty"`
}

// of is the result with what the step did: its commit, its undelivered notices, and whether it was the repeat of a
// step that answered its items already, which queued nothing again.
func (r stepResult) of(outcome ownerpkg.Outcome) stepResult {
	r.Commit, r.AlreadyDone, r.Undelivered = outcome.Commit, outcome.AlreadyDone, outcome.Undelivered
	if outcome.AlreadyDone {
		r.FollowUps = nil
	}
	return r
}

// party is the owner of the session as a protocol participant, with what every step needs: its epoch, the routing
// table, its worktree and its step runner.
type party struct {
	identity
	epoch   int
	routing ownerpkg.Routing
	tree    ownerpkg.Worktree
	runner  ownerpkg.Runner
}

// party reads everything a step needs before the step writes anything: an input or a listener configuration that
// cannot be read stops the command here, before it creates the worktree or its branch (exit 3).
func (s *session) party() (party, error) {
	if err := s.load(); err != nil {
		return party{}, err
	}
	id, err := s.identityOf(s.owner)
	if err != nil {
		return party{}, err
	}
	m, err := s.manifest()
	if err != nil {
		return party{}, err
	}
	routing, err := s.routing()
	if err != nil {
		return party{}, err
	}
	store, err := s.ledger()
	if err != nil {
		return party{}, err
	}
	runner, err := ownerpkg.NewRunner(s.owner, s.repo, store, s.deploy.StateDir, s.getenv)
	if err != nil {
		return party{}, err
	}
	tree, err := s.worktree()
	if err != nil {
		return party{}, err
	}
	runner.Tree = tree
	return party{identity: id, epoch: m.epochFor(id.ID), routing: routing, tree: tree, runner: runner}, nil
}

// targetKind is the routing kind of a need's target: its graph kind, or "" for a fact owner.
func (s *session) targetKind(target string) (string, error) {
	id, err := s.identityOf(target)
	if err != nil {
		return "", err
	}
	if id.Kind == "fact" {
		return "", nil
	}
	if id.Kind == "role" {
		return "", refusef("%s takes no needs", target)
	}
	return id.Kind, nil
}
