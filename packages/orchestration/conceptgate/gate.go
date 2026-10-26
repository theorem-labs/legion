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
	// Refused: an exact duplicate of a registered concept or an existing declaration; Existing names it. With
	// --accepted-by (DecideOptions.AcceptedBy), an exact duplicate of declarations that no concept registers is added.
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
	// Duplicates are the names of the indexed declarations whose compiled statement equals the proposal's, sorted:
	// every copy that a refusal names, or that an addition admitted by --accepted-by records.
	Duplicates []string `json:"duplicates,omitempty"`
	// Defects are the registry's defects at this decision: every defect that Load reports (a concept file that it
	// refused and left out, a duplicate name or compiled hash, a wrongly keyed file, an extends of a concept that is
	// not registered, an accepted duplicate that is a registered concept), and each pending concept whose stored
	// source no longer compiles, which the decision leaves out of the forms.
	Defects []Defect `json:"defects,omitempty"`
	// WithoutForms counts the modules that the index's compiled part read without the registry's forms
	// (declindex.FormsClash), an index defect: no fact of a written form is known in them, so `states` and the
	// index's facts miss any statement there.
	WithoutForms int `json:"modules_without_forms,omitempty"`
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
	// WorkDir holds the compiled proposals and librarian requests.
	WorkDir string
	// release closes what OpenGate opened for the gate (see Close).
	release func()
}

// ProposalModule is the module name a proposal is compiled as.
const ProposalModule = "DeclIndexProposal"

// Forms gives the registry's forms module, importing the module of each registered concept that the index declares
// in the concept's home file. A concept with forms and no stored source states its fact through those forms, so it
// is kept whether or not the index declares it. A source-bearing concept that the index does not yet declare is
// pending; only the gate compiles it, after Lean's parser allow-list, and then adds it to the forms module for that
// one decision.
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
	// The gate compares compiled statements, so it needs the compiled part of the index's own commit.
	base, err := g.Index.CompiledCommit(ctx, true)
	if err != nil {
		return v, err
	}
	clashes, err := g.Index.WithoutForms(ctx)
	if err != nil {
		return v, err
	}
	v.WithoutForms = declindex.ClashModules(clashes)
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
	home, err := homeModule(ctx, g.Index, p.Concept)
	if err != nil {
		return v, err
	}
	consts, fresh, pending, err := g.export(ctx, reg, p, home)
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
		// A pending concept is compared by what its stored source compiles to now as well as by the hash its file
		// recorded, which a change of the hash's formula leaves stale.
		if (rc.Hash != "" && rc.Hash == decl.Hash) || (c.CompiledHash != "" && c.CompiledHash == decl.Hash) ||
			(fresh[c.Name].Hash != "" && fresh[c.Name].Hash == decl.Hash) {
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
		// The copies are the indexed declarations of this statement. --accepted-by gives the librarian's ruling that
		// the proposal stands beside them (its own concept with an equal body, or the canonical name of copies that
		// no concept registers), never beside a registered concept.
		v.Duplicates = duplicateNames(same)
		for _, name := range v.Duplicates {
			if c, ok := reg.Find(name); ok {
				return decided(Refused, c.Name, c.Home, "the compiled statement equals the declaration of the registered concept "+c.Name+", and --accepted-by admits no copy of a registered concept")
			}
		}
		if options.AcceptedBy == "" {
			return decided(Refused, same[0].Name, same[0].At(), "the compiled statement equals an existing declaration's")
		}
		// A refused concept file that holds a copy's name may be that copy's concept, which the gate cannot read.
		for _, refused := range reg.Refused {
			name := refused.Name
			if !slices.Contains(v.Duplicates, name) {
				name = refused.Key()
			}
			if slices.Contains(v.Duplicates, name) {
				at, err := g.additionPath(refused.Key())
				if err != nil {
					return v, err
				}
				return v, fmt.Errorf("the concept file %s holds the name of %s, an exact duplicate of the proposal, but the loader refused it (%s): repair or remove that file, then propose again", at, name, refused.Defect.Message)
			}
		}
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
	c.Declarations = landedNames(own, home)
	c.Source = p.Source
	// accepted_by, accepted_near and accepted_duplicates record the librarian's decision: never the proposal's own.
	c.AcceptedBy, c.AcceptedNear, c.AcceptedDuplicates = options.AcceptedBy, nil, nil
	if options.AcceptedBy != "" {
		seen := map[string]bool{}
		for _, m := range v.Matches {
			seen[m.Concept] = true
		}
		c.AcceptedNear = slices.Sorted(maps.Keys(seen))
		c.AcceptedDuplicates = v.Duplicates
	}
	content, err := Render(c)
	if err != nil {
		return v, err
	}
	v.Outcome, v.Reason = Added, "no registered concept or existing declaration states it"
	if len(c.AcceptedDuplicates) > 0 {
		v.Reason = "an exact duplicate of unregistered declarations " + strings.Join(c.AcceptedDuplicates, ", ") + ", admitted by " + options.AcceptedBy
	}
	addition, err := g.additionPath(Key(c))
	if err != nil {
		return v, err
	}
	v.Addition = &Addition{Path: addition, Content: string(content), Base: base}
	return v, nil
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

// duplicateNames are the names of the declarations same, sorted, each once: a name that two modules declare is one
// copy's name.
func duplicateNames(same []declindex.Found) []string {
	names := make([]string, 0, len(same))
	for _, f := range same {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
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

// admit runs untrusted source through the gate's two steps for it, each in a fresh directory below run: Lean's
// parser allow-list (CheckSource), and only once that passed, the compile of source as module against ex's search
// path, with the rights of the user who runs the gate. It gives the directory that holds the compiled module. When
// the source itself is refused or does not compile, the error satisfies sourceFailed.
func admit(ctx context.Context, ex *declindex.Exporter, run, module, source string) (string, error) {
	check, err := os.MkdirTemp(run, "check-")
	if err != nil {
		return "", err
	}
	if err := ex.CheckSource(ctx, []byte(source), ex.SearchPath, check); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(run, "module-")
	if err != nil {
		return "", err
	}
	if err := declindex.CompileSource(ctx, ex.Lean, ex.SearchPath, dir, module, []byte(source)); err != nil {
		return "", err
	}
	return dir, nil
}

// pendingSource is a pending concept's compiled source: the module it is compiled as, and the module of its home file,
// which its constants are hashed as of, so that they hash as the declaration that lands there.
type pendingSource struct {
	Concept, Module, Home string
}

// pendingForms gives the forms module of a decision on the proposal of the concept exclude, or of the recheck of a
// landed declaration when exclude is empty, with the compiled sources of the registry concepts that the index does
// not yet declare, and those sources' modules. It runs only from the gate's export path (read); refresh and
// FormsModule never receive those source bytes. A pending concept whose source the text filter or Lean refuses is a
// defect of that concept, left out of the forms. The proposal supplies exclude, also when the index declares it
// outside its home file. Lean refuses two definitions of one name, so the forms then import no module whose imports
// declare exclude (hiddenModules), and a concept whose compiled constant is in such a module is pending.
func (g *Gate) pendingForms(ctx context.Context, reg Registry, ex *declindex.Exporter, run, exclude string) (declindex.FormsModule, []pendingSource, []Defect, error) {
	var hidden map[string]bool
	if exclude != "" {
		var err error
		if hidden, err = hiddenModules(ctx, g.Index, exclude); err != nil {
			return declindex.FormsModule{}, nil, nil, err
		}
	}
	fm, err := forms(ctx, g.Index, reg, hidden)
	if err != nil {
		return fm, nil, nil, err
	}
	var sources []pendingSource
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
			return fm, sources, defects, err
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
				return fm, sources, defects, fmt.Errorf("pending concept %s: %w", c.Name, err)
			}
			defect(c, "%v", err)
			continue
		}
		ex.SearchPath = append(ex.SearchPath, dir)
		if !slices.Contains(fm.Imports, module) {
			fm.Imports = append(fm.Imports, module)
			home, err := homeModule(ctx, g.Index, c)
			if err != nil {
				return fm, sources, defects, err
			}
			sources = append(sources, pendingSource{Concept: c.Name, Module: module, Home: home})
		}
		fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
	}
	return fm, sources, defects, nil
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

// landedDeclaration is the declaration of the concept c in the module of its home file, and false when the index
// declares no c there.
func landedDeclaration(ctx context.Context, s *declindex.Store, c Concept) (declindex.Found, bool, error) {
	home, err := s.ModuleAt(ctx, c.Home)
	if err != nil || home == "" {
		return declindex.Found{}, false, err
	}
	found, err := s.Resolve(ctx, c.Name)
	if err != nil {
		return declindex.Found{}, false, err
	}
	for _, f := range found {
		if f.Name == c.Name && f.Module == home {
			return f, true, nil
		}
	}
	return declindex.Found{}, false, nil
}

// landedConstants reads the module of d, the landed declaration of an added concept, as export reads a proposal:
// with the registry's forms and the compiled sources of its pending concepts. It gives the constants of d and of the
// other declarations of its proposal (declared, the concept's Declarations) in that module, each declaration's own
// and those that Lean generated from it, and the defects of the pending concepts it left out of the forms. Another
// declaration of the module, though it lies in the namespace of one of them, is not the proposal's.
func (g *Gate) landedConstants(ctx context.Context, reg Registry, d declindex.Found, declared []string) ([]declindex.Constant, []Defect, error) {
	parts, err := g.Index.DeclarationParts(ctx, d.Module, append([]string{d.Name}, declared...))
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(parts, d.Name) {
		return nil, nil, fmt.Errorf("the index declares %s in %s but holds no compiled constant of it: refresh the index with lean.export on", d.Name, d.At())
	}
	consts, _, defects, err := g.read(ctx, reg, nil, d.Module, "")
	if err != nil {
		return nil, defects, err
	}
	return slices.DeleteFunc(consts, func(c declindex.Constant) bool { return !slices.Contains(parts, c.Name) }), defects, nil
}

// export checks the proposal, compiles it against the build and reads it with the registry's forms (read), hashed
// as of home, the module of the proposal's home file (homeModule). It also gives the compiled constant of each
// pending concept, by name.
func (g *Gate) export(ctx context.Context, reg Registry, p Proposal, home string) ([]declindex.Constant, map[string]declindex.Constant, []Defect, error) {
	return g.read(ctx, reg, &p, ProposalModule, home)
}

// homeModule is the module of the concept c's home file, where its declaration lands: the module that the index
// read there, else the one that a Lean file at that path builds. Concept.check has made the home a clean,
// tree-relative .lean path (homeFile).
func homeModule(ctx context.Context, s *declindex.Store, c Concept) (string, error) {
	if m, err := s.ModuleAt(ctx, c.Home); err != nil || m != "" {
		return m, err
	}
	return strings.ReplaceAll(strings.TrimSuffix(c.Home, ".lean"), "/", "."), nil
}

// landedNames are the names of a proposal's constants, own, as they are once it lands in the module home: a private
// name holds its module, the proposal's here and home's there.
func landedNames(own map[string]bool, home string) []string {
	private := "_private." + ProposalModule + "."
	out := make([]string, 0, len(own))
	for n := range own {
		if rest, ok := strings.CutPrefix(n, private); ok {
			n = "_private." + home + "." + rest
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// read reads module with the registry's forms, each step in a fresh directory of its own below one run directory
// that the gate owns and removes. With a proposal p, it first checks p's source and compiles it as module against
// the build, and the forms leave p's concept out (pendingForms); without one, module is the landed build's. home,
// when set, is the module that module's constants are hashed as of. It reads the pending concepts' compiled sources
// in the same run, each hashed as of its own home file's module, and gives the constant of each pending concept by
// name: what its stored source compiles to now, which the hash its file recorded may no longer be. It also gives the
// defects of the pending concepts it left out of the forms. When the proposal's own source is refused or does not
// compile, the error satisfies sourceFailed.
func (g *Gate) read(ctx context.Context, reg Registry, p *Proposal, module, home string) ([]declindex.Constant, map[string]declindex.Constant, []Defect, error) {
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	run, err := os.MkdirTemp(g.WorkDir, "read-")
	if err != nil {
		return nil, nil, nil, err
	}
	defer os.RemoveAll(run)
	exclude := ""
	if p != nil {
		exclude = p.Concept.Name
	}
	// The forms of this one decision, which import its pending concepts' compiled sources, are compiled below run,
	// never in the forms directory that refresh shares.
	formsDir, err := os.MkdirTemp(run, "forms-")
	if err != nil {
		return nil, nil, nil, err
	}
	ex := *g.Compiled.Exporter
	ex.ScratchDir = run
	co := *g.Compiled
	co.Exporter, co.Dir = &ex, formsDir
	forms, sources, defects, err := g.pendingForms(ctx, reg, &ex, run, exclude)
	if err != nil {
		return nil, nil, defects, err
	}
	co.Forms = forms
	roots := declindex.Roots{}
	for r := range g.Tree.Roots {
		roots[r] = true
	}
	var extra []string
	if p != nil {
		compiled, err := admit(ctx, &ex, run, module, p.Source)
		if err != nil {
			return nil, nil, defects, err
		}
		roots[module] = true
		extra = []string{compiled}
	}
	modules := []string{module}
	var homes []declindex.ModuleHome
	for _, s := range sources {
		modules = append(modules, s.Module)
		homes = append(homes, declindex.ModuleHome{Module: s.Module, Home: s.Home})
	}
	all, err := co.Export(ctx, roots, modules, extra, home, homes)
	if err != nil {
		return nil, nil, defects, err
	}
	pending := map[string]declindex.Constant{}
	for _, s := range sources {
		for _, c := range all {
			if c.Module == s.Module && c.Name == s.Concept {
				pending[s.Concept] = c
			}
		}
	}
	consts := slices.DeleteFunc(all, func(c declindex.Constant) bool { return c.Module != module })
	return consts, pending, defects, nil
}
