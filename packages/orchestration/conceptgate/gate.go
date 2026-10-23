package conceptgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// Proposal is a proposed shared concept: its registry entry, and a Lean module that declares it against the
// project's build.
type Proposal struct {
	ID      string  `json:"id"`
	Concept Concept `json:"concept"`
	Source  string  `json:"source"`
}

// Outcome is the gate's decision.
type Outcome string

const (
	// Refused: an exact duplicate of a registered concept or an existing declaration; Existing names it.
	Refused Outcome = "refused"
	// Near: the proposal restates a registered concept's fact; the librarian decides.
	Near Outcome = "near"
	// Added: nothing in the registry or the index states it; it is appended to the registry.
	Added Outcome = "added"
	// NameTaken: the name is a registered concept's or an existing declaration's. The statement may be new; the
	// proposal needs another name. Existing names the holder.
	NameTaken Outcome = "name-taken"
)

// Match is one registered fact that the proposal's own text states.
type Match struct {
	Concept     string `json:"concept"`
	How         string `json:"how"`
	Declaration string `json:"declaration"`
}

// Verdict is the gate's answer to one proposal.
type Verdict struct {
	Proposal     string  `json:"proposal"`
	Name         string  `json:"name"`
	Outcome      Outcome `json:"outcome"`
	Existing     string  `json:"existing,omitempty"`
	ExistingAt   string  `json:"existing_at,omitempty"`
	Reason       string  `json:"reason"`
	Matches      []Match `json:"matches,omitempty"`
	CompiledHash string  `json:"compiled_hash,omitempty"`
}

// Librarian receives the near matches, and only them.
type Librarian interface {
	Review(ctx context.Context, v Verdict, p Proposal) error
}

// CommandLibrarian hands a near match to a command (the deployment's message to the librarian): it runs Command
// with {"verdict":…,"proposal":…} on its standard input.
type CommandLibrarian struct{ Command []string }

// Review runs the command.
func (c CommandLibrarian) Review(ctx context.Context, v Verdict, p Proposal) error {
	if len(c.Command) == 0 {
		return fmt.Errorf("no librarian command is configured, so near match %s cannot reach the librarian", p.ID)
	}
	body, err := json.Marshal(map[string]any{"verdict": v, "proposal": p})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("librarian command: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Gate is the similarity gate in front of the librarian.
type Gate struct {
	Index    *declindex.Store
	Tree     declindex.Tree
	Registry string
	// Compiled reads the proposal; its Forms are replaced by the registry's at each decision.
	Compiled  *declindex.CompiledOptions
	Librarian Librarian
	// WorkDir holds the compiled proposals.
	WorkDir string
}

// ProposalModule is the module name a proposal is compiled as.
const ProposalModule = "DeclIndexProposal"

// Forms gives the registry's forms module, importing the module of each registered concept that the index has.
func Forms(ctx context.Context, s *declindex.Store, r Registry) (declindex.FormsModule, error) {
	fm := r.FormsModule()
	seen := map[string]bool{}
	for _, i := range fm.Imports {
		seen[i] = true
	}
	for _, c := range r.Concepts {
		found, err := s.Resolve(ctx, c.Name)
		if err != nil {
			return fm, err
		}
		for _, f := range found {
			if f.Name == c.Name && !seen[f.Module] {
				seen[f.Module] = true
				fm.Imports = append(fm.Imports, f.Module)
			}
		}
	}
	return fm, nil
}

// Decide compares a proposal with the registry and the index: it refuses an exact duplicate and names the
// existing concept, sends a near match to the librarian, and adds anything else to the registry. An error means
// the gate could not decide: the proposal is neither refused nor added.
func (g *Gate) Decide(ctx context.Context, p Proposal) (Verdict, error) {
	v := Verdict{Proposal: p.ID, Name: p.Concept.Name}
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Source) == "" {
		return v, fmt.Errorf("a proposal needs an id and a Lean source that declares it")
	}
	if err := p.Concept.check(); err != nil {
		return v, err
	}
	if err := checkProposalSource(p.Source); err != nil {
		return v, err
	}
	if d, err := g.Index.Meta(ctx, "compiled"); err != nil {
		return v, err
	} else if d == "" {
		return v, fmt.Errorf("the index was read without its compiled part, so the gate has no compiled statements to compare")
	}
	reg, err := Load(g.Registry)
	if err != nil {
		return v, err
	}
	if p.Concept.Extends != "" {
		if _, ok := reg.Find(p.Concept.Extends); !ok {
			return v, fmt.Errorf("concept %s extends %s, which is not registered", p.Concept.Name, p.Concept.Extends)
		}
	}
	refuse := func(existing, at, reason string) (Verdict, error) {
		v.Outcome, v.Existing, v.ExistingAt, v.Reason = Refused, existing, at, reason
		return v, nil
	}
	taken := func(existing, at, reason string) (Verdict, error) {
		v.Outcome, v.Existing, v.ExistingAt, v.Reason = NameTaken, existing, at, reason
		return v, nil
	}
	if c, ok := reg.Find(p.Concept.Name); ok {
		return taken(c.Name, c.Home, "a concept of this name is registered")
	}
	if found, err := g.Index.Resolve(ctx, p.Concept.Name); err != nil {
		return v, err
	} else {
		for _, f := range found {
			if f.Name == p.Concept.Name {
				return taken(f.Name, f.At(), "a declaration of this name exists")
			}
		}
	}
	consts, err := g.export(ctx, reg, p)
	if err != nil {
		return v, err
	}
	own := map[string]bool{}
	var decl *declindex.Constant
	for i, c := range consts {
		own[c.Name] = true
		if c.Name == p.Concept.Name {
			decl = &consts[i]
		}
	}
	if decl == nil {
		return v, fmt.Errorf("the proposal's source does not declare %s", p.Concept.Name)
	}
	for _, c := range consts {
		if c.Truncated {
			return v, fmt.Errorf("the walk of %s hit its node budget, so the gate cannot decide", c.Name)
		}
	}
	v.CompiledHash = decl.Hash
	// Exact duplicates: a registered concept first, by its compiled statement in the index or, before it lands,
	// the one the gate saved when it added it; then any declaration of the project. A concept with neither and no
	// form cannot be compared.
	var unknown []string
	for _, c := range reg.Concepts {
		rc, err := g.Index.Constant(ctx, c.Name)
		var nf *declindex.NotFoundError
		switch {
		case errors.As(err, &nf):
			rc = declindex.Compiled{}
		case err != nil:
			return v, err
		}
		if (rc.Hash != "" && rc.Hash == decl.Hash) || (c.CompiledHash != "" && c.CompiledHash == decl.Hash) {
			return refuse(c.Name, c.Home, "the compiled statement equals the registered concept's")
		}
		if rc.Hash == "" && c.CompiledHash == "" && len(c.Forms) == 0 {
			unknown = append(unknown, c.Name)
		}
	}
	same, err := g.Index.SameCompiled(ctx, decl.Hash)
	if err != nil {
		return v, err
	}
	if len(same) > 0 {
		return refuse(same[0].Name, same[0].At(), "the compiled statement equals an existing declaration's")
	}
	// Near matches: a registered fact that the proposal's own text states, directly or through a definition the
	// proposal declares. Applying a registered concept or an existing definition reuses it. A definition of the
	// proposal that only wraps a registered concept is also routed to the librarian: the matcher cannot tell its
	// template from a restatement, and a false near costs a review, never a duplicate.
	for _, c := range consts {
		for _, f := range c.Facts {
			if f.How == "direct" || (strings.HasPrefix(f.How, "via:") && own[strings.TrimPrefix(f.How, "via:")]) {
				v.Matches = append(v.Matches, Match{Concept: f.Concept, How: f.How, Declaration: c.Name})
			}
		}
	}
	if len(v.Matches) > 0 || len(unknown) > 0 {
		sort.Slice(v.Matches, func(i, j int) bool {
			a, b := v.Matches[i], v.Matches[j]
			return a.Concept+a.Declaration+a.How < b.Concept+b.Declaration+b.How
		})
		v.Outcome, v.Reason = Near, "the proposal restates a registered concept's fact"
		if len(v.Matches) == 0 {
			v.Reason = "the gate cannot compare the proposal with " + strings.Join(unknown, ", ") +
				": registered, not declared in the index, with no form and no compiled hash"
		}
		if err := g.Librarian.Review(ctx, v, p); err != nil {
			return v, err
		}
		return v, nil
	}
	c := p.Concept
	c.Proposal = p.ID
	c.CompiledHash = decl.Hash
	if err := Add(g.Registry, c); err != nil {
		return v, err
	}
	v.Outcome, v.Reason = Added, "no registered concept or existing declaration states it"
	return v, nil
}

// export compiles the proposal against the build and reads it with the registry's forms.
func (g *Gate) export(ctx context.Context, reg Registry, p Proposal) ([]declindex.Constant, error) {
	if err := os.MkdirAll(g.WorkDir, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(g.WorkDir, "proposal-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, ProposalModule+".lean"), []byte(p.Source), 0o644); err != nil {
		return nil, err
	}
	co := *g.Compiled
	co.Dir = filepath.Join(dir, "forms")
	if co.Forms, err = Forms(ctx, g.Index, reg); err != nil {
		return nil, err
	}
	if err := declindex.CompileModule(ctx, co.Exporter.Lean, co.Exporter.SearchPath, dir, ProposalModule); err != nil {
		return nil, fmt.Errorf("the proposal does not compile: %w", err)
	}
	roots := declindex.Roots{ProposalModule: true}
	for r := range g.Tree.Roots {
		roots[r] = true
	}
	return co.Export(ctx, roots, []string{ProposalModule}, []string{dir})
}
