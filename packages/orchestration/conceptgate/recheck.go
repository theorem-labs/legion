package conceptgate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// RecheckNear is one near pair found among the additions of a landing.
type RecheckNear struct {
	ID    string `json:"id"`
	Added string `json:"added"`
	Other string `json:"other"`
}

// RecheckReport is the registry part of the post-landing check.
type RecheckReport struct {
	Defects []Defect      `json:"defects,omitempty"`
	Near    []RecheckNear `json:"near,omitempty"`
}

// Recheck recompiles each added concept's stored source in the sandbox, checks its recorded hash, and sends each
// near pair to the librarian once. The caller supplies paths added by the landing diff and the loaded registry.
func Recheck(ctx context.Context, g *Gate, reg Registry, added []string) (RecheckReport, error) {
	var report RecheckReport
	check := g.CheckSandbox
	if check == nil {
		check = declindex.CheckSandbox
	}
	if err := check(ctx, g.Compiled.Exporter.Lean); err != nil {
		return report, fmt.Errorf("the recheck compiles stored source only in the sandbox: %w", err)
	}
	byKey := map[string]Concept{}
	byName := map[string]Concept{}
	for _, c := range reg.Concepts {
		byKey[Key(c)] = c
		byName[c.Name] = c
	}
	seenPairs := map[string]bool{}
	for _, path := range added {
		key := strings.TrimSuffix(filepath.Base(path), ".yaml")
		concept, ok := byKey[key]
		if !ok {
			report.Defects = append(report.Defects, Defect{Path: path, Message: "added file is not a registered concept"})
			continue
		}
		if concept.Source == "" {
			report.Defects = append(report.Defects, Defect{Path: path, Message: "added concept has no stored source"})
			continue
		}
		proposal := Proposal{ID: concept.Proposal, Concept: concept, Source: concept.Source}
		constants, err := g.export(ctx, reg, proposal)
		if err != nil {
			return report, fmt.Errorf("recheck %s: %w", concept.Name, err)
		}
		own := map[string]bool{}
		var actual string
		for _, constant := range constants {
			own[constant.Name] = true
			if constant.Name == concept.Name {
				actual = constant.Hash
			}
		}
		if actual == "" {
			report.Defects = append(report.Defects, Defect{Path: path, Message: "stored source does not declare " + concept.Name})
			continue
		}
		if actual != concept.CompiledHash {
			report.Defects = append(report.Defects, Defect{Path: path, Message: "compiled hash " + actual + " differs from recorded " + concept.CompiledHash})
			continue
		}
		accepted := map[string]bool{}
		for _, name := range concept.AcceptedNear {
			accepted[name] = true
		}
		for _, constant := range constants {
			for _, fact := range constant.Facts {
				if fact.How != "direct" && (!strings.HasPrefix(fact.How, "via:") || !own[strings.TrimPrefix(fact.How, "via:")]) {
					continue
				}
				other, ok := byName[fact.Concept]
				if !ok || other.Name == concept.Name || accepted[other.Name] {
					continue
				}
				for _, name := range other.AcceptedNear {
					if name == concept.Name {
						accepted[other.Name] = true
					}
				}
				if accepted[other.Name] {
					continue
				}
				pair := Key(concept) + "\x00" + Key(other)
				if Key(other) < Key(concept) {
					pair = Key(other) + "\x00" + Key(concept)
				}
				if seenPairs[pair] {
					continue
				}
				seenPairs[pair] = true
				id := "recheck-" + concept.CompiledHash + "-" + Key(other)
				match := Match{Concept: other.Name, How: fact.How, Declaration: constant.Name}
				verdict := Verdict{Proposal: concept.Proposal, Name: concept.Name, Outcome: Near, Reason: "an added concept restates a registered concept's fact", Matches: []Match{match}, CompiledHash: actual, VerdictID: id}
				report.Near = append(report.Near, RecheckNear{ID: id, Added: Key(concept), Other: Key(other)})
				if g.Librarian == nil {
					return report, fmt.Errorf("no librarian is configured")
				}
				if err := g.Librarian.Review(ctx, ReviewRequest{ID: id, Proposer: concept.Proposer, Kind: "recheck", Verdict: verdict, Proposal: proposal}); err != nil {
					return report, err
				}
			}
		}
	}
	return report, nil
}
