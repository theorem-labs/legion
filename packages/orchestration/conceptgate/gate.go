package conceptgate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/declindex/confine"
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
	// Defects are the registry's defects at this decision: every defect that Load reports (a concept file that it
	// refused and left out, a duplicate name or compiled hash, a wrongly keyed file, an extends of a concept that is
	// not registered), and each pending concept whose stored source no longer compiles, which the decision leaves
	// out of the forms.
	Defects []Defect `json:"defects,omitempty"`
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
	Index    *declindex.Store
	Tree     declindex.Tree
	Registry string
	// AdditionRoot is the registry directory relative to the repository root, slash-separated, as additions name
	// it. OpenGate sets it from the tree's path in its repository, since it reads Registry from a snapshot.
	AdditionRoot string
	// Compiled reads the proposal; its Forms are replaced by the registry's at each decision.
	Compiled  *declindex.CompiledOptions
	Librarian Librarian
	// WorkDir holds the compiled proposals and librarian request files.
	WorkDir string
	// CheckSandbox is declindex.CheckSandbox, unless a test replaces it: see checkSandbox.
	CheckSandbox func(context.Context, declindex.Lean) error
	// release closes what OpenGate opened for the gate (see Close).
	release func()
}

// ProposalModule is the module name a proposal is compiled as.
const ProposalModule = "DeclIndexProposal"

// Forms gives the registry's forms module, importing the module of each registered concept that the index declares
// in the concept's home file. A concept with forms and no stored source states its fact through those forms, so it
// is kept whether or not the index declares it. A source-bearing concept that the index does not yet declare is
// pending; only the gate compiles it, in a sandbox, and then adds it to the forms module for that one decision.
func Forms(ctx context.Context, s *declindex.Store, r Registry) (declindex.FormsModule, error) {
	return forms(ctx, s, r, nil)
}

// forms is Forms without the modules of hidden: it imports none of them, and a concept that only they declare is
// not declared.
func forms(ctx context.Context, s *declindex.Store, r Registry, hidden map[string]bool) (declindex.FormsModule, error) {
	fm := declindex.FormsModule{Opens: append([]string(nil), r.Opens...)}
	seen := map[string]bool{}
	for _, i := range r.Imports {
		if !hidden[i] {
			fm.Imports = append(fm.Imports, i)
		}
		seen[i] = true
	}
	for _, c := range r.Concepts {
		home, err := s.ModuleAt(ctx, c.Home)
		if err != nil {
			return fm, err
		}
		found, err := s.Resolve(ctx, c.Name)
		if err != nil {
			return fm, err
		}
		declared := false
		for _, f := range found {
			if f.Name != c.Name || f.Module != home || hidden[f.Module] {
				continue
			}
			declared = true
			if !seen[f.Module] {
				seen[f.Module] = true
				fm.Imports = append(fm.Imports, f.Module)
			}
		}
		if declared || (c.Source == "" && len(c.Forms) > 0) {
			fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
		}
	}
	return fm, nil
}

// conceptConstant is the compiled constant of the registered concept c: its name in the module of its home file.
// Another module can declare the same name; it is not c's.
func conceptConstant(ctx context.Context, s *declindex.Store, c Concept) (declindex.Compiled, error) {
	module, err := s.ModuleAt(ctx, c.Home)
	if err != nil {
		return declindex.Compiled{}, err
	}
	if module == "" {
		return declindex.Compiled{}, &declindex.NotFoundError{Name: c.Name}
	}
	return s.Constant(ctx, c.Name, module)
}

// Decide compares a proposal with the registry and the index. An added verdict carries a canonical file; the
// gate never writes to the registry. The verdict's defects are the registry's (Verdict.Defects).
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
	if err := g.checkSandbox(ctx); err != nil {
		return v, fmt.Errorf("the gate compiles a proposal only in the sandbox: %w", err)
	}
	// The gate compares compiled statements, so it needs the compiled part of the index's own commit.
	base, err := g.Index.CompiledCommit(ctx, true)
	if err != nil {
		return v, err
	}
	reg, defects, err := Load(g.Registry)
	if err != nil {
		return v, err
	}
	v.Defects = defects
	if p.Concept.Extends != "" {
		if _, ok := reg.Find(p.Concept.Extends); !ok {
			return v, fmt.Errorf("concept %s extends %s, which is not registered", p.Concept.Name, p.Concept.Extends)
		}
	}
	decided := func(outcome Outcome, existing, at, reason string) (Verdict, error) {
		v.Outcome, v.Existing, v.ExistingAt, v.Reason = outcome, existing, at, reason
		return v, nil
	}
	if c, ok := reg.Find(p.Concept.Name); ok {
		return decided(NameTaken, c.Name, c.Home, "a concept of this name is registered")
	}
	// A refused concept file still holds its name: the name its YAML gives, or its key when it is a name.
	for _, refused := range reg.Refused {
		if refused.Name == p.Concept.Name || refused.Key() == p.Concept.Name {
			at, err := g.additionPath(refused.Key())
			if err != nil {
				return v, err
			}
			return decided(NameTaken, p.Concept.Name, at, "the concept file "+at+" holds this name, but the loader refused it: "+refused.Defect.Message)
		}
	}
	found, err := g.Index.Resolve(ctx, p.Concept.Name)
	if err != nil {
		return v, err
	}
	for _, f := range found {
		if f.Name == p.Concept.Name {
			return decided(NameTaken, f.Name, f.At(), "a declaration of this name exists")
		}
	}
	consts, pending, err := g.export(ctx, reg, p)
	v.Defects = append(v.Defects, pending...)
	if err != nil {
		return v, err
	}
	own, decl := ownConstants(consts, p.Concept.Name)
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
		rc, err := conceptConstant(ctx, g.Index, c)
		var nf *declindex.NotFoundError
		switch {
		case errors.As(err, &nf):
			rc = declindex.Compiled{}
		case err != nil:
			return v, err
		}
		if (rc.Hash != "" && rc.Hash == decl.Hash) || (c.CompiledHash != "" && c.CompiledHash == decl.Hash) {
			return decided(Refused, c.Name, c.Home, "the compiled statement equals the registered concept's")
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
		return decided(Refused, same[0].Name, same[0].At(), "the compiled statement equals an existing declaration's")
	}
	// A refused concept file that holds this statement, as its key or as the compiled hash it records, is a concept
	// that the gate cannot read: an addition would replace that file or restate it.
	for _, refused := range reg.Refused {
		if refused.Key() == decl.Hash || refused.CompiledHash == decl.Hash {
			at, err := g.additionPath(refused.Key())
			if err != nil {
				return v, err
			}
			return v, fmt.Errorf("the concept file %s holds the proposal's compiled statement %s, but the loader refused it (%s): repair or remove that file, then propose again", at, decl.Hash, refused.Defect.Message)
		}
	}
	for _, c := range consts {
		for _, f := range c.Facts {
			if statesOwnFact(f.How, own) {
				v.Matches = append(v.Matches, Match{Concept: f.Concept, How: f.How, Declaration: c.Name})
			}
		}
	}
	if len(v.Matches) > 0 || len(unknown) > 0 {
		sortMatches(v.Matches)
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
	// accepted_by and accepted_near record the librarian's decision: never the proposal's own.
	c.AcceptedBy, c.AcceptedNear = options.AcceptedBy, nil
	if options.AcceptedBy != "" {
		seen := map[string]bool{}
		for _, m := range v.Matches {
			seen[m.Concept] = true
		}
		c.AcceptedNear = slices.Sorted(maps.Keys(seen))
	}
	content, err := Render(c)
	if err != nil {
		return v, err
	}
	v.Outcome, v.Reason = Added, "no registered concept or existing declaration states it"
	addition, err := g.additionPath(Key(c))
	if err != nil {
		return v, err
	}
	v.Addition = &Addition{Path: addition, Content: string(content), Base: base}
	return v, nil
}

// checkSandbox is the authoritative sandbox check, which Decide and Recheck run before any untrusted source
// compiles: the gate's own toolchain runs `lean --version` in the sandbox. Decide and Recheck run it themselves,
// because a Gate can be built without OpenGate. OpenGate's earlier check (Options.SandboxAvailable) starts no
// process and only spares a machine without the sandbox the toolchain lookup and the exporter build.
func (g *Gate) checkSandbox(ctx context.Context) error {
	check := g.CheckSandbox
	if check == nil {
		check = declindex.CheckSandbox
	}
	return check(ctx, g.Compiled.Exporter.Lean)
}

// ownConstants are the names of the constants that a proposal's source declares, and the one named name.
func ownConstants(consts []declindex.Constant, name string) (map[string]bool, *declindex.Constant) {
	own := map[string]bool{}
	var decl *declindex.Constant
	for i, c := range consts {
		own[c.Name] = true
		if c.Name == name {
			decl = &consts[i]
		}
	}
	return own, decl
}

// statesOwnFact says whether a fact that a proposal's constant states, as how, is the proposal's own statement:
// directly, or through a definition the proposal itself declares.
func statesOwnFact(how string, own map[string]bool) bool {
	return how == "direct" || strings.HasPrefix(how, "via:") && own[strings.TrimPrefix(how, "via:")]
}

// sortMatches orders matches by concept, then declaration, then how.
func sortMatches(ms []Match) {
	slices.SortFunc(ms, func(a, b Match) int {
		return cmp.Or(strings.Compare(a.Concept, b.Concept), strings.Compare(a.Declaration, b.Declaration), strings.Compare(a.How, b.How))
	})
}

// additionPath is the repository-relative, slash-separated path of the addition file with key: the path that its
// owner writes in its own worktree and opens with `ledger open --write`.
func (g *Gate) additionPath(key string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(g.AdditionRoot)) {
		return "", fmt.Errorf("the registry's path in the repository, %q, is not a path inside it", g.AdditionRoot)
	}
	return path.Join(g.AdditionRoot, conceptPath(key)), nil
}

// sourceFailed says whether err is the failure of an untrusted source itself: refused by Lean's parser allow-list,
// or rejected by Lean. Any other error is the gate's own inability.
func sourceFailed(err error) bool {
	return errors.Is(err, declindex.ErrRefusedSource) || errors.Is(err, declindex.ErrSourceFailed)
}

// admit runs untrusted source through the gate's two steps for it, each in the sandbox with a fresh scratch
// directory below run: Lean's parser allow-list (CheckSource), and only once that passed, the compile of source as
// module against ex's search path. It gives the directory that holds the compiled module. When the source itself is
// refused or does not compile, the error satisfies sourceFailed.
func admit(ctx context.Context, ex *declindex.Exporter, run, module, source string) (string, error) {
	check, err := confine.NewScratch(run)
	if err != nil {
		return "", err
	}
	if err := ex.CheckSource(ctx, []byte(source), ex.SearchPath, check); err != nil {
		return "", err
	}
	dir, err := confine.NewScratch(run)
	if err != nil {
		return "", err
	}
	if err := declindex.CompileConfined(ctx, ex.Lean, ex.SearchPath, dir, module, []byte(source)); err != nil {
		return "", err
	}
	return dir, nil
}

// pendingForms gives the forms module of a decision on the proposal of the concept exclude, with the compiled
// sources of the registry concepts that the index does not yet declare. It runs only from the gate's confined
// export path; refresh and FormsModule never receive those source bytes. A pending concept whose source the text
// filter or Lean refuses is a defect of that concept, left out of the forms. The proposal supplies exclude, also
// when the index declares it outside its home file. Lean refuses two definitions of one name, so the forms then
// import no module whose imports declare exclude (hiddenModules), and a concept whose compiled constant is in such a
// module is pending.
func (g *Gate) pendingForms(ctx context.Context, reg Registry, ex *declindex.Exporter, run, exclude string) (declindex.FormsModule, []Defect, error) {
	hidden, err := hiddenModules(ctx, g.Index, exclude)
	if err != nil {
		return declindex.FormsModule{}, nil, err
	}
	fm, err := forms(ctx, g.Index, reg, hidden)
	if err != nil {
		return fm, nil, err
	}
	var defects []Defect
	defect := func(c Concept, format string, args ...any) {
		defects = append(defects, Defect{Path: conceptFile(c), Message: fmt.Sprintf("pending concept %s: ", c.Name) + fmt.Sprintf(format, args...)})
	}
	for _, c := range reg.Concepts {
		if c.Name == exclude {
			continue
		}
		compiled, err := conceptConstant(ctx, g.Index, c)
		var nf *declindex.NotFoundError
		if err != nil && !errors.As(err, &nf) {
			return fm, defects, err
		}
		if err == nil && compiled.Hash != "" && !hidden[compiled.Module] {
			continue
		}
		if c.Source == "" {
			continue
		}
		if c.CompiledHash == "" {
			defect(c, "it has source but no compiled hash")
			continue
		}
		if err := checkProposalSource(c.Source); err != nil {
			defect(c, "%v", err)
			continue
		}
		module := "DeclIndexPending" + c.CompiledHash
		dir, err := admit(ctx, ex, run, module, c.Source)
		if err != nil {
			if !sourceFailed(err) {
				return fm, defects, fmt.Errorf("pending concept %s: %w", c.Name, err)
			}
			defect(c, "%v", err)
			continue
		}
		ex.SearchPath = append(ex.SearchPath, dir)
		if !slices.Contains(fm.Imports, module) {
			fm.Imports = append(fm.Imports, module)
		}
		fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
	}
	return fm, defects, nil
}

// hiddenModules are the modules in which the index declares name, and every module that imports one of them.
func hiddenModules(ctx context.Context, s *declindex.Store, name string) (map[string]bool, error) {
	found, err := s.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	var modules []string
	for _, f := range found {
		if f.Name == name {
			modules = append(modules, f.Module)
		}
	}
	return s.Importers(ctx, modules)
}

// landedConstants are the compiled constants of the concept c's declaration in the module of its home file, and nil
// when the index declares no c there.
func landedConstants(ctx context.Context, s *declindex.Store, c Concept) ([]declindex.Constant, error) {
	home, err := s.ModuleAt(ctx, c.Home)
	if err != nil || home == "" {
		return nil, err
	}
	found, err := s.Resolve(ctx, c.Name)
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		if f.Name != c.Name || f.Module != home {
			continue
		}
		consts, err := s.DeclarationConstants(ctx, f)
		if err == nil && len(consts) == 0 {
			err = fmt.Errorf("the index declares %s in %s but holds no compiled constant of it: refresh the index with lean.export on", c.Name, f.At())
		}
		return consts, err
	}
	return nil, nil
}

// export checks the proposal, compiles it against the build and reads it with the registry's forms, each step in
// the sandbox with a fresh scratch directory of its own below one run directory that the gate owns and removes.
// It also gives the defects of the pending concepts it left out of the forms. When the proposal's own source is
// refused or does not compile, the error satisfies sourceFailed.
func (g *Gate) export(ctx context.Context, reg Registry, p Proposal) ([]declindex.Constant, []Defect, error) {
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return nil, nil, err
	}
	run, err := confine.NewScratch(g.WorkDir)
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(run)
	// The confined exporter also has the forms module compiled in the sandbox (declindex.Exporter.Confined).
	ex := *g.Compiled.Exporter
	ex.ScratchDir, ex.Confined = run, true
	co := *g.Compiled
	co.Exporter = &ex
	forms, defects, err := g.pendingForms(ctx, reg, &ex, run, p.Concept.Name)
	if err != nil {
		return nil, defects, err
	}
	co.Forms = forms
	compiled, err := admit(ctx, &ex, run, ProposalModule, p.Source)
	if err != nil {
		return nil, defects, err
	}
	roots := declindex.Roots{ProposalModule: true}
	for r := range g.Tree.Roots {
		roots[r] = true
	}
	consts, err := co.Export(ctx, roots, []string{ProposalModule}, []string{compiled})
	return consts, defects, err
}
