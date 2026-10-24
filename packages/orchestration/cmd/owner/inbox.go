package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func (s *session) inbox() (any, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	items, err := store.OpenItems(s.ctx, s.repo, s.owner)
	if err != nil {
		return nil, err
	}
	ruleData, err := s.mainFile(s.config.RuleFile)
	if err != nil {
		return nil, unable(err)
	}
	rule, err := ownerpkg.ParseRule(ruleData)
	if err != nil {
		return nil, unable(err)
	}
	inputs, err := s.ruleInputs()
	if err != nil {
		return nil, err
	}
	priorities, err := inputs.Priorities(items)
	if err != nil {
		return nil, unable(err)
	}
	inbox, err := ownerpkg.BuildInbox(items, s.config, priorities, rule)
	if err != nil {
		return nil, err
	}
	for index := range inbox {
		if err := s.fillRecord(&inbox[index]); err != nil {
			return nil, err
		}
	}
	return inbox, nil
}

func (s *session) ruleInputs() (ownerpkg.RuleInputs, error) {
	index, err := s.graphIndex()
	if err != nil {
		return ownerpkg.RuleInputs{}, err
	}
	owners, err := s.owners()
	if err != nil {
		return ownerpkg.RuleInputs{}, err
	}
	active, err := s.active()
	if err != nil {
		return ownerpkg.RuleInputs{}, err
	}
	facts, err := ownerpkg.LoadFacts(s.graphFile("facts.json"))
	if err != nil {
		return ownerpkg.RuleInputs{}, unable(err)
	}
	factOwners, err := s.factOwners()
	if err != nil {
		return ownerpkg.RuleInputs{}, err
	}
	factUsers := map[string][]string{}
	for owner, name := range factOwners {
		factUsers[owner] = facts[name].Users
	}
	counts, err := s.nodeInstructions()
	if err != nil {
		return ownerpkg.RuleInputs{}, unable(err)
	}
	interfaces, err := s.interfaces()
	if err != nil {
		return ownerpkg.RuleInputs{}, unable(err)
	}
	return ownerpkg.RuleInputs{Index: index, Owners: owners, Active: active, FactUsers: factUsers, NodeCounts: counts, Interfaces: interfaces}, nil
}

// nodeInstructions reads skeleton.node_instructions from the newest score JSON of the score part on the bare
// repository's score branch. Before the first score there is none, and every count is 0.
func (s *session) nodeInstructions() (map[string]int, error) {
	commit, err := s.lastCommit("refs/heads/score", s.config.ScorePart+"/")
	if err != nil || commit == "" {
		return nil, err
	}
	out, err := s.git("diff-tree", "--no-commit-id", "--name-only", "-r", "--root", commit, "--", s.config.ScorePart+"/")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, file := range strings.Fields(out) {
		if strings.HasSuffix(file, ".json") {
			files = append(files, file)
		}
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("score commit %s changes %d score files of part %s, want 1", commit, len(files), s.config.ScorePart)
	}
	data, _, err := s.blob(commit, files[0])
	if err != nil {
		return nil, err
	}
	var score struct {
		Skeleton *struct {
			NodeInstructions map[string]int `json:"node_instructions"`
		} `json:"skeleton"`
	}
	if err := json.Unmarshal(data, &score); err != nil {
		return nil, fmt.Errorf("decode score %s: %w", files[0], err)
	}
	if score.Skeleton == nil {
		return nil, nil
	}
	return score.Skeleton.NodeInstructions, nil
}

// interfaces reads measures.json's interface mappings, or nil when the graph directory has none.
func (s *session) interfaces() (map[string][]string, error) {
	data, err := os.ReadFile(s.graphFile("measures.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var measures struct {
		Format     string `json:"format"`
		Interfaces struct {
			Mappings []struct {
				Interface string   `json:"interface"`
				Functions []string `json:"functions"`
			} `json:"mappings"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal(data, &measures); err != nil {
		return nil, fmt.Errorf("decode measures.json: %w", err)
	}
	if measures.Format != "cgraph/measures/v1" {
		return nil, fmt.Errorf("measures.json format = %q, want cgraph/measures/v1", measures.Format)
	}
	out := map[string][]string{}
	for _, mapping := range measures.Interfaces.Mappings {
		out[mapping.Interface] = mapping.Functions
	}
	return out, nil
}

// fillRecord names the commit to read, and marks an item that waits for its record (a concept item) ready once its
// branch holds that record. A reply item's record is the reply of either party whose content hash starts with the
// item's version.
func (s *session) fillRecord(entry *ownerpkg.InboxItem) error {
	ref, err := ownerpkg.ParseItemID(entry.ID)
	if err != nil {
		return err
	}
	if ref.Type == "reply" {
		source, target, err := ownerpkg.ParseEdgeID(ref.Subject)
		if err != nil {
			return err
		}
		for _, party := range []string{source, target} {
			dir := ownerpkg.NodeRecord(s.config.RecordsRoot, party, "replies")
			out, err := s.git("ls-tree", "--name-only", branch(party)+":"+dir)
			if err != nil {
				continue
			}
			for _, name := range strings.Fields(out) {
				if !strings.HasPrefix(name, ownerpkg.Hex16(ref.Subject)+"-") {
					continue
				}
				file := path.Join(dir, name)
				data, found, err := s.blob(branch(party), file)
				if err != nil {
					return err
				}
				if found && ownerpkg.Hex16(string(data))[:12] == ref.Version {
					entry.Record, entry.RecordOwner = file, party
				}
			}
		}
	}
	if entry.Record == "" {
		return nil
	}
	rev := "main"
	switch {
	case entry.RecordOwner != "":
		rev = branch(entry.RecordOwner)
	case entry.Branch != "":
		rev = "refs/heads/" + entry.Branch
	}
	commit, err := s.lastCommit(rev, entry.Record)
	entry.Commit = commit
	if entry.WaitsForRecord() {
		entry.Ready = commit != ""
	}
	return err
}
