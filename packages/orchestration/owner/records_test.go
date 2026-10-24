package owner

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRecordsParseSpec(t *testing.T) {
	data, err := os.ReadFile("testdata/records/spec.md")
	if err != nil {
		t.Fatal(err)
	}

	front, claims, err := ParseSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	wantFront := SpecFront{
		Owner:       "function:ns_match",
		Revision:    2,
		Epoch:       1,
		Complete:    false,
		Answers:     []string{"need:function:ns_match->struct:ns_common@r1"},
		Resolves:    []string{"reply:function:ns_match->struct:ns_common@abc123def456"},
		Retired:     []string{"C7"},
		AuthorModel: "openai-codex/gpt-5.6-terra",
	}
	if !reflect.DeepEqual(front, wantFront) {
		t.Fatalf("ParseSpec front = %#v, want %#v", front, wantFront)
	}
	wantClaims := []Claim{
		{
			ID:    "C1",
			Depth: 1,
			Text:  "ns_match returns 1 when the namespace number and device match.",
			Uses:  []string{"struct:ns_common#C2", "global:nsfs_mnt@fs/nsfs.c#C1"},
			By:    []string{"code fs/nsfs.c:400"},
		},
		{
			ID:    "C2",
			Depth: 2,
			Text:  "The namespace number read returns the value that initialization wrote.",
			Uses:  []string{"struct:ns_common#C2"},
			By:    []string{"uses"},
		},
	}
	if !reflect.DeepEqual(claims, wantClaims) {
		t.Fatalf("ParseSpec claims = %#v, want %#v", claims, wantClaims)
	}
}

func TestRecordsParseSpecRefusesUnknownFrontKey(t *testing.T) {
	data, err := os.ReadFile("testdata/records/unknown-front.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ParseSpec(data)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("ParseSpec unknown front key error = %v", err)
	}
}

func TestRecordsLintClaims(t *testing.T) {
	tests := []struct {
		name     string
		cur      []Claim
		retired  []string
		wantLine int
		contains string
	}{
		{
			name:     "duplicate id names the second claim line",
			cur:      []Claim{{ID: "C1", Text: "first"}, {ID: "C1", Text: "duplicate"}},
			wantLine: 2,
			contains: "duplicate",
		},
		{
			name:     "an identifier retired in an earlier revision is never used again",
			cur:      []Claim{{ID: "C1", Text: "the kept claim"}, {ID: "C2", Depth: 2, Text: "a later claim under a retired id"}},
			retired:  []string{"C2"},
			wantLine: 2,
			contains: "retired",
		},
		{
			name:     "unresolved local use",
			cur:      []Claim{{ID: "C1", Text: "a claim", Uses: []string{"C9"}}},
			wantLine: 1,
			contains: "C9",
		},
		{
			name:     "malformed remote use",
			cur:      []Claim{{ID: "C1", Text: "a claim", Uses: []string{"struct:ns_common#not-a-claim"}}},
			wantLine: 1,
			contains: "remote",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := LintClaims(tc.cur, tc.retired)
			if len(errs) == 0 {
				t.Fatal("LintClaims returned no errors")
			}
			if errs[0].Line != tc.wantLine || !strings.Contains(errs[0].Error(), tc.contains) {
				t.Fatalf("LintClaims = %#v, want first error at line %d containing %q", errs, tc.wantLine, tc.contains)
			}
		})
	}
}

// A claim keeps its id while its owner revises it: only an id that left the tree is retired, and every id retired
// once stays retired.
func TestRetiredIDsAccumulateEveryRemovedClaim(t *testing.T) {
	prev := []Claim{{ID: "C1", Text: "kept"}, {ID: "C2", Text: "removed now"}, {ID: "C3", Text: "edited"}}
	cur := []Claim{{ID: "C1", Text: "kept"}, {ID: "C3", Text: "edited in place"}, {ID: "C4", Text: "new"}}
	if got, want := RetiredIDs([]string{"C7"}, prev, cur), []string{"C2", "C7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RetiredIDs = %v, want %v", got, want)
	}
	if got := RetiredIDs(nil, nil, cur); got == nil || len(got) != 0 {
		t.Fatalf("RetiredIDs of a first revision = %#v, want an empty list", got)
	}
	if errs := LintClaims(cur, []string{"C2", "C7"}); len(errs) != 0 {
		t.Fatalf("LintClaims refused a claim edited in place under its id: %v", errs)
	}
}

func TestRecordsMarshalRoundTripsOrigin(t *testing.T) {
	at := time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC)
	need := NeedFront{
		Edge:        "function:ns_match->struct:ns_common",
		Source:      "function:ns_match",
		Target:      "struct:ns_common",
		Origin:      "neighbor",
		Revision:    2,
		Epoch:       1,
		Cites:       []string{"C2"},
		Resolves:    []string{"reply:function:ns_match->struct:ns_common@abc123def456"},
		Retired:     []string{"C3"},
		AuthorModel: "openai-codex/gpt-5.6-terra",
	}
	review := Review{
		Subject:  "spec",
		Revision: 1,
		Pass:     1,
		Model:    "openai-codex/gpt-5.6-terra",
		Findings: []Finding{{
			ID:       "F1",
			Severity: "owner",
			Text:     "The spec does not state the lock rule.",
			Origin:   "review",
			Node:     "struct:ns_common",
		}},
	}
	upstreamReview := UpstreamReview{
		Contract: "spec:struct:ns_common",
		Reviewer: "struct:ns_common",
		Upstream: "0123456789ab",
		Head:     "abcdef0123456789abcdef0123456789abcdef01",
		FromHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ToHash:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Verdict:  "accept",
		Passes: []ReviewPass{{
			Pass:  1,
			Model: "openai-codex/gpt-5.6-terra",
			Findings: []Finding{{
				ID:       "F2",
				Severity: "stop",
				Text:     "The invariant changes the published ownership rule.",
				Origin:   "review",
				Node:     "struct:ns_common",
			}},
		}},
		At: at,
	}

	cases := []struct {
		name string
		in   any
		out  func() any
	}{
		{"need", need, func() any { return new(NeedFront) }},
		{"reply", Reply{Kind: "reply", Edge: need.Edge, Reason: "missing-premise", PremiseOwner: "subsystem:mount", Answers: map[string]string{"need": "sha256:need", "offer": "sha256:offer"}, Epoch: 1, At: at, Text: "The lock invariant needs a shared premise."}, func() any { return new(Reply) }},
		{"edge", EdgeRecord{Edge: need.Edge, Source: "function:ns_match", Target: "struct:ns_common", Status: "agreed", Need: EdgeNeed{Path: "skeleton/nodes/K/needs/E.md", Hash: "sha256:need"}, Offer: EdgeOffer{SpecHash: "sha256:offer"}, Agreement: "edge:function:ns_match->struct:ns_common@1", Lean: EdgeLean{NeedDecl: "Skeleton.Need", OfferDecl: "Skeleton.Offer", Module: "Linux/Skeleton/K/E.lean", Theorem: "Skeleton.edge", Cites: []string{"Contract"}}, TranslationReviews: []string{"reviews/1-1.yaml"}, BuiltAt: at}, func() any { return new(EdgeRecord) }},
		{"review", review, func() any { return new(Review) }},
		{"upstream review", upstreamReview, func() any { return new(UpstreamReview) }},
		{"node metadata", NodeMeta{ID: "struct:ns_common", Key: "K9c01e86f8749cad3", Kind: "struct", Role: "data-structure", LeanDir: "Linux/kernel/nscommon", CFile: "kernel/nscommon.c", Active: true, Parent: "subsystem:mount", Children: []string{"struct:ns_common~cache"}, ContractDecl: "Skeleton.K9c01e86f8749cad3.contract"}, func() any { return new(NodeMeta) }},
		{"escalation", Escalation{Subject: "reply:function:ns_match->struct:ns_common@abc123def456", Record: "skeleton/nodes/K/replies/abc.yaml", Reason: "program-defect", Counterexample: "ns_match returns 0 for matching inputs.", BugsRow: "| ns_match | matching inputs return 0 |"}, func() any { return new(Escalation) }},
		{"produced", Produced{Interface: "NsCommonIf", Module: "Linux/kernel/nscommon/Model.lean", Declaration: "nsCommonIf", Migration: "m:K9c01e86f8749cad3:1"}, func() any { return new(Produced) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonData, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			jsonOut := tc.out()
			if err := json.Unmarshal(jsonData, jsonOut); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			assertRecordEqual(t, tc.in, jsonOut)

			yamlData, err := yaml.Marshal(tc.in)
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			yamlOut := tc.out()
			if err := yaml.Unmarshal(yamlData, yamlOut); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			assertRecordEqual(t, tc.in, yamlOut)

			record, ok := tc.in.(interface{ Marshal() ([]byte, error) })
			if !ok {
				t.Fatalf("%T does not implement Marshal", tc.in)
			}
			recordData, err := record.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			recordOut := tc.out()
			decoded, ok := recordOut.(interface{ Unmarshal([]byte) error })
			if !ok {
				t.Fatalf("%T does not implement Unmarshal", recordOut)
			}
			if err := decoded.Unmarshal(recordData); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			assertRecordEqual(t, tc.in, recordOut)
		})
	}
}

func TestRecordsRefuseInvalidReasons(t *testing.T) {
	var reply Reply
	if err := yaml.Unmarshal([]byte("kind: reply\nedge: function:ns_match->struct:ns_common\nreason: maybe\nepoch: 1\nat: 2026-10-23T12:00:00Z\ntext: unsupported reason\n"), &reply); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("reply with reason maybe error = %v", err)
	}

	var upstream UpstreamReview
	if err := json.Unmarshal([]byte(`{"contract":"spec:struct:ns_common","reviewer":"struct:ns_common","upstream":"0123456789ab","head":"abcdef0123456789abcdef0123456789abcdef01","from_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","to_hash":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","verdict":"refuse","at":"2026-10-23T12:00:00Z"}`), &upstream); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("refused upstream review without a reason error = %v", err)
	}
}

// Measure 6 counts each requirement by where it came from, and the record kind decides it: a need comes from a
// neighbor, a review finding from the review of the node's own spec. The field is required, never defaulted, and
// a need's source and target stay the edge's endpoints (the P3 record format).
func TestRecordsRequireTheOriginOfTheirKind(t *testing.T) {
	const need = "edge: function:ns_match->struct:ns_common\nsource: function:ns_match\ntarget: struct:ns_common\nrevision: 1\nepoch: 1\nauthor_model: openai-codex/gpt-5.6-terra\n"
	for name, tc := range map[string]struct{ yaml, contains string }{
		"no origin":                       {need, "origin"},
		"a review origin":                 {need + "origin: review\n", "origin"},
		"a source that is not the edge's": {strings.Replace(need, "source: function:ns_match", "source: neighbor", 1) + "origin: neighbor\n", "source"},
		"a target that is not the edge's": {strings.Replace(need, "target: struct:ns_common", "target: function:other", 1) + "origin: neighbor\n", "target"},
		"the retired node field":          {need + "origin: neighbor\nnode: struct:ns_common\n", "node"},
	} {
		var front NeedFront
		if err := yaml.Unmarshal([]byte(tc.yaml), &front); err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Errorf("need with %s: error = %v, want one naming %q", name, err, tc.contains)
		}
	}
	var front NeedFront
	if err := yaml.Unmarshal([]byte(need+"origin: neighbor\n"), &front); err != nil || front.Origin != "neighbor" || front.Source != "function:ns_match" {
		t.Fatalf("need with origin neighbor = %+v, %v", front, err)
	}

	const finding = "subject: spec\nrevision: 1\npass: 1\nmodel: openai-codex/gpt-5.6-terra\nfindings:\n  - id: F1\n    severity: owner\n    text: The spec omits the lock rule.\n    node: struct:ns_common\n"
	for name, tc := range map[string]string{"no origin": finding, "a neighbor origin": finding + "    origin: neighbor\n", "the source field": finding + "    origin: review\n    source: review\n"} {
		var review Review
		if err := yaml.Unmarshal([]byte(tc), &review); err == nil {
			t.Errorf("review finding with %s was accepted: %+v", name, review)
		}
	}
	var review Review
	if err := yaml.Unmarshal([]byte(finding+"    origin: review\n"), &review); err != nil || review.Findings[0].Origin != "review" {
		t.Fatalf("review finding with origin review = %+v, %v", review, err)
	}
}

func TestRecordsContentHash(t *testing.T) {
	if got, want := ContentHash([]byte("record")), "sha256:70ce871f8a3d3fb449bc3c3ace6547cef02dfc74ffe48d912532a724bfdbe5b9"; got != want {
		t.Fatalf("ContentHash = %q, want %q", got, want)
	}
}

func assertRecordEqual(t *testing.T, want, got any) {
	t.Helper()
	value := reflect.ValueOf(got)
	if value.Kind() != reflect.Ptr || !reflect.DeepEqual(want, value.Elem().Interface()) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}
