package main

import (
	"fmt"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

type whoamiResult struct {
	Owner     string `json:"owner"`
	Role      string `json:"role"`
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	LeanDir   string `json:"lean_dir"`
	CFile     string `json:"c_file"`
	ActiveSet string `json:"active_set"`
	Active    bool   `json:"active"`
	Epoch     int    `json:"epoch"`
}

func (s *session) whoami() (any, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	id, err := s.identityOf(s.owner)
	if err != nil {
		return nil, err
	}
	active, err := s.active()
	if err != nil {
		return nil, err
	}
	m, err := s.manifest()
	if err != nil {
		return nil, err
	}
	return whoamiResult{
		Owner: id.ID, Role: id.Role, Key: ownerpkg.Key(id.ID), Kind: id.Kind, LeanDir: id.LeanDir, CFile: id.CFile,
		ActiveSet: s.config.ActiveFile, Active: active[id.ID] || id.Kind == "fact" || id.Kind == "role", Epoch: m.epochFor(id.ID),
	}, nil
}

type topicsResult struct {
	Owner  string   `json:"owner"`
	Topics []string `json:"topics"`
}

func (s *session) topics() (any, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	id, err := s.identityOf(s.owner)
	if err != nil {
		return nil, err
	}
	topics, err := ownerpkg.LoadTopics(s.graphFile("topics.json"))
	if err != nil {
		return nil, unable(err)
	}
	if topics.Prefix != s.config.TopicPrefix {
		return nil, unable(fmt.Errorf("topics.json prefix %q differs from skeleton/owner.yaml topic_prefix %q", topics.Prefix, s.config.TopicPrefix))
	}
	facts, err := ownerpkg.LoadFacts(s.graphFile("facts.json"))
	if err != nil {
		return nil, unable(err)
	}
	factOwners, err := s.factOwners()
	if err != nil {
		return nil, err
	}
	subscriptions, err := ownerpkg.Subscriptions(id.ID, id.Role, topics, facts, factOwners)
	if err != nil {
		return nil, unable(err)
	}
	return topicsResult{Owner: id.ID, Topics: subscriptions}, nil
}

type worktreeResult struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

func (s *session) worktreeCommand() (any, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	tree, err := s.worktree()
	if err != nil {
		return nil, err
	}
	return worktreeResult{Path: tree.Path, Branch: tree.Branch}, nil
}
