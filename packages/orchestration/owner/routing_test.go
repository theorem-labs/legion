package owner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type routingFixture struct {
	graph      Graph
	index      GraphIndex
	owners     Owners
	topics     Topics
	facts      Facts
	factOwners FactOwners
}

func loadRoutingFixture(t *testing.T) routingFixture {
	t.Helper()
	graph, err := LoadGraph(filepath.Join("testdata", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	owners, err := LoadOwners(filepath.Join("testdata", "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	topics, err := LoadTopics(filepath.Join("testdata", "topics.json"))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := LoadFacts(filepath.Join("testdata", "facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := os.ReadFile(filepath.Join("testdata", "facts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	factOwners, err := ParseFactOwners(declarations)
	if err != nil {
		t.Fatal(err)
	}
	return routingFixture{graph: graph, index: NewGraphIndex(graph), owners: owners, topics: topics, facts: facts, factOwners: factOwners}
}

func (f routingFixture) subscriptions(t *testing.T, owner string) []string {
	t.Helper()
	role := ""
	if graphOwner, ok := f.owners.ByID(owner); ok {
		role = graphOwner.Role
	}
	if owner == "librarian" {
		role = "librarian"
	}
	topics, err := Subscriptions(owner, role, f.topics, f.facts, f.factOwners)
	if err != nil {
		t.Fatal(err)
	}
	return topics
}

func (f routingFixture) receivers(t *testing.T, delivery Delivery) []string {
	t.Helper()
	switch delivery.Audience {
	case AudienceOwner:
		return []string{delivery.Receiver}
	case AudienceLinked:
		return f.index.Linked(delivery.Node)
	case AudienceNeighbors:
		return f.index.Neighbors(delivery.Node)
	case AudienceAll:
		// Every owner: the graph's, each fact owner and the librarian.
		ids := []string{"librarian"}
		for owner := range f.factOwners {
			ids = append(ids, owner)
		}
		for _, owner := range f.owners.Owners {
			ids = append(ids, owner.ID)
		}
		return ids
	default:
		t.Fatalf("delivery %+v has no audience", delivery)
		return nil
	}
}

func TestEveryGraphEdgeReceiverSubscribesToEveryRoute(t *testing.T) {
	fixture := loadRoutingFixture(t)
	routing, err := NewRouting(fixture.topics.Prefix, fixture.factOwners)
	if err != nil {
		t.Fatal(err)
	}

	type edge struct{ source, target, targetKind string }
	var edges []edge
	for _, graphEdge := range fixture.index.NeedEdges() {
		target, ok := fixture.owners.ByID(graphEdge.Target)
		if !ok {
			t.Fatalf("edge target %q has no owner", graphEdge.Target)
		}
		edges = append(edges, edge{graphEdge.Source, graphEdge.Target, target.Kind})
	}
	for owner, name := range fixture.factOwners {
		for _, user := range fixture.facts[name].Users {
			edges = append(edges, edge{user, owner, ""})
		}
	}
	if len(edges) != 5 {
		t.Fatalf("fixture has %d need edges, want four graph edges and one fact edge: %+v", len(edges), edges)
	}

	for _, edge := range edges {
		edge := edge
		t.Run(edge.source+"->"+edge.target, func(t *testing.T) {
			for _, route := range []Route{
				{Step: RouteNeed, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind},
				{Step: RouteOffer, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind},
				{Step: RouteOffer, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind, Linked: edge.targetKind != "function" && edge.targetKind != ""},
				{Step: RouteReplyFromTarget, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind},
				{Step: RouteAccept, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind},
				{Step: RouteReplyFromSource, Source: edge.source, Target: edge.target, TargetKind: edge.targetKind},
			} {
				assertRouteReachesReceivers(t, fixture, routing, route)
			}
		})
	}

	owners := []string{"fact-stack-alignment"}
	for _, owner := range fixture.owners.Owners {
		owners = append(owners, owner.ID)
	}
	for _, owner := range owners {
		// A reply that names its premise owner goes to that owner, as its own item: a graph or a fact owner.
		for _, route := range []Route{{Step: RouteOwnerItem, Owner: owner}, {Step: RouteLibrarianAnswer, Owner: owner}, {Step: RouteReplyToPremise, Owner: owner}} {
			assertRouteReachesReceivers(t, fixture, routing, route)
		}
	}
	for _, owner := range fixture.owners.Owners {
		assertRouteReachesReceivers(t, fixture, routing, Route{Step: RouteProposal, Owner: owner.ID})
	}
	assertRouteReachesReceivers(t, fixture, routing, Route{Step: RouteConcept})
	assertRouteReachesReceivers(t, fixture, routing, Route{Step: RouteLanding})
}

func assertRouteReachesReceivers(t *testing.T, fixture routingFixture, routing Routing, route Route) {
	t.Helper()
	deliveries := routing.Topics(route)
	if len(deliveries) == 0 {
		t.Fatalf("%s %+v produced no delivery", route.Step, route)
	}
	if route.Linked && len(deliveries) < 2 {
		t.Fatalf("%s %+v with a linked claim produced %+v, want the linked topic too", route.Step, route, deliveries)
	}
	for _, delivery := range deliveries {
		receivers := fixture.receivers(t, delivery)
		if len(receivers) == 0 {
			t.Fatalf("%s delivery %+v has no receiver", route.Step, delivery)
		}
		for _, receiver := range receivers {
			if !contains(fixture.subscriptions(t, receiver), delivery.Topic) {
				t.Errorf("%s receiver %q does not subscribe to %q", route.Step, receiver, delivery.Topic)
			}
		}
	}
}

// A routing table always has its topic prefix: no notice is addressed under a default the deployment did not name.
func TestNewRoutingRefusesAPrefixOutsideTheNamespace(t *testing.T) {
	for _, prefix := range []string{"", "notifications.", "notifications.example", "example."} {
		if _, err := NewRouting(prefix, nil); err == nil || !strings.Contains(err.Error(), "prefix") {
			t.Errorf("NewRouting(%q) error = %v, want a refused prefix", prefix, err)
		}
	}
	routing, err := NewRouting("notifications.example-message-test.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := routing.NeedsTopic("struct:state"); got != "notifications.example-message-test.node.struct:state.needs" {
		t.Fatalf("NeedsTopic = %q", got)
	}
}

func TestLoadGraphDocumentsRefuseAnotherFormat(t *testing.T) {
	if _, err := LoadOwners(filepath.Join("testdata", "graph.json")); err == nil {
		t.Fatal("LoadOwners accepted a graph document")
	}
	if _, err := LoadTopics(filepath.Join("testdata", "owners.json")); err == nil {
		t.Fatal("LoadTopics accepted an owners document")
	}
}
