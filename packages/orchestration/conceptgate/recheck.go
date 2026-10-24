package conceptgate

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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
// An added file that is not a loaded concept, a stored source that Lean refuses or does not compile, and a pending
// concept left out of the forms are defects; an error means that the recheck could not run.
func Recheck(ctx context.Context, g *Gate, reg Registry, added []string) (RecheckReport, error) {
	var report RecheckReport
	if err := g.checkSandbox(ctx); err != nil {
		return report, fmt.Errorf("the recheck compiles stored source only in the sandbox: %w", err)
	}
	if _, err := g.compiledCommit(ctx); err != nil {
		return report, err
	}
	byKey := map[string]Concept{}
	byName := map[string]Concept{}
	for _, c := range reg.Concepts {
		byKey[Key(c)] = c
		byName[c.Name] = c
	}
	seenDefects := map[Defect]bool{}
	defect := func(d Defect) {
		if !seenDefects[d] {
			seenDefects[d] = true
			report.Defects = append(report.Defects, d)
		}
	}
	seenPairs := map[string]bool{}
	for _, path := range added {
		key := strings.TrimSuffix(filepath.Base(path), ".yaml")
		concept, ok := byKey[key]
		if !ok {
			defect(Defect{Path: path, Message: "added file is not a registered concept"})
			continue
		}
		if concept.Source == "" {
			defect(Defect{Path: path, Message: "added concept has no stored source"})
			continue
		}
		if err := ValidateOwnerID(concept.Proposer); err != nil {
			defect(Defect{Path: path, Message: err.Error()})
			continue
		}
		proposal := Proposal{ID: concept.Proposal, Concept: concept, Source: concept.Source}
		constants, pending, err := g.export(ctx, reg, proposal)
		for _, d := range pending {
			defect(d)
		}
		if err != nil {
			if !sourceFailed(err) {
				return report, fmt.Errorf("recheck %s: %w", concept.Name, err)
			}
			defect(Defect{Path: path, Message: "stored source does not compile: " + err.Error()})
			continue
		}
		own, decl := ownConstants(constants, concept.Name)
		if decl == nil {
			defect(Defect{Path: path, Message: "stored source does not declare " + concept.Name})
			continue
		}
		actual := decl.Hash
		if actual != concept.CompiledHash {
			defect(Defect{Path: path, Message: "compiled hash " + actual + " differs from recorded " + concept.CompiledHash})
			continue
		}
		addedKey := Key(concept)
		for _, constant := range constants {
			for _, fact := range constant.Facts {
				if !statesOwnFact(fact.How, own) {
					continue
				}
				other, ok := byName[fact.Concept]
				if !ok || other.Name == concept.Name || slices.Contains(concept.AcceptedNear, other.Name) ||
					slices.Contains(other.AcceptedNear, concept.Name) {
					continue
				}
				otherKey := Key(other)
				pair := min(addedKey, otherKey) + "\x00" + max(addedKey, otherKey)
				if seenPairs[pair] {
					continue
				}
				seenPairs[pair] = true
				id := recheckID(concept.CompiledHash, other)
				match := Match{Concept: other.Name, How: fact.How, Declaration: constant.Name}
				verdict := Verdict{Proposal: concept.Proposal, Name: concept.Name, Outcome: Near, Reason: "an added concept restates a registered concept's fact", Matches: []Match{match}, CompiledHash: actual, VerdictID: id}
				report.Near = append(report.Near, RecheckNear{ID: id, Added: addedKey, Other: otherKey})
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

// recheckID is the librarian id of the near pair of an added concept, by its checked compiled hash, and other, by
// its key in a form that the ledger accepts in an id (`recheck-[A-Za-z0-9._:@/-]+`): a compiled hash as it is, and
// the name that keys a concept without one as `name.` and the hexadecimal of its UTF-8 bytes, since a Lean name
// can hold any character. The prefix keeps a 16-byte name from making a compiled hash's id.
func recheckID(added string, other Concept) string {
	key := other.CompiledHash
	if key == "" {
		key = "name." + hex.EncodeToString([]byte(other.Name))
	}
	return "recheck-" + added + "-" + key
}
