package main

import (
	"bytes"
	"fmt"
	"path/filepath"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

type manifest struct {
	Epoch  int `yaml:"epoch"`
	Trials []struct {
		Epoch  int      `yaml:"epoch"`
		Owners []string `yaml:"owners"`
	} `yaml:"trials"`
	Reasons map[string]struct {
		To       []string `yaml:"to"`
		Requires []string `yaml:"requires"`
	} `yaml:"reasons"`
}

func (s *session) manifest() (manifest, error) {
	data, err := s.mainFile(s.config.ManifestFile)
	if err != nil {
		return manifest{}, unable(err)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return manifest{}, unable(fmt.Errorf("decode %s: %w", s.config.ManifestFile, err))
	}
	if m.Epoch <= 0 || len(m.Reasons) == 0 {
		return manifest{}, unable(fmt.Errorf("%s needs a positive epoch and the reasons table", s.config.ManifestFile))
	}
	return m, nil
}

// epoch is the protocol epoch active for owner: its trial's epoch, else the manifest's.
func (m manifest) epochFor(owner string) int {
	epoch := m.Epoch
	for _, trial := range m.Trials {
		for _, member := range trial.Owners {
			if member == owner && trial.Epoch > epoch {
				epoch = trial.Epoch
			}
		}
	}
	return epoch
}

func (s *session) active() (map[string]bool, error) {
	data, err := s.mainFile(s.config.ActiveFile)
	if err != nil {
		return nil, unable(err)
	}
	var set struct {
		Name  string   `yaml:"name"`
		Nodes []string `yaml:"nodes"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&set); err != nil {
		return nil, unable(fmt.Errorf("decode %s: %w", s.config.ActiveFile, err))
	}
	active := make(map[string]bool, len(set.Nodes))
	for _, node := range set.Nodes {
		active[node] = true
	}
	return active, nil
}

func (s *session) factOwners() (ownerpkg.FactOwners, error) {
	data, found, err := s.blob("main", s.config.FactsFile)
	if err != nil {
		return nil, err
	}
	if !found {
		return ownerpkg.FactOwners{}, nil
	}
	owners, err := ownerpkg.ParseFactOwners(data)
	return owners, unable(err)
}

func (s *session) graphFile(name string) string { return filepath.Join(s.deploy.GraphDir, name) }

func (s *session) owners() (ownerpkg.Owners, error) {
	owners, err := ownerpkg.LoadOwners(s.graphFile("owners.json"))
	return owners, unable(err)
}

func (s *session) graphIndex() (ownerpkg.GraphIndex, error) {
	graph, err := ownerpkg.LoadGraph(s.graphFile("graph.json"))
	if err != nil {
		return ownerpkg.GraphIndex{}, unable(err)
	}
	return ownerpkg.NewGraphIndex(graph), nil
}

func (s *session) routing() (ownerpkg.Routing, error) {
	factOwners, err := s.factOwners()
	if err != nil {
		return ownerpkg.Routing{}, err
	}
	routing, err := ownerpkg.NewRouting(s.config.TopicPrefix, factOwners)
	return routing, unable(err)
}

// identity is a protocol participant: a graph owner, a fact owner or the librarian.
type identity struct {
	ID, Role, Kind, LeanDir, CFile string
	Fact                           string
}

func (s *session) identityOf(id string) (identity, error) {
	owners, err := s.owners()
	if err != nil {
		return identity{}, err
	}
	if owner, ok := owners.ByID(id); ok {
		return identity{ID: id, Role: owner.Role, Kind: owner.Kind, LeanDir: owner.Contract, CFile: owner.CFile}, nil
	}
	factOwners, err := s.factOwners()
	if err != nil {
		return identity{}, err
	}
	if name, ok := factOwners[id]; ok {
		return identity{ID: id, Role: "cross_cutting", Kind: "fact", Fact: name}, nil
	}
	if id == "librarian" {
		return identity{ID: id, Role: "librarian", Kind: "role"}, nil
	}
	return identity{}, refusef("%q is not a graph owner, a fact owner or the librarian of this run", id)
}
