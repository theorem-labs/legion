package conceptgate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// Added: nothing in the registry or the index states it; the gate returns a canonical addition.
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

// Addition is the one file that an owner writes and lands after an added verdict.
type Addition struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Base    string `json:"base"`
}

// Verdict is the gate's answer to one proposal.
type Verdict struct {
	Proposal     string    `json:"proposal"`
	Name         string    `json:"name"`
	Outcome      Outcome   `json:"outcome"`
	Existing     string    `json:"existing,omitempty"`
	ExistingAt   string    `json:"existing_at,omitempty"`
	Reason       string    `json:"reason"`
	Matches      []Match   `json:"matches,omitempty"`
	CompiledHash string    `json:"compiled_hash,omitempty"`
	VerdictID    string    `json:"verdict_id"`
	Addition     *Addition `json:"addition,omitempty"`
}

// DecideOptions are the identity and source bytes that bind a decision to the command invocation.
type DecideOptions struct {
	Proposer      string
	AcceptedBy    string
	ProposalBytes []byte
}

var (
	nodeOwnerID = regexp.MustCompile(`^(function|struct|global|percpu|subsystem):\S+$`)
	roleOwnerID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// ValidateOwnerID accepts the supervisor's node identifiers and role names.
func ValidateOwnerID(id string) error {
	if nodeOwnerID.MatchString(id) || roleOwnerID.MatchString(id) {
		return nil
	}
	return fmt.Errorf("owner id %q matches neither a node id nor a role name", id)
}

// Gate is the similarity gate in front of the librarian.
type Gate struct {
	Index        *declindex.Store
	Tree         declindex.Tree
	Registry     string
	AdditionRoot string
	// Compiled reads the proposal; its Forms are replaced by the registry's at each decision.
	Compiled  *declindex.CompiledOptions
	Librarian Librarian
	// WorkDir holds the compiled proposals and librarian request files.
	WorkDir string
	// CheckSandbox is declindex.CheckSandbox, unless a test replaces it.
	CheckSandbox func(context.Context, declindex.Lean) error
}

// ProposalModule is the module name a proposal is compiled as.
const ProposalModule = "DeclIndexProposal"

// Forms gives the registry's forms module, importing the module of each registered concept that the index has.
// A source-bearing concept that the index does not yet declare is pending; only the gate compiles it, in a
// sandbox, and then adds it to the forms module for that one decision.
func Forms(ctx context.Context, s *declindex.Store, r Registry) (declindex.FormsModule, error) {
	fm := declindex.FormsModule{Imports: append([]string(nil), r.Imports...), Opens: append([]string(nil), r.Opens...)}
	seen := map[string]bool{}
	for _, i := range fm.Imports {
		seen[i] = true
	}
	for _, c := range r.Concepts {
		found, err := s.Resolve(ctx, c.Name)
		if err != nil {
			return fm, err
		}
		declared := false
		for _, f := range found {
			if f.Name != c.Name {
				continue
			}
			declared = true
			if !seen[f.Module] {
				seen[f.Module] = true
				fm.Imports = append(fm.Imports, f.Module)
			}
		}
		if declared {
			fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
		}
	}
	return fm, nil
}

// Decide compares a proposal with the registry and the index. An added verdict carries a canonical file; the
// gate never writes to the registry.
func (g *Gate) Decide(ctx context.Context, p Proposal, options DecideOptions) (Verdict, error) {
	bytes := options.ProposalBytes
	if bytes == nil {
		var err error
		bytes, err = json.Marshal(p)
		if err != nil {
			return Verdict{}, err
		}
	}
	digest := sha256.Sum256(bytes)
	v := Verdict{Proposal: p.ID, Name: p.Concept.Name, VerdictID: "near-" + hex.EncodeToString(digest[:8])}
	if err := ValidateOwnerID(options.Proposer); err != nil {
		return v, err
	}
	if options.AcceptedBy != "" {
		if err := ValidateOwnerID(options.AcceptedBy); err != nil {
			return v, err
		}
	}
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Source) == "" {
		return v, fmt.Errorf("a proposal needs an id and a Lean source that declares it")
	}
	if err := p.Concept.check(); err != nil {
		return v, err
	}
	if len(p.Concept.Forms) > 0 {
		return v, fmt.Errorf("a proposal carries no forms: the librarian writes a concept's forms")
	}
	if err := checkProposalSource(p.Source); err != nil {
		return v, err
	}
	check := g.CheckSandbox
	if check == nil {
		check = declindex.CheckSandbox
	}
	if err := check(ctx, g.Compiled.Exporter.Lean); err != nil {
		return v, fmt.Errorf("the gate compiles a proposal only in the sandbox: %w", err)
	}
	if d, err := g.Index.Meta(ctx, "compiled"); err != nil {
		return v, err
	} else if d == "" {
		return v, fmt.Errorf("the index was read without its compiled part, so the gate has no compiled statements to compare")
	}
	base, err := g.Index.Meta(ctx, "commit")
	if err != nil {
		return v, err
	}
	reg, _, err := Load(g.Registry)
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
		if rc.Hash == "" && c.CompiledHash == "" && len(c.Forms) == 0 && c.Source == "" {
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
		if options.AcceptedBy == "" {
			v.Outcome, v.Reason = Near, "the proposal restates a registered concept's fact"
			if len(v.Matches) == 0 {
				v.Reason = "the gate cannot compare the proposal with " + strings.Join(unknown, ", ") +
					": registered, not declared in the index, with no form and no compiled hash"
			}
			if g.Librarian == nil {
				return v, errors.New("no librarian is configured")
			}
			if err := g.Librarian.Review(ctx, ReviewRequest{ID: v.VerdictID, Proposer: options.Proposer, Kind: "near", Verdict: v, Proposal: p}); err != nil {
				return v, err
			}
			return v, nil
		}
	}
	c := p.Concept
	c.Proposal = p.ID
	c.Proposer = options.Proposer
	c.CompiledHash = decl.Hash
	c.Source = p.Source
	c.AcceptedBy = options.AcceptedBy
	if options.AcceptedBy != "" {
		seen := map[string]bool{}
		for _, m := range v.Matches {
			seen[m.Concept] = true
		}
		for name := range seen {
			c.AcceptedNear = append(c.AcceptedNear, name)
		}
		sort.Strings(c.AcceptedNear)
	}
	content, err := Render(c)
	if err != nil {
		return v, err
	}
	v.Outcome, v.Reason = Added, "no registered concept or existing declaration states it"
	root := g.AdditionRoot
	if root == "" {
		root = g.Registry
	}
	v.Addition = &Addition{Path: filepath.Join(root, "concepts", Key(c)+".yaml"), Content: string(content), Base: base}
	return v, nil
}

// pendingForms compiles the sources of registry concepts that are not yet declared by the index. It runs only
// from the gate's confined export path; refresh and FormsModule never receive those source bytes.
func (g *Gate) pendingForms(ctx context.Context, reg Registry, ex *declindex.Exporter, run, exclude string) (declindex.FormsModule, error) {
	fm, err := Forms(ctx, g.Index, reg)
	if err != nil {
		return fm, err
	}
	seenImports := map[string]bool{}
	for _, i := range fm.Imports {
		seenImports[i] = true
	}
	for _, c := range reg.Concepts {
		if c.Name == exclude {
			continue
		}
		compiled, err := g.Index.Constant(ctx, c.Name)
		var nf *declindex.NotFoundError
		if err != nil && !errors.As(err, &nf) {
			return fm, err
		}
		if err == nil && compiled.Hash != "" {
			continue
		}
		if c.Source == "" {
			continue
		}
		if c.CompiledHash == "" {
			return fm, fmt.Errorf("pending concept %s has source but no compiled hash", c.Name)
		}
		if err := checkProposalSource(c.Source); err != nil {
			return fm, fmt.Errorf("pending concept %s: %w", c.Name, err)
		}
		check, err := declindex.NewScratch(run)
		if err != nil {
			return fm, err
		}
		if err := ex.CheckSource(ctx, []byte(c.Source), ex.SearchPath, check); err != nil {
			return fm, fmt.Errorf("check pending concept %s: %w", c.Name, err)
		}
		dir, err := declindex.NewScratch(run)
		if err != nil {
			return fm, err
		}
		module := "DeclIndexPending" + c.CompiledHash
		if err := declindex.CompileConfined(ctx, ex.Lean, ex.SearchPath, dir, module, []byte(c.Source)); err != nil {
			return fm, fmt.Errorf("compile pending concept %s: %w", c.Name, err)
		}
		ex.SearchPath = append(ex.SearchPath, dir)
		if !seenImports[module] {
			seenImports[module] = true
			fm.Imports = append(fm.Imports, module)
		}
		fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
	}
	return fm, nil
}

// export checks the proposal, compiles it against the build and reads it with the registry's forms, each step in
// the sandbox with a fresh scratch directory of its own below one run directory that the gate owns and removes.
func (g *Gate) export(ctx context.Context, reg Registry, p Proposal) ([]declindex.Constant, error) {
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return nil, err
	}
	run, err := declindex.NewScratch(g.WorkDir)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(run)
	ex := *g.Compiled.Exporter
	ex.ScratchDir, ex.Confined = run, true
	co := *g.Compiled
	co.Exporter, co.Confined = &ex, true
	if co.Forms, err = g.pendingForms(ctx, reg, &ex, run, p.Concept.Name); err != nil {
		return nil, err
	}
	check, err := declindex.NewScratch(run)
	if err != nil {
		return nil, err
	}
	if err := ex.CheckSource(ctx, []byte(p.Source), ex.SearchPath, check); err != nil {
		return nil, err
	}
	compiled, err := declindex.NewScratch(run)
	if err != nil {
		return nil, err
	}
	if err := declindex.CompileConfined(ctx, ex.Lean, ex.SearchPath, compiled, ProposalModule, []byte(p.Source)); err != nil {
		return nil, fmt.Errorf("the proposal does not compile: %w", err)
	}
	roots := declindex.Roots{ProposalModule: true}
	for r := range g.Tree.Roots {
		roots[r] = true
	}
	return co.Export(ctx, roots, []string{ProposalModule}, []string{compiled})
}
