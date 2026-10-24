package main

import (
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

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

// party is the owner of the session as a protocol participant, with what every step needs: its epoch, the routing
// table, its worktree and its step runner.
type party struct {
	identity
	epoch   int
	routing ownerpkg.Routing
	tree    ownerpkg.Worktree
	runner  ownerpkg.Runner
}

// party reads everything a step needs before the step writes anything: a listener configuration that cannot be
// read stops the command here, unable to check (exit 3).
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
	tree, err := s.worktree()
	if err != nil {
		return party{}, err
	}
	runner, err := ownerpkg.RunnerFromEnv(s.getenv, store, tree, s.deploy.StateDir)
	if err != nil {
		return party{}, err
	}
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

func (s *session) checkNotHeldByOther(store *ledger.Store, item string) error {
	current, err := store.Item(s.ctx, s.repo, item)
	if err != nil {
		return err
	}
	if current.Holder != "" && current.Holder != s.owner && (current.ExpiresAt == nil || current.ExpiresAt.After(time.Now())) {
		return refusef("%s is held by %s", item, current.Holder)
	}
	return nil
}

// runStep runs one step of p with its runner, which journals each notice the listener refuses in the deployment's
// state_dir/undelivered.jsonl.
func (s *session) runStep(p party, step ownerpkg.Step) (ownerpkg.Outcome, error) {
	return p.runner.Run(s.ctx, step)
}
