package owner

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Graph is the part of a cgraph graph.json document that owner routing reads.
type Graph struct {
	Format string      `json:"format"`
	Part   string      `json:"part"`
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
}

// GraphNode is one cgraph node.
type GraphNode struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Part     bool     `json:"part"`
	CFile    string   `json:"c_file"`
	Contract string   `json:"contract"`
	Members  []string `json:"members"`
}

// GraphEdge is one cgraph edge.
type GraphEdge struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Target string `json:"target"`
	Access string `json:"access,omitempty"`
	Field  string `json:"field,omitempty"`
}

// Owner is one cgraph owners.json entry. Neighbors is cgraph's neighbor count.
type Owner struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	CFile     string `json:"c_file"`
	Contract  string `json:"contract"`
	Subsystem string `json:"subsystem"`
	Neighbors int    `json:"neighbors"`
}

// Owners is the cgraph owners.json document.
type Owners struct {
	Format string  `json:"format"`
	Part   string  `json:"part"`
	Owners []Owner `json:"owners"`
}

// ByID finds one owner by graph node id.
func (o Owners) ByID(id string) (Owner, bool) {
	for _, owner := range o.Owners {
		if owner.ID == id {
			return owner, true
		}
	}
	return Owner{}, false
}

// LoadGraph reads a cgraph graph.json document.
func LoadGraph(path string) (Graph, error) {
	var graph Graph
	if err := loadCgraphJSON(path, "cgraph/graph/v1", &graph, func() string { return graph.Format }); err != nil {
		return Graph{}, err
	}
	for _, edge := range graph.Edges {
		if strings.TrimSpace(edge.Kind) == "" || strings.TrimSpace(edge.Source) == "" || strings.TrimSpace(edge.Target) == "" {
			return Graph{}, fmt.Errorf("%s has an edge with an empty kind or endpoint", path)
		}
	}
	return graph, nil
}

// LoadOwners reads a cgraph owners.json document and refuses a duplicate or incomplete owner.
func LoadOwners(path string) (Owners, error) {
	var owners Owners
	if err := loadCgraphJSON(path, "cgraph/owners/v1", &owners, func() string { return owners.Format }); err != nil {
		return Owners{}, err
	}
	seen := make(map[string]struct{}, len(owners.Owners))
	for _, owner := range owners.Owners {
		if strings.TrimSpace(owner.ID) == "" || strings.TrimSpace(owner.Kind) == "" || strings.TrimSpace(owner.Role) == "" {
			return Owners{}, fmt.Errorf("%s has an owner with an empty id, kind or role", path)
		}
		if _, duplicate := seen[owner.ID]; duplicate {
			return Owners{}, fmt.Errorf("%s lists owner %q twice", path, owner.ID)
		}
		seen[owner.ID] = struct{}{}
	}
	return owners, nil
}

// loadCgraphJSON decodes a cgraph output and checks its format. cgraph documents carry analysis fields this
// package does not read, so unknown fields are allowed.
func loadCgraphJSON(path, format string, dest any, got func() string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if got() != format {
		return fmt.Errorf("%s format = %q, want %q", path, got(), format)
	}
	return nil
}

var (
	callEdgeKinds = map[string]bool{"call": true, "call_indirect": true}
	linkEdgeKinds = map[string]bool{"embeds": true, "embeds_base": true, "points_to": true, "member_of": true}
	stateKinds    = map[string]bool{"struct": true, "global": true, "percpu": true}
)

// GraphIndex holds cgraph's three relation layers, built the way cgraph builds owners' topics.
type GraphIndex struct {
	kinds     map[string]string
	partFuncs []string
	needEdges []GraphEdge
	callees   map[string]map[string]bool
	callers   map[string]map[string]bool
	uses      map[string]map[string]bool
	users     map[string]map[string]bool
	linked    map[string]map[string]bool
}

// NewGraphIndex indexes calls, accesses (with subsystem membership) and structure links.
func NewGraphIndex(graph Graph) GraphIndex {
	index := GraphIndex{
		kinds: map[string]string{}, callees: map[string]map[string]bool{},
		callers: map[string]map[string]bool{}, uses: map[string]map[string]bool{}, users: map[string]map[string]bool{},
		linked: map[string]map[string]bool{},
	}
	subsystemOf := map[string]string{}
	for _, node := range graph.Nodes {
		index.kinds[node.ID] = node.Kind
		if node.Kind == "function" && node.Part {
			index.partFuncs = append(index.partFuncs, node.ID)
		}
		if node.Kind == "subsystem" {
			for _, member := range node.Members {
				subsystemOf[member] = node.ID
			}
		}
	}
	sort.Strings(index.partFuncs)
	add := func(relation map[string]map[string]bool, from, to string) {
		if relation[from] == nil {
			relation[from] = map[string]bool{}
		}
		relation[from][to] = true
	}
	for _, edge := range graph.Edges {
		switch {
		case callEdgeKinds[edge.Kind]:
			add(index.callees, edge.Source, edge.Target)
			add(index.callers, edge.Target, edge.Source)
			index.needEdges = append(index.needEdges, edge)
		case edge.Kind == "access":
			add(index.uses, edge.Source, edge.Target)
			add(index.users, edge.Target, edge.Source)
			if subsystem, ok := subsystemOf[edge.Target]; ok {
				add(index.uses, edge.Source, subsystem)
				add(index.users, subsystem, edge.Source)
			}
			index.needEdges = append(index.needEdges, edge)
		case (linkEdgeKinds[edge.Kind] && stateKinds[index.kinds[edge.Source]] && stateKinds[index.kinds[edge.Target]] && edge.Source != edge.Target) ||
			edge.Kind == "in_subsystem":
			add(index.linked, edge.Source, edge.Target)
			add(index.linked, edge.Target, edge.Source)
		}
	}
	return index
}

// NeedEdges returns the call and access edges, along which an owner sends needs.
func (g GraphIndex) NeedEdges() []GraphEdge { return append([]GraphEdge(nil), g.needEdges...) }

// Kind returns a node's graph kind.
func (g GraphIndex) Kind(id string) string { return g.kinds[id] }

// Linked returns the nodes that a structure link or subsystem membership joins to id.
func (g GraphIndex) Linked(id string) []string { return sortedSet(g.linked[id]) }

// Neighbors returns id's neighbors in all three layers.
func (g GraphIndex) Neighbors(id string) []string {
	all := map[string]bool{}
	for _, relation := range []map[string]map[string]bool{g.callees, g.callers, g.uses, g.users, g.linked} {
		for other := range relation[id] {
			all[other] = true
		}
	}
	return sortedSet(all)
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// RouteStep names one row of the routing table.
type RouteStep string

// The rows of the routing table.
const (
	RouteNeed            RouteStep = "need"
	RouteOffer           RouteStep = "offer"
	RouteAccept          RouteStep = "accept"
	RouteReplyFromTarget RouteStep = "reply-from-target"
	RouteReplyFromSource RouteStep = "reply-from-source"
	RouteReplyToPremise  RouteStep = "reply-to-premise-owner"
	RouteOwnerItem       RouteStep = "owner-item"
	RouteProposal        RouteStep = "proposal"
	RouteConcept         RouteStep = "concept"
	RouteLibrarianAnswer RouteStep = "librarian-answer"
	RouteLanding         RouteStep = "landing"
)

// Route is one protocol step to route. TargetKind is the target's graph kind; a fact owner has none.
// Linked marks an offer or reply that changes a claim about a linked data structure.
type Route struct {
	Step       RouteStep
	Source     string
	Target     string
	TargetKind string
	Owner      string
	Linked     bool
}

// Audience says who a delivery reaches.
type Audience string

// The audiences of a delivery.
const (
	AudienceOwner     Audience = "owner"
	AudienceLinked    Audience = "linked"
	AudienceNeighbors Audience = "neighbors"
	AudienceAll       Audience = "all"
)

// Delivery is one topic of a routed step. Receiver is set for AudienceOwner; Node for linked and neighbors.
type Delivery struct {
	Topic    string
	Audience Audience
	Receiver string
	Node     string
}

// Routing is the routing table of the owner protocol under one topic prefix. Build it with NewRouting.
type Routing struct {
	prefix     string
	factOwners FactOwners
}

// NewRouting is the routing table under prefix, a prefix under notifications. that ends in a dot (ValidTopicPrefix);
// factOwners maps a fact owner's role name to its fact.
func NewRouting(prefix string, factOwners FactOwners) (Routing, error) {
	if !ValidTopicPrefix(prefix) {
		return Routing{}, fmt.Errorf("topic prefix %q is not a prefix under notifications. that ends in a dot", prefix)
	}
	return Routing{prefix: prefix, factOwners: factOwners}, nil
}

// Topics returns the deliveries the routing table gives for route.
func (r Routing) Topics(route Route) []Delivery {
	switch route.Step {
	case RouteNeed:
		return []Delivery{r.toOwner(r.NeedsTopic(route.Target), route.Target)}
	case RouteOffer, RouteReplyFromTarget:
		if name, ok := r.factOwners[route.Target]; ok {
			return []Delivery{r.toOwner(r.fact(name, "users"), route.Source)}
		}
		if route.TargetKind == "function" {
			return []Delivery{r.toOwner(r.node(route.Target, "callers"), route.Source)}
		}
		deliveries := []Delivery{r.toOwner(r.node(route.Target, "users"), route.Source)}
		if route.Linked {
			deliveries = append(deliveries, Delivery{Topic: r.node(route.Target, "linked"), Audience: AudienceLinked, Node: route.Target})
		}
		return deliveries
	case RouteAccept, RouteReplyFromSource:
		if name, ok := r.factOwners[route.Target]; ok {
			return []Delivery{r.toOwner(r.fact(name, "needs"), route.Target)}
		}
		if route.TargetKind == "function" {
			return []Delivery{r.toOwner(r.node(route.Source, "callees"), route.Target)}
		}
		return []Delivery{r.toOwner(r.node(route.Target, "needs"), route.Target)}
	case RouteOwnerItem, RouteLibrarianAnswer, RouteReplyToPremise:
		// A reply whose reason goes to the premise owner is that owner's item, not the edge's other party's.
		return []Delivery{r.toOwner(r.NeedsTopic(route.Owner), route.Owner)}
	case RouteProposal:
		return []Delivery{{Topic: r.node(route.Owner, "neighbors"), Audience: AudienceNeighbors, Node: route.Owner}}
	case RouteConcept:
		return []Delivery{r.toOwner(r.prefix+"concepts", "librarian")}
	case RouteLanding:
		return []Delivery{{Topic: r.prefix + "all", Audience: AudienceAll}}
	}
	return nil
}

// NeedsTopic is an owner's needs topic: node.<id>.needs, or fact.<name>.needs for a fact owner.
func (r Routing) NeedsTopic(owner string) string {
	if name, ok := r.factOwners[owner]; ok {
		return r.fact(name, "needs")
	}
	return r.node(owner, "needs")
}

func (r Routing) toOwner(topic, owner string) Delivery {
	return Delivery{Topic: topic, Audience: AudienceOwner, Receiver: owner}
}

func (r Routing) node(id, suffix string) string {
	return r.prefix + "node." + TopicToken(id) + "." + suffix
}

func (r Routing) fact(name, suffix string) string {
	return r.prefix + "fact." + TopicToken(name) + "." + suffix
}

// NoticeFields is the payload of an owner notice (plan "Envoy notices").
type NoticeFields struct {
	Type     string
	Epoch    int
	From, To string
	Edge     string
	Item     string // the work item the notice is about; "" for a record that queues no item
	Version  string // the record's version, which the message names when the notice names no item
	Record   string
	Commit   string
	Versions map[string]any
	Models   map[string]any
}

// Notices builds one notice per delivery. Every notice of an item carries the key NoticeKey(item). A notice of a record
// that queues no item names no item: its subject is its sender, and its key is NoticeKey(record@commit), one for each
// revision of the record. A re-run of the step repeats the key, so each subscribed session takes the notice once. The
// message names the record's commit when there is one: the gate's near verdict has none yet.
func Notices(deliveries []Delivery, fields NoticeFields) []Notice {
	at := fields.Commit
	if len(at) > 12 {
		at = at[:12]
	}
	if at != "" {
		at = "at " + at
	}
	subject := fields.Edge
	if source, target, err := ParseEdgeID(fields.Edge); err == nil {
		subject = source + " -> " + target
	} else if fields.Edge == "" {
		subject = fields.From
	}
	version, key := fields.Version, fields.Record+"@"+fields.Commit
	if fields.Item != "" {
		key = fields.Item
		if ref, err := ParseItemID(fields.Item); err == nil {
			version = ref.Version
		}
	}
	message := strings.Join(strings.Fields(fmt.Sprintf("%s %s %s (epoch %d) %s", fields.Type, subject, version, fields.Epoch, at)), " ")
	notices := make([]Notice, 0, len(deliveries))
	for _, delivery := range deliveries {
		payload := Payload{
			"type": fields.Type, "epoch": fields.Epoch, "from": fields.From, "to": fields.To, "edge": fields.Edge,
			"record": fields.Record, "commit": fields.Commit, "versions": fields.Versions, "models": fields.Models,
		}
		if fields.Item != "" {
			payload["item"] = fields.Item
		}
		notices = append(notices, Notice{Topic: delivery.Topic, Message: message, Payload: payload, Key: NoticeKey(key)})
	}
	return notices
}
