package owner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Topics is the cgraph topics.json document: each graph owner's node topics.
type Topics struct {
	Format    string              `json:"format"`
	Part      string              `json:"part"`
	Prefix    string              `json:"prefix"`
	TokenRule string              `json:"token_rule"`
	Owners    map[string][]string `json:"owners"`
}

// Facts is the deployment graph's facts.json: for each fact, the owners whose contracts state it.
type Facts map[string]Fact

// Fact names the owners whose contracts state one fact.
type Fact struct {
	Users []string `json:"users"`
}

// FactOwners maps a fact owner's role name to its fact's name, from the copy's skeleton/facts.yaml.
type FactOwners map[string]string

var roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// LoadTopics reads a cgraph topics.json document.
func LoadTopics(path string) (Topics, error) {
	var topics Topics
	if err := loadCgraphJSON(path, "cgraph/topics/v1", &topics, func() string { return topics.Format }); err != nil {
		return Topics{}, err
	}
	if !ValidTopicPrefix(topics.Prefix) {
		return Topics{}, fmt.Errorf("%s prefix %q is not a prefix under notifications. that ends in a dot", path, topics.Prefix)
	}
	return topics, nil
}

// LoadFacts reads facts.json. A missing file means no fact has users yet.
func LoadFacts(path string) (Facts, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Facts{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	facts := Facts{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&facts); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return facts, nil
}

// ParseFactOwners reads skeleton/facts.yaml and refuses a fact owner that is not a role name or owns two facts.
func ParseFactOwners(data []byte) (FactOwners, error) {
	var document struct {
		Facts []struct {
			Name    string  `yaml:"name"`
			Owner   string  `yaml:"owner"`
			Concept *string `yaml:"concept"`
		} `yaml:"facts"`
	}
	if err := decodeStrictYAML(data, &document); err != nil {
		return nil, fmt.Errorf("decode facts: %w", err)
	}
	owners := FactOwners{}
	names := map[string]bool{}
	for _, fact := range document.Facts {
		if strings.TrimSpace(fact.Name) == "" || !roleName.MatchString(fact.Owner) {
			return nil, fmt.Errorf("fact %q needs a name and an owner role name, got owner %q", fact.Name, fact.Owner)
		}
		if _, duplicate := owners[fact.Owner]; duplicate {
			return nil, fmt.Errorf("fact owner %q owns more than one fact", fact.Owner)
		}
		if names[fact.Name] {
			return nil, fmt.Errorf("fact %q is listed twice", fact.Name)
		}
		owners[fact.Owner] = fact.Name
		names[fact.Name] = true
	}
	return owners, nil
}

// Has reports whether owner's graph topics include topic.
func (t Topics) Has(owner, topic string) bool {
	for _, candidate := range t.Owners[owner] {
		if candidate == topic {
			return true
		}
	}
	return false
}

// Subscriptions is the owner's topics.json list plus concepts for the librarian, fact.<name>.needs for a fact
// owner, fact.<name>.users for every fact whose users include the owner, and all, on which landings, the score and
// protocol adoptions and reverts reach every owner (role owners are in no topics.json).
func Subscriptions(owner, role string, topics Topics, facts Facts, factOwners FactOwners) ([]string, error) {
	routing, err := NewRouting(topics.Prefix, factOwners)
	if err != nil {
		return nil, fmt.Errorf("topics.json: %w", err)
	}
	values := append([]string(nil), topics.Owners[owner]...)
	if role == "librarian" {
		values = appendNew(values, routing.prefix+"concepts")
	}
	if name, ok := factOwners[owner]; ok {
		values = appendNew(values, routing.fact(name, "needs"))
	}
	names := make([]string, 0, len(facts))
	for name := range facts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, user := range facts[name].Users {
			if user == owner {
				values = appendNew(values, routing.fact(name, "users"))
				break
			}
		}
	}
	return appendNew(values, routing.prefix+"all"), nil
}

func appendNew(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
