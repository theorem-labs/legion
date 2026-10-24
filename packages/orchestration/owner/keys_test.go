package owner

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestKeyVectors(t *testing.T) {
	for id, want := range map[string]string{
		"function:ns_match":                      "Kb401aaa780da5ad4",
		"struct:ns_common":                       "K9c01e86f8749cad3",
		"function:get_mountpoint@fs/namespace.c": "Kd95dbe5387b35fb1",
		"struct:mountpoint":                      "K135ea7ac23fb1090",
		"function:negative-control":              "Ka206f968b8e0c670",
	} {
		if got := Key(id); got != want {
			t.Errorf("Key(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestEdgeIDRoundTrip(t *testing.T) {
	source := "function:get_mountpoint@fs/namespace.c"
	target := "struct:mountpoint"
	edge := EdgeID(source, target)
	if edge != source+"->"+target {
		t.Fatalf("EdgeID = %q", edge)
	}
	gotSource, gotTarget, err := ParseEdgeID(edge)
	if err != nil {
		t.Fatal(err)
	}
	if gotSource != source || gotTarget != target {
		t.Fatalf("ParseEdgeID(%q) = (%q, %q), want (%q, %q)", edge, gotSource, gotTarget, source, target)
	}
	for _, malformed := range []string{"", "source", "->target", "source->", "a->b->c"} {
		if _, _, err := ParseEdgeID(malformed); err == nil {
			t.Errorf("ParseEdgeID(%q) succeeded", malformed)
		}
	}
}

func TestTopicTokenPercentEncodesNonGraphBytes(t *testing.T) {
	id := "function:get_mountpoint@fs/namespace.c"
	if got, want := TopicToken(id), "function:get_mountpoint%40fs%2Fnamespace%2Ec"; got != want {
		t.Fatalf("TopicToken(%q) = %q, want %q", id, got, want)
	}
}

// The fixture is a topics.json in the graph tool's cgraph/topics/v1 shape, its tokens spelled by its token_rule, with
// ids that hold `@`, `/` and `.` as source-file ids do.
func TestTopicTokenMatchesGraphTool(t *testing.T) {
	data, err := os.ReadFile("testdata/topics-tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var topics struct {
		Prefix string              `json:"prefix"`
		Owners map[string][]string `json:"owners"`
	}
	if err := json.Unmarshal(data, &topics); err != nil {
		t.Fatal(err)
	}
	if len(topics.Owners) == 0 {
		t.Fatal("topics fixture has no owners")
	}
	for owner, subscriptions := range topics.Owners {
		want := topics.Prefix + "node." + TopicToken(owner) + ".needs"
		if !slices.Contains(subscriptions, want) {
			t.Errorf("%s lacks %s", owner, want)
		}
	}
}

func TestItemIDRoundTripEveryType(t *testing.T) {
	edge := EdgeID("function:get_mountpoint@fs/namespace.c", "struct:mountpoint")
	for _, want := range []ItemRef{
		{Type: "start", Subject: "function:ns_match", Version: "1"},
		{Type: "need", Subject: edge, Version: "r12"},
		{Type: "offer", Subject: edge, Version: "r12"},
		{Type: "reply", Subject: edge, Version: "a1b2c3d4e5f6"},
		{Type: "agreed", Subject: edge, Version: "2"},
		{Type: "lean", Subject: edge, Version: "a1b2c3d4e5f6"},
		{Type: "stale", Subject: "edge:" + edge + "@2"},
		{Type: "returned", Subject: "m:Kd95dbe5387b35fb1:3", Version: "4411"},
		{Type: "rebuild", Subject: "Linux/Skeleton/Kb401aaa780da5ad4/Node.lean", Version: "a1b2c3d4e5f6"},
		{Type: "produce", Subject: "NsCommonIf", Version: "a1b2c3d4e5f6"},
		{Type: "interface", Subject: "0123456789abcdef", Version: "1"},
		{Type: "upstream", Upstream: "94a0cd19e2ab", Subject: "6f1c2d3e4a5b6c7d"},
		{Type: "upstream-review", Upstream: "94a0cd19e2ab", Subject: "LinuxMmu.Spec.nsMatchSpec"},
		{Type: "upstream-review", Upstream: "94a0cd19e2ab", Subject: "LinuxMmu.Spec.nsMatchSpec", Version: "2"},
		{Type: "upstream-review", Upstream: "94a0cd19e2ab", Subject: "spec:function:get_mountpoint@fs/namespace.c"},
		{Type: "upstream-review", Upstream: "94a0cd19e2ab", Subject: "spec:function:get_mountpoint@fs/namespace.c", Version: "3"},
		{Type: "concept", Subject: "function:ns_match", Version: "near-0123456789abcdef"},
		{Type: "concept", Subject: "function:get_mountpoint@fs/namespace.c", Version: "near-0123456789abcdef"},
		{Type: "concept", Subject: "fact-stack-alignment", Version: "recheck-0123456789abcdef0123456789abcdef-fedcba9876543210fedcba9876543210"},
		{Type: "concept", Subject: "function:ns_match", Version: "recheck-0123456789abcdef0123456789abcdef-name.46782e5370416c69676e6564"},
		{Type: "concept-answer", Subject: "function:get_mountpoint@fs/namespace.c", Version: "near-0123456789abcdef"},
		{Type: "concept-answer", Subject: "function:ns_match", Version: "recheck-0123456789abcdef0123456789abcdef-name.46782e5370416c69676e6564"},
		{Type: "split-review", Subject: "ns-match-child", Version: "r2"},
		{Type: "proposal", Subject: "protocol-v2"},
		{Type: "escalation", Subject: "0123456789abcdef"},
		{Type: "decision", Subject: "0123456789abcdef"},
	} {
		got, err := ParseItemID(want.String())
		if err != nil {
			t.Errorf("ParseItemID(%q): %v", want.String(), err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseItemID(%q) = %+v, want %+v", want.String(), got, want)
		}
	}
}

func TestItemIDRejectsInvalidLayout(t *testing.T) {
	for _, id := range []string{
		"",
		"unknown:item",
		"start:function:ns_match@2",
		"need:not-an-edge@r1",
		"offer:function:a->struct:b@r0",
		"reply:function:a->struct:b@bad",
		"agreed:function:a->struct:b@0",
		"lean:function:a->struct:b@not-a-hash",
		"stale:not-an-agreement",
		"returned:m:Kshort:1@0",
		"rebuild:Linux/Skeleton/Node.lean@short",
		"produce:NsCommonIf@short",
		"interface:short@1",
		"upstream:short:6f1c2d3e4a5b6c7d",
		"upstream-review:short:LinuxMmu.Spec.nsMatchSpec",
		// A concept item's version is the gate's id: near-<hex16> or recheck-<key>-<key> (no hex12, no other key).
		"concept:function:ns_match@a1b2c3d4e5f6",
		"concept:function:ns_match@short",
		"concept:function:ns_match@near-0123456789ABCDEF",
		"concept:function:ns_match@near-0123456789abcde",
		"concept:function:ns_match@recheck-0123456789abcdef0123456789abcdef",
		"concept:function:ns_match@recheck-0123456789abcdef0123456789abcdef-name.",
		"concept:function:ns_match@recheck-0123456789abcdef0123456789abcdef-name.467",
		"concept:function:ns_match@recheck-0123456789abcdef0123456789abcdef-name.46782e-5370",
		"concept:function:ns_match@recheck-0123456789abcdef0123456789abcdef-0123",
		"concept-answer:function:ns_match@a1b2c3d4e5f6",
		// The proposer passes the gate's owner-id rule: a node id of the supervisor's kinds, or a role name.
		"concept:Function:ns_match@near-0123456789abcdef",
		"concept:file:fs/nsfs.c@near-0123456789abcdef",
		"concept:function:@near-0123456789abcdef",
		"concept-answer:Librarian@near-0123456789abcdef",
		"split-review:ns-match-child@r0",
		"proposal:one@two",
		"escalation:short",
		"decision:short",
		// A decimal version is canonical: a leading zero would make a second id for the same version.
		"need:function:a->struct:b@r01",
		"offer:function:a->struct:b@r007",
		"split-review:ns-match-child@r02",
		"agreed:function:a->struct:b@02",
		"returned:m:Kd95dbe5387b35fb1:3@04411",
		"returned:m:Kd95dbe5387b35fb1:03@4411",
		"stale:edge:function:a->struct:b@02",
	} {
		if _, err := ParseItemID(id); err == nil {
			t.Errorf("ParseItemID(%q) succeeded", id)
		}
	}
	// An upstream review's version is canonical too: @02 is no version, so it stays part of the contract name.
	if ref, err := ParseItemID("upstream-review:94a0cd19e2ab:LinuxMmu.Spec.nsMatchSpec@02"); err != nil || ref.Version != "" {
		t.Errorf("ParseItemID(upstream-review …@02) = %+v, %v; want no version", ref, err)
	}
}

func TestParseRevisionIsCanonical(t *testing.T) {
	for version, want := range map[string]int{"r1": 1, "r12": 12, "r100": 100} {
		if got, err := ParseRevision(version); err != nil || got != want {
			t.Errorf("ParseRevision(%q) = %d, %v; want %d", version, got, err, want)
		}
	}
	for _, version := range []string{"", "r", "r0", "r01", "1", "R1", "r1x", "r-1", "r+1"} {
		if got, err := ParseRevision(version); err == nil {
			t.Errorf("ParseRevision(%q) = %d, want an error", version, got)
		}
	}
}
