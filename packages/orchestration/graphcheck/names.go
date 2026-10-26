package graphcheck

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// Names runs the name rules of README.md, "The rules": C1 (a changed module that does not compile), N1 (a built
// unchanged module that uses a name the compiled candidate no longer declares) and N2 (a name the compiled
// candidate adds that another built module declares too).
//
// Both sides are compiled output: the base's from the declaration index, the candidate's from the modules that
// Open compiled against the base's build.
func Names(ctx context.Context, c *Candidate, idx *declindex.Store, cfg Config) ([]landing.Finding, error) {
	n, err := readNames(ctx, c, idx, cfg)
	if err != nil {
		return nil, err
	}
	out, err := n.removedUses(ctx)
	if err != nil {
		return nil, err
	}
	duplicates, err := n.duplicates(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, duplicates...)
	// C1 runs last: N1's confirming compiles are part of what Lean has said by now, so a module another member
	// broke is reported with the names Lean's own errors carry.
	refused, err := compileFindings(c, cfg, n.reportedModules)
	if err != nil {
		return nil, err
	}
	out = append(out, refused...)
	sortFindings(out)
	return out, nil
}

// nameState is what the name rules read once: the index's module paths, the base declarations of the changed
// modules, the declarations of the compiled candidate modules, and the names the candidate removed.
type nameState struct {
	c   *Candidate
	idx *declindex.Store
	cfg Config
	// paths maps each module of the index to its file, and modules each file to the index's module name.
	paths, modules map[string]string
	// baseDecls are the index's declarations of the modified and deleted files, by file.
	baseDecls map[string][]declindex.Decl
	// public maps each name a compiled candidate module declares publicly to where it declares it, and private
	// holds the names such a module declares privately.
	public  map[string][]located
	private map[string]bool
	// unchanged holds the index declarations of unchanged modules for the names asked about.
	unchanged map[string][]declindex.Decl
	// structures maps each base module of a changed file to the structures and classes it declared, and
	// candidateConsts each compiled candidate module to its compiled constants: a field is a constant S.f, which
	// no module's declarations hold.
	structures      map[string][]string
	candidateConsts map[string]map[string]bool
	removed         []*removal
	// reverse maps each module of the candidate to the modules that import it directly, read once for N2, and
	// closures each user module to its candidate import closure, for N1.
	reverse  map[string][]string
	closures map[string]map[string]bool
	// reportedModules are the modules N1 has reported on, which C1 then leaves to it.
	reportedModules map[string]bool
}

// located is one declaration of a compiled candidate module.
type located struct {
	Module, Path string
	Line         int
}

// removal is a name that a base declaration of a changed file gave and the compiled candidate no longer does.
type removal struct {
	// Name is the full name; Target is the index's name for it, which its uses name.
	Name, Target string
	// Path and Module are the base file and module that declared it.
	Path, Module string
	// Publicly is set when the candidate still declares the name, privately.
	Publicly bool
	// Structure and Field are set for a removed field Structure.Field: a declaration Structure.Field of a base
	// structure or class that the candidate still declares.
	Structure, Field string
	// Doubt says why a use may still resolve: the finding is a flag, not a refusal.
	Doubt string
}

func (r *removal) kind() blame.Kind {
	if r.Field != "" {
		return blame.Field
	}
	return blame.Unknown
}

func (r *removal) verdict() string {
	if r.Doubt != "" {
		return landing.VerdictFlag
	}
	return landing.VerdictRefuse
}

// what says what the candidate did to the name.
func (r *removal) what() string {
	switch {
	case r.Field != "":
		return fmt.Sprintf("structure %s in %s no longer has field %s", r.Structure, r.Path, r.Field)
	case r.Publicly:
		return fmt.Sprintf("%s no longer declares %s publicly", r.Path, r.Name)
	}
	return fmt.Sprintf("%s no longer declares %s", r.Path, r.Name)
}

func (r *removal) doubt() string {
	if r.Doubt == "" {
		return ""
	}
	return "; " + r.Doubt
}

// structureKinds are the kinds whose compiled constants below their own name are their constructor and fields.
var structureKinds = map[string]bool{"structure": true, "class": true}

func readNames(ctx context.Context, c *Candidate, idx *declindex.Store, cfg Config) (*nameState, error) {
	paths, err := idx.ModulePaths(ctx)
	if err != nil {
		return nil, err
	}
	n := &nameState{c: c, idx: idx, cfg: cfg, paths: map[string]string{}, modules: map[string]string{},
		baseDecls: map[string][]declindex.Decl{}, public: map[string][]located{}, closures: map[string]map[string]bool{},
		reportedModules: map[string]bool{},
		private:         map[string]bool{}, unchanged: map[string][]declindex.Decl{}}
	for module, file := range paths {
		n.paths[normalName(module)] = file
		n.modules[file] = module
	}
	var changedModules []string
	for _, ch := range c.Changes {
		if ch.Status != Added {
			changedModules = append(changedModules, n.indexModule(ch))
		}
		m := c.Module(ch.Module)
		if m == nil || m.State != Compiled || !c.Built(ch.Module) {
			continue
		}
		for _, d := range m.Data.Decls {
			name, isPrivate := unmangle(d.Name, m.Module)
			if isPrivate {
				n.private[name] = true
				continue
			}
			n.public[name] = append(n.public[name], located{Module: m.Module, Path: ch.Path, Line: d.Line})
		}
	}
	decls, err := idx.DeclsInModules(ctx, changedModules)
	if err != nil {
		return nil, err
	}
	var asked []string
	for _, d := range decls {
		file := n.fileOf(d.Module)
		n.baseDecls[file] = append(n.baseDecls[file], d)
		name, isPrivate := unmangle(d.Name, d.Module)
		if isPrivate {
			continue
		}
		asked = append(asked, name)
	}
	if err := n.readStructures(ctx); err != nil {
		return nil, err
	}
	if err := n.askUnchanged(ctx, asked); err != nil {
		return nil, err
	}
	return n, n.findRemovals(ctx)
}

// indexModule is the index's name of a changed file's module.
func (n *nameState) indexModule(ch Change) string {
	if module, ok := n.modules[ch.Path]; ok {
		return module
	}
	return ch.Module
}

// fileOf is a module's file: the index's path, else the path its name gives.
func (n *nameState) fileOf(module string) string {
	module = normalName(module)
	if file, ok := n.paths[module]; ok {
		return file
	}
	return strings.ReplaceAll(module, ".", "/") + ".lean"
}

// askUnchanged records which of the names a non-private declaration of an unchanged module declares.
func (n *nameState) askUnchanged(ctx context.Context, names []string) error {
	var missing []string
	for _, name := range names {
		if _, ok := n.unchanged[name]; !ok {
			missing = append(missing, name)
			n.unchanged[name] = nil
		}
	}
	if len(missing) == 0 {
		return nil
	}
	decls, err := n.idx.DeclsNamed(ctx, missing)
	if err != nil {
		return err
	}
	for _, d := range decls {
		if _, isPrivate := unmangle(d.Name, d.Module); isPrivate || n.c.ChangeOfModule(d.Module) != nil {
			continue
		}
		name := normalName(d.Name)
		n.unchanged[name] = append(n.unchanged[name], d)
	}
	return nil
}

// findRemovals lists the names a changed module's compiled output no longer declares. Only a deleted file and a
// module that compiled say anything: a module whose compile failed is C1's, and a changed module the build does
// not build has no built user to break. A name another module declares now is still listed: whether a user reaches
// that module is the user's own import closure's question (removedUses).
func (n *nameState) findRemovals(ctx context.Context) error {
	files := make([]string, 0, len(n.baseDecls))
	for file := range n.baseDecls {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		ch := n.c.Change(file)
		if ch == nil {
			continue
		}
		here := map[string]bool{}
		if ch.Status != Deleted {
			m := n.c.Module(ch.Module)
			if m == nil || m.State != Compiled {
				continue
			}
			for _, d := range m.Data.Decls {
				if name, isPrivate := unmangle(d.Name, m.Module); !isPrivate {
					here[name] = true
				}
			}
		}
		for _, d := range n.baseDecls[file] {
			name, isPrivate := unmangle(d.Name, d.Module)
			if isPrivate || here[name] {
				continue
			}
			n.removed = append(n.removed, &removal{Name: name, Target: d.Name, Path: file, Module: d.Module,
				Publicly: n.private[name]})
		}
	}
	if err := n.keepCompiledConstants(ctx); err != nil {
		return err
	}
	n.keepOutermost()
	if err := n.removedFields(ctx); err != nil {
		return err
	}
	n.keepOutermost()
	return n.addDoubts(ctx)
}

// keepCompiledConstants drops a removal that its own module, compiled, still has as a constant. The reference file
// records no declaration range for a constant that a command macro or `deriving` makes, so only the exporter tells
// such a constant from a removed one. It runs only when there is a removal to check.
func (n *nameState) keepCompiledConstants(ctx context.Context) error {
	if len(n.removed) == 0 {
		return nil
	}
	if err := n.loadCandidateConsts(ctx); err != nil {
		return err
	}
	n.removed = slices.DeleteFunc(n.removed, func(r *removal) bool {
		ch := n.c.Change(r.Path)
		return ch != nil && n.candidateConsts[ch.Module][r.Name]
	})
	return nil
}

// loadCandidateConsts reads, once, the exporter's constants of every compiled changed module, by module, private
// names unmangled out.
func (n *nameState) loadCandidateConsts(ctx context.Context) error {
	if n.candidateConsts != nil {
		return nil
	}
	candidate, _, err := n.c.Constants(ctx)
	if err != nil {
		return err
	}
	n.candidateConsts = make(map[string]map[string]bool, len(candidate))
	for module, constants := range candidate {
		names := make(map[string]bool, len(constants))
		for _, k := range constants {
			if name, isPrivate := unmangle(k.Name, module); !isPrivate {
				names[name] = true
			}
		}
		n.candidateConsts[module] = names
	}
	return nil
}

// keepOutermost drops a removal whose name lies below another removed name: a declaration that goes takes with it
// every name Lean derived from it (its constructor, its projections, its equation lemmas), and the outermost name
// is the one to report.
func (n *nameState) keepOutermost() {
	removed := make(map[string]bool, len(n.removed))
	for _, r := range n.removed {
		removed[r.Name] = true
	}
	n.removed = slices.DeleteFunc(n.removed, func(r *removal) bool {
		for name := r.Name; ; {
			cut := strings.LastIndexByte(name, '.')
			if cut < 0 {
				return false
			}
			name = name[:cut]
			if removed[name] {
				return true
			}
		}
	})
}

// readStructures records the base structures and classes of the changed modules, and, when there are any, the
// compiled constants of the candidate's modules, which is where a field lives. An index read without its
// compiled part knows no field, so nothing is read and no field is reported: a miss, never a false refusal.
func (n *nameState) readStructures(ctx context.Context) error {
	n.structures = map[string][]string{}
	if _, err := n.idx.CompiledCommit(ctx, true); err != nil {
		return nil
	}
	for file, decls := range n.baseDecls {
		if ch := n.c.Change(file); ch == nil || ch.Status == Deleted {
			continue
		}
		for _, d := range decls {
			name, isPrivate := unmangle(d.Name, d.Module)
			if !isPrivate && structureKinds[d.Kind] {
				module := normalName(d.Module)
				n.structures[module] = append(n.structures[module], name)
			}
		}
	}
	if len(n.structures) == 0 {
		return nil
	}
	candidate, _, err := n.c.Constants(ctx)
	if err != nil {
		return err
	}
	n.candidateConsts = make(map[string]map[string]bool, len(candidate))
	for module, constants := range candidate {
		names := make(map[string]bool, len(constants))
		for _, k := range constants {
			names[normalName(k.Name)] = true
		}
		n.candidateConsts[module] = names
	}
	return nil
}

// removedFields lists the fields and the constructor that a base structure or class had and the candidate's
// compiled module no longer has. The base's are the index's compiled constants below the structure's name
// (DeclarationParts), the candidate's the exporter's constants of the module that declares it now.
func (n *nameState) removedFields(ctx context.Context) error {
	for _, module := range slices.Sorted(maps.Keys(n.structures)) {
		names := n.structures[module]
		parts, err := n.idx.DeclarationParts(ctx, module, names)
		if err != nil {
			return err
		}
		owned := make(map[string]bool, len(names))
		for _, name := range names {
			owned[name] = true
		}
		for _, part := range parts {
			parent, ok := structureOf(owned, part)
			if !ok {
				continue
			}
			where := n.public[parent]
			if len(where) == 0 || n.candidateConsts[where[0].Module][part] {
				continue
			}
			n.removed = append(n.removed, &removal{Name: part, Target: part, Path: n.fileOf(module), Module: module,
				Structure: parent, Field: strings.TrimPrefix(part, parent+".")})
		}
	}
	return nil
}

// structureOf is the structure a compiled constant belongs to: its longest proper prefix among them.
func structureOf(structures map[string]bool, part string) (string, bool) {
	for name := part; ; {
		cut := strings.LastIndexByte(name, '.')
		if cut < 0 {
			return "", false
		}
		name = name[:cut]
		if structures[name] {
			return name, true
		}
	}
}

// addDoubts turns a removal whose use may still resolve into a flag: another declaration of the candidate has the
// removed name's last component, or the structure of a removed field now extends another structure, which may
// give the field.
func (n *nameState) addDoubts(ctx context.Context) error {
	var shorts []string
	for _, r := range n.removed {
		if r.Field == "" {
			shorts = append(shorts, lastComponent(r.Name))
		}
	}
	byShort := map[string][]string{}
	if len(shorts) != 0 {
		others, err := n.idx.DeclsShort(ctx, shorts)
		if err != nil {
			return err
		}
		for _, d := range others {
			if _, isPrivate := unmangle(d.Name, d.Module); isPrivate || n.c.ChangeOfModule(d.Module) != nil {
				continue
			}
			name := normalName(d.Name)
			byShort[lastComponent(name)] = append(byShort[lastComponent(name)], name)
		}
	}
	for name := range n.public {
		byShort[lastComponent(name)] = append(byShort[lastComponent(name)], name)
	}
	for _, r := range n.removed {
		if r.Field != "" {
			if n.extends(r.Structure) {
				r.Doubt = fmt.Sprintf("%s now extends a structure, which may still give the field", r.Structure)
			}
			continue
		}
		candidates := slices.DeleteFunc(slices.Clone(byShort[lastComponent(r.Name)]), func(s string) bool { return s == r.Name })
		if len(candidates) != 0 {
			sort.Strings(candidates)
			r.Doubt = fmt.Sprintf("a use written %s may resolve to %s", lastComponent(r.Name), candidates[0])
		}
	}
	return nil
}

// extends reports whether the candidate's structure extends another: Lean compiles a projection to each parent,
// `S.toParent`, among the structure's own constants.
func (n *nameState) extends(structure string) bool {
	where := n.public[structure]
	if len(where) == 0 {
		return false
	}
	for name := range n.candidateConsts[where[0].Module] {
		rest, ok := strings.CutPrefix(name, structure+".to")
		if ok && !strings.Contains(rest, ".") && startsUpper(rest) {
			return true
		}
	}
	return false
}

func startsUpper(s string) bool {
	return s != "" && unicode.IsUpper(firstRune(s))
}

// group is the recorded uses of one removed name in one module: how many, which is first, and the positions they
// sit at, which is where Lean's own error must sit for N1 to refuse.
type group struct {
	r      *removal
	module string
	first  declindex.Use
	count  int
	at     map[[2]int]bool
	lines  map[int]bool
}

// holds reports whether the base recorded a use of this removal where Lean's error sits. Lean reports a name at
// its first character, as the reference file records a use, so the column tells two uses on one line apart. It
// reports a removed field under the term whose structure it cannot find the field in, at that term's position
// rather than the field's, so a field matches on the line alone.
func (g *group) holds(e LeanError) bool {
	if g.r.Field != "" {
		return g.lines[e.Line]
	}
	return g.at[[2]int{e.Line, e.Col}]
}

// removedUses runs N1: a built module whose file no changes uses a name its own import closure no longer declares.
// A recorded base use is evidence, not proof: a use inside a tactic that has a fallback leaves a module that still
// builds. So every module N1 would refuse is compiled against the candidate first (Candidate.Confirm), and Lean
// decides: a module that compiles gives no finding, one that fails over the removal is refused, one that fails for
// another member's change is C1's, and one Lean could not compile is flagged. A changed module that uses a removed
// name is C1's too.
func (n *nameState) removedUses(ctx context.Context) ([]landing.Finding, error) {
	if len(n.removed) == 0 {
		return nil, nil
	}
	byTarget := map[string]*removal{}
	targets := make([]string, 0, len(n.removed))
	var names []string
	for _, r := range n.removed {
		byTarget[r.Target] = r
		targets = append(targets, r.Target)
		names = append(names, r.Name)
	}
	uses, err := n.idx.UsesOf(ctx, targets)
	if err != nil {
		return nil, err
	}
	// A use of a name below a removed one (an equation lemma of a removed definition) fails with it, unless the
	// candidate declares that name itself.
	for _, u := range uses {
		if _, ok := byTarget[u.Target]; !ok {
			names = append(names, normalName(u.Target))
		}
	}
	if err := n.askUnchanged(ctx, names); err != nil {
		return nil, err
	}
	groups := map[[2]string]*group{}
	var order [][2]string
	for _, u := range uses {
		r := owner(byTarget, u.Target)
		if r == nil {
			continue
		}
		module := normalName(u.Module)
		if !n.c.Built(module) || n.c.ChangeOfModule(module) != nil {
			continue
		}
		if n.reachable(normalName(u.Target), module) || (u.Target != r.Target && n.reachable(r.Name, module)) {
			continue
		}
		key := [2]string{r.Target, module}
		g, ok := groups[key]
		if !ok {
			g = &group{r: r, module: module, first: u, at: map[[2]int]bool{}, lines: map[int]bool{}}
			groups[key] = g
			order = append(order, key)
		} else if u.Line < g.first.Line || (u.Line == g.first.Line && u.Col < g.first.Col) {
			g.first = u
		}
		g.at[[2]int{u.Line, u.Col}] = true
		g.lines[u.Line] = true
		g.count++
	}
	if len(order) == 0 {
		return nil, nil
	}
	users := map[string]bool{}
	for _, key := range order {
		users[key[1]] = true
	}
	if err := n.c.Confirm(ctx, slices.Sorted(maps.Keys(users))); err != nil {
		return nil, err
	}
	counts := map[*removal][2]int{} // uses and modules of the groups kept
	order = slices.DeleteFunc(order, func(key [2]string) bool {
		m := n.c.Module(key[1])
		if m != nil && m.State == Compiled {
			return true
		}
		g := groups[key]
		// A module Lean refused for something else is another member's doing, not this removal's: C1 reports the
		// names Lean's own errors carry, and N1 keeps quiet about it.
		if m != nil && m.State == Failed && !n.blamed(m, g) {
			return true
		}
		c := counts[g.r]
		counts[g.r] = [2]int{c[0] + g.count, c[1] + 1}
		return false
	})
	var out []landing.Finding
	for _, key := range order {
		g := groups[key]
		r := g.r
		c := counts[r]
		verdict, doubt := landing.VerdictRefuse, ""
		if m := n.c.Module(g.module); m == nil || m.State != Failed {
			verdict, doubt = landing.VerdictFlag, "; Lean could not compile this module against the candidate to confirm"+r.doubt()
		}
		n.reportedModules[g.module] = true
		out = append(out, landing.Finding{
			Path: n.fileOf(g.module), Line: max(g.first.Line, 1), Col: max(g.first.Col, 1),
			Verdict: verdict, Declaration: r.Name, Kind: r.kind(),
			Reason: fmt.Sprintf("N1: %s; %s in %s, first here%s", r.what(), plural(c[0], "use"),
				plural(c[1], "unchanged module"), doubt)})
	}
	return out, nil
}

// blamed reports whether a module's compile failed over this removal. Either Lean's error sits on a line where the
// base recorded a use of the removed name and the landing program's blame reads, there, an unknown or field name
// that is the removed declaration — its full name, or a suffix of it, which is how Lean writes a name an open
// namespace shortens — or the module the name came from is gone from the candidate's build and Lean's error says
// that module is missing. A name that merely ends in the same last component is another declaration: that failure
// is another member's, and C1 names it.
func (n *nameState) blamed(m *Module, g *group) bool {
	r := g.r
	gone := n.c.Module(r.Module) == nil && !n.c.Built(normalName(r.Module))
	for _, e := range m.Errors {
		if gone && namesWord(e.Message, normalName(r.Module)) {
			return true
		}
		if !g.holds(e) {
			continue
		}
		for _, block := range blame.Parse(blameLog(m.Module, e), n.cfg.Roots).Blocks {
			for _, name := range blame.Names(block.Message) {
				if name.Kind != blame.Unknown && name.Kind != blame.Field {
					continue
				}
				if name.Value == r.Name || strings.HasSuffix(r.Name, "."+name.Value) {
					return true
				}
				// A removed field: at a line where the base recorded a use of it, a field Lean cannot place, and
				// the structure itself, are how Lean writes that the field went ("`S` has no field `f`", "Fields
				// missing: `g`", `g` being what the candidate has instead).
				if r.Field != "" && (name.Kind == blame.Field || name.Value == r.Structure) {
					return true
				}
			}
		}
	}
	return false
}

// namesWord reports whether text holds the name as a whole identifier, not inside a longer one.
func namesWord(text, name string) bool {
	for i := 0; ; {
		at := strings.Index(text[i:], name)
		if at < 0 {
			return false
		}
		at += i
		before, after := ' ', ' '
		if at > 0 {
			before, _ = utf8.DecodeLastRuneInString(text[:at])
		}
		if end := at + len(name); end < len(text) {
			after, _ = utf8.DecodeRuneInString(text[end:])
		}
		if !identRune(before) && before != '.' && !identRune(after) && after != '.' {
			return true
		}
		i = at + 1
	}
}

// reachable reports whether a user module's candidate import closure, the user included, holds a public
// declaration of the name: in a compiled changed module (a declaration or a compiled constant) or in an unchanged
// module of the base. A field never moves away from its structure, so a removed field is never reachable.
func (n *nameState) reachable(name, user string) bool {
	closure := n.importClosure(user)
	for _, l := range n.public[name] {
		if closure[l.Module] {
			return true
		}
	}
	for module, constants := range n.candidateConsts {
		if constants[name] && closure[module] {
			return true
		}
	}
	for _, d := range n.unchanged[name] {
		if closure[normalName(d.Module)] {
			return true
		}
	}
	return false
}

// importClosure is the set of modules a module imports in the candidate, directly or not, itself included.
func (n *nameState) importClosure(module string) map[string]bool {
	if closure, ok := n.closures[module]; ok {
		return closure
	}
	closure := map[string]bool{}
	stack := []string{module}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if closure[m] {
			continue
		}
		closure[m] = true
		stack = append(stack, n.c.graph[m]...)
	}
	n.closures[module] = closure
	return closure
}

// owner is the removal whose target is the use's target or the longest one it lies below.
func owner(byTarget map[string]*removal, target string) *removal {
	for name := target; ; {
		if r, ok := byTarget[name]; ok {
			return r
		}
		cut := strings.LastIndexAny(name, "./")
		if cut < 0 {
			return nil
		}
		name = name[:cut]
	}
}

// duplicates runs N2: a name that a compiled candidate module declares and the base did not declare in that
// module, which another module declares too, where one built module imports both. Lean refuses two declarations of
// one name in one module itself, so C1 reports that case.
func (n *nameState) duplicates(ctx context.Context) ([]landing.Finding, error) {
	type occurrence struct {
		module, path string
		line         int
		new          bool
	}
	byName := map[string][]occurrence{}
	var asked []string
	for _, ch := range n.c.Changes {
		m := n.c.Module(ch.Module)
		if m == nil || m.State != Compiled || !n.c.Built(ch.Module) {
			continue
		}
		base := map[string]bool{}
		for _, d := range n.baseDecls[ch.Path] {
			name, isPrivate := unmangle(d.Name, d.Module)
			if !isPrivate {
				base[name] = true
			}
		}
		for _, d := range m.Data.Decls {
			name, isPrivate := unmangle(d.Name, m.Module)
			if isPrivate {
				continue
			}
			if _, ok := byName[name]; !ok {
				asked = append(asked, name)
			}
			byName[name] = append(byName[name], occurrence{module: m.Module, path: ch.Path, line: d.Line, new: !base[name]})
		}
	}
	if err := n.askUnchanged(ctx, asked); err != nil {
		return nil, err
	}
	var fresh []string
	for _, name := range asked {
		if slices.ContainsFunc(byName[name], func(o occurrence) bool { return o.new }) {
			fresh = append(fresh, name)
		}
	}
	generated, err := n.generatedOwners(ctx, fresh)
	if err != nil {
		return nil, err
	}
	clashing := map[string]bool{}
	var out []landing.Finding
	for _, name := range asked {
		occurrences := slices.Clone(byName[name])
		for _, d := range n.unchanged[name] {
			if module := normalName(d.Module); n.c.Built(module) {
				occurrences = append(occurrences, occurrence{module: module, path: n.fileOf(module), line: d.Line})
			}
		}
		for _, d := range generated[name] {
			module := normalName(d.Module)
			occurrences = append(occurrences, occurrence{module: module, path: n.fileOf(module), line: d.Line})
		}
		if len(occurrences) < 2 {
			continue
		}
		sort.Slice(occurrences, func(i, j int) bool {
			if occurrences[i].path != occurrences[j].path {
				return occurrences[i].path < occurrences[j].path
			}
			return occurrences[i].line < occurrences[j].line
		})
		for i, o := range occurrences {
			if !o.new || below(clashing, name) {
				continue
			}
			for j, other := range occurrences {
				if i == j || other.module == o.module || !n.importedTogether(o.module, other.module) {
					continue
				}
				clashing[name] = true
				out = append(out, landing.Finding{Path: o.path, Line: o.line, Col: declarationColumn(n.c.Text(o.path), o.line),
					Verdict: landing.VerdictRefuse, Kind: blame.Added, Declaration: name,
					Reason: fmt.Sprintf("N2: %s is also declared in %s:%d; a built module imports both", name, other.path, other.line)})
				break
			}
		}
	}
	return out, nil
}

// generatedOwners finds, for each new name, the built unchanged declarations that Lean compiled it from: a
// structure's constructor or field, an equation lemma. No module's declarations hold those names, only the index's
// compiled constants do (DeclarationParts), so a new declaration of one is caught there. An index without its
// compiled part answers nothing: such a collision is then a miss, never a false refusal.
func (n *nameState) generatedOwners(ctx context.Context, names []string) (map[string][]declindex.Decl, error) {
	var prefixes []string
	for _, name := range names {
		for p := name; strings.Contains(p, "."); {
			p = p[:strings.LastIndexByte(p, '.')]
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	if err := n.askUnchanged(ctx, prefixes); err != nil {
		return nil, err
	}
	parts := map[[2]string]map[string]bool{} // (module, declaration) -> its compiled constants
	out := map[string][]declindex.Decl{}
	for _, name := range names {
		for p := name; strings.Contains(p, "."); {
			p = p[:strings.LastIndexByte(p, '.')]
			for _, d := range n.unchanged[p] {
				module := normalName(d.Module)
				if !n.c.Built(module) {
					continue
				}
				key := [2]string{d.Module, d.Name}
				if parts[key] == nil {
					list, err := n.idx.DeclarationParts(ctx, d.Module, []string{d.Name})
					if err != nil {
						return nil, err
					}
					parts[key] = map[string]bool{}
					for _, part := range list {
						parts[key][normalName(part)] = true
					}
				}
				if parts[key][name] {
					out[name] = append(out[name], d)
				}
			}
		}
	}
	return out, nil
}

// importedTogether reports whether one built module's import closure holds both modules: only there does Lean
// meet the two declarations and refuse the second. Two modules no built module imports together both build.
func (n *nameState) importedTogether(a, b string) bool {
	if n.reverse == nil {
		n.reverse = n.c.reverseGraph()
	}
	importers := n.c.importersOf(normalName(a), n.reverse)
	for m := range n.c.importersOf(normalName(b), n.reverse) {
		if importers[m] {
			return true
		}
	}
	return false
}

// below reports whether a name lies under one already reported: Lean derives names from a declaration, and the
// clash of the declaration itself is the one to report.
func below(reported map[string]bool, name string) bool {
	for {
		cut := strings.LastIndexByte(name, '.')
		if cut < 0 {
			return false
		}
		name = name[:cut]
		if reported[name] {
			return true
		}
	}
}

// declarationColumn is the 1-based column of a line's first character that is not white space. The compiled
// reference file records a declaration's line, not its column; this reads the candidate's text for the column
// only, and judges nothing by it.
func declarationColumn(text string, line int) int {
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return 1
	}
	col := 1
	for _, r := range lines[line-1] {
		if r != ' ' && r != '\t' {
			break
		}
		col++
	}
	return col
}

// unmangle gives a private declaration's name as its source writes it: Lean names it
// _private.<module>.<n>.<name>.
func unmangle(name, module string) (string, bool) {
	name = normalName(name)
	prefix := "_private." + normalName(module) + "."
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return name, false
	}
	digits, after, found := strings.Cut(rest, ".")
	if !found || strings.Trim(digits, "0123456789") != "" {
		return name, false
	}
	return after, true
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// sortFindings orders findings by path, line, column, declaration and reason.
func sortFindings(fs []landing.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Col != b.Col:
			return a.Col < b.Col
		case a.Declaration != b.Declaration:
			return a.Declaration < b.Declaration
		}
		return a.Reason < b.Reason
	})
}
