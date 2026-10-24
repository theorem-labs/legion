package owner

import (
	"cmp"
	"fmt"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// Rule is the shared rule of skeleton/RULE.yaml: the keys that order every owner's queue.
type Rule struct {
	Version     int               `yaml:"version"`
	Order       []RuleKey         `yaml:"order"`
	Definitions map[string]string `yaml:"definitions"`
}

// RuleKey is one ordering key and its direction.
type RuleKey struct {
	Key       string `yaml:"key"`
	Direction string `yaml:"direction"`
}

// Priority is the value of the shared rule's computed keys for one item.
type Priority struct {
	StoppedInstructions int `json:"stopped_instructions"`
	CallHeight          int `json:"call_height"`
}

// PrioritizedItem is one item with every value the rule can order by. Position is the item's place in the order the
// ledger lists an owner's open items (Store.OpenItems), which is the order it enqueued them: the rule's enqueued_at.
type PrioritizedItem struct {
	ID                  string
	StoppedInstructions int
	CallHeight          int
	Position            int
}

var ruleKeys = map[string]bool{"stopped_instructions": true, "call_height": true, "enqueued_at": true}

// ParseRule reads RULE.yaml and refuses a key or direction that this command cannot apply.
func ParseRule(data []byte) (Rule, error) {
	var rule Rule
	if err := decodeStrictYAML(data, &rule); err != nil {
		return Rule{}, fmt.Errorf("decode rule: %w", err)
	}
	if rule.Version <= 0 {
		return Rule{}, fmt.Errorf("rule version %d must be positive", rule.Version)
	}
	if len(rule.Order) == 0 {
		return Rule{}, fmt.Errorf("rule has no order")
	}
	seen := map[string]bool{}
	for _, key := range rule.Order {
		if !ruleKeys[key.Key] {
			return Rule{}, fmt.Errorf("rule key %q is not one of stopped_instructions, call_height, enqueued_at", key.Key)
		}
		if key.Direction != "ascending" && key.Direction != "descending" {
			return Rule{}, fmt.Errorf("rule key %q direction %q must be ascending or descending", key.Key, key.Direction)
		}
		if seen[key.Key] {
			return Rule{}, fmt.Errorf("rule key %q appears twice", key.Key)
		}
		seen[key.Key] = true
	}
	return rule, nil
}

// Sort orders items by the rule's keys. It is stable, so items the keys tie keep the order they are given in: the
// ledger's order, when BuildInbox gives them.
func (r Rule) Sort(items []PrioritizedItem) {
	sort.SliceStable(items, func(i, j int) bool { return r.before(items[i], items[j]) })
}

func (r Rule) before(left, right PrioritizedItem) bool {
	for _, key := range r.Order {
		var order int
		switch key.Key {
		case "stopped_instructions":
			order = cmp.Compare(left.StoppedInstructions, right.StoppedInstructions)
		case "call_height":
			order = cmp.Compare(left.CallHeight, right.CallHeight)
		case "enqueued_at":
			order = cmp.Compare(left.Position, right.Position)
		}
		if key.Direction == "descending" {
			order = -order
		}
		if order != 0 {
			return order < 0
		}
	}
	return false
}

// RuleInputs are the facts the rule's definitions read. NodeCounts maps a node id to the image instructions of its
// Lean directory (the newest score's skeleton.node_instructions, keyed by node as the plan's score schema P6
// writes it; nil before the first score, so every value is 0). Interfaces maps an interface name to the functions
// that take it (measures.json); nil when unread.
type RuleInputs struct {
	Index      GraphIndex
	Owners     Owners
	Active     map[string]bool
	FactUsers  map[string][]string
	NodeCounts map[string]int
	Interfaces map[string][]string
}

// Priority computes the rule's values for one item.
func (in RuleInputs) Priority(item ledger.Item) (Priority, error) {
	priorities, err := in.Priorities([]ledger.Item{item})
	if err != nil {
		return Priority{}, err
	}
	return priorities[item.ID], nil
}

// Priorities computes the rule's values for every item, keyed by item id. The stopped instructions of a subject are
// computed once: many items of one queue share their subject.
func (in RuleInputs) Priorities(items []ledger.Item) (map[string]Priority, error) {
	heights := in.callHeights()
	stopped := map[string]int{}
	out := make(map[string]Priority, len(items))
	for _, item := range items {
		ref, err := ParseItemID(item.ID)
		if err != nil {
			return nil, err
		}
		switch ref.Type {
		case "upstream-review":
			out[item.ID] = Priority{StoppedInstructions: in.sum(in.Index.partFuncs)}
			continue
		case "interface", "produce":
			functions, err := in.interfaceFunctions(ref)
			if err != nil {
				return nil, fmt.Errorf("item %s: %w", item.ID, err)
			}
			out[item.ID] = Priority{StoppedInstructions: in.sum(functions)}
			continue
		}
		subject, err := itemSubject(ref, item.Owner)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", item.ID, err)
		}
		count, ok := stopped[subject]
		if !ok {
			count = in.stopped(subject)
			stopped[subject] = count
		}
		out[item.ID] = Priority{StoppedInstructions: count, CallHeight: heights[subject]}
	}
	return out, nil
}

// itemSubject is the owner S that the rule's definitions name: an edge item's source, a start item's owner,
// and otherwise the owner whose queue holds the item.
func itemSubject(ref ItemRef, owner string) (string, error) {
	switch ref.Type {
	case "need", "offer", "reply", "agreed", "lean":
		source, _, err := ParseEdgeID(ref.Subject)
		return source, err
	case "stale":
		edge := strings.TrimPrefix(ref.Subject, "edge:")
		if at := strings.LastIndexByte(edge, '@'); at > 0 {
			edge = edge[:at]
		}
		source, _, err := ParseEdgeID(edge)
		return source, err
	case "start":
		return ref.Subject, nil
	}
	return owner, nil
}

func (in RuleInputs) interfaceFunctions(ref ItemRef) ([]string, error) {
	if in.Interfaces == nil {
		return nil, fmt.Errorf("the rule needs measures.json for interface and produce items")
	}
	if ref.Type == "produce" {
		functions, ok := in.Interfaces[ref.Subject]
		if !ok {
			return nil, fmt.Errorf("interface %q is not in measures.json", ref.Subject)
		}
		return functions, nil
	}
	for name, functions := range in.Interfaces {
		if Hex16(name) == ref.Subject {
			return functions, nil
		}
	}
	return nil, fmt.Errorf("no interface of measures.json has hex16 %s", ref.Subject)
}

func (in RuleInputs) stopped(subject string) int {
	if users, ok := in.FactUsers[subject]; ok {
		return in.sum(users)
	}
	switch in.kind(subject) {
	case "function":
		total := in.instructions(subject)
		for caller := range in.reachingCallers(subject) {
			if in.Active[caller] {
				total += in.instructions(caller)
			}
		}
		return total
	case "struct", "global", "percpu", "subsystem":
		total := 0
		for user := range in.Index.users[subject] {
			if in.Active[user] && in.kind(user) == "function" {
				total += in.instructions(user)
			}
		}
		return total
	}
	return 0
}

func (in RuleInputs) kind(id string) string {
	if kind := in.Index.Kind(id); kind != "" {
		return kind
	}
	owner, _ := in.Owners.ByID(id)
	return owner.Kind
}

func (in RuleInputs) instructions(id string) int {
	return in.NodeCounts[id]
}

func (in RuleInputs) sum(ids []string) int {
	total := 0
	for _, id := range ids {
		total += in.instructions(id)
	}
	return total
}

// reachingCallers returns every function that reaches subject through call edges, subject excluded.
func (in RuleInputs) reachingCallers(subject string) map[string]bool {
	seen := map[string]bool{}
	pending := []string{subject}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for caller := range in.Index.callers[current] {
			if caller != subject && !seen[caller] {
				seen[caller] = true
				pending = append(pending, caller)
			}
		}
	}
	return seen
}

// callHeights is, for each active function, the longest call path to an active function with no callee in the
// active set, over the strongly connected components of the active call graph.
func (in RuleInputs) callHeights() map[string]int {
	var nodes []string
	for id, active := range in.Active {
		if active && in.kind(id) == "function" {
			nodes = append(nodes, id)
		}
	}
	sort.Strings(nodes)
	successors := func(id string) []string {
		var out []string
		for callee := range in.Index.callees[id] {
			if in.Active[callee] && in.kind(callee) == "function" {
				out = append(out, callee)
			}
		}
		sort.Strings(out)
		return out
	}

	// Tarjan's algorithm: components are produced in reverse topological order, so every successor
	// component already has its height.
	index, lowlink := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	component := map[string]int{}
	componentHeight := []int{}
	var stack []string
	next := 0
	var visit func(string)
	visit = func(id string) {
		index[id], lowlink[id] = next, next
		next++
		stack = append(stack, id)
		onStack[id] = true
		for _, callee := range successors(id) {
			if _, seen := index[callee]; !seen {
				visit(callee)
				lowlink[id] = min(lowlink[id], lowlink[callee])
			} else if onStack[callee] {
				lowlink[id] = min(lowlink[id], index[callee])
			}
		}
		if lowlink[id] != index[id] {
			return
		}
		number := len(componentHeight)
		var members []string
		for {
			member := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[member] = false
			component[member] = number
			members = append(members, member)
			if member == id {
				break
			}
		}
		height := 0
		for _, member := range members {
			for _, callee := range successors(member) {
				if other := component[callee]; other != number {
					height = max(height, componentHeight[other]+1)
				}
			}
		}
		componentHeight = append(componentHeight, height)
	}
	for _, id := range nodes {
		if _, seen := index[id]; !seen {
			visit(id)
		}
	}
	heights := make(map[string]int, len(nodes))
	for _, id := range nodes {
		heights[id] = componentHeight[component[id]]
	}
	return heights
}
