package declindex

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// declRef names a declaration, or a constant that a use records: its module and its full name. Two modules that are
// never imported together can each declare one name, so a name alone does not name a declaration.
type declRef struct{ module, name string }

func (r declRef) compare(o declRef) int {
	return cmp.Or(cmp.Compare(r.name, o.name), cmp.Compare(r.module, o.module))
}

// declaring maps each name to the modules that declare it, each list sorted and without repeats.
type declaring map[string][]string

func (d declaring) add(r pgx.Rows) error {
	var n, m string
	if err := r.Scan(&n, &m); err != nil {
		return err
	}
	d[n] = append(d[n], m)
	return nil
}

func (d declaring) sorted() declaring {
	for n, ms := range d {
		slices.Sort(ms)
		d[n] = slices.Compact(ms)
	}
	return d
}

// allDeclaring reads every declaration of the index.
func allDeclaring(ctx context.Context, q querier) (declaring, error) {
	d := declaring{}
	rows, err := q.Query(ctx, `SELECT name, module FROM decls`)
	if err := eachRow(rows, err, d.add); err != nil {
		return nil, err
	}
	return d.sorted(), nil
}

// declaringPrefixes reads the declarations of names and of their dotted prefixes: all that owner needs to resolve
// constants of those names.
func declaringPrefixes(ctx context.Context, q querier, names []string) (declaring, error) {
	cands := map[string]bool{}
	for _, n := range names {
		for p := range dottedPrefixes(n) {
			cands[p] = true
		}
	}
	d := declaring{}
	rows, err := q.Query(ctx, `SELECT name, module FROM decls WHERE name = ANY($1)`, slices.Sorted(maps.Keys(cands)))
	if err := eachRow(rows, err, d.add); err != nil {
		return nil, err
	}
	return d.sorted(), nil
}

// importGraph is the modules that each module the index read imports directly.
type importGraph map[string][]string

func loadImports(ctx context.Context, q querier) (importGraph, error) {
	g := importGraph{}
	rows, err := q.Query(ctx, `SELECT module, imported FROM imports`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var m, i string
		if err := r.Scan(&m, &i); err != nil {
			return err
		}
		g[m] = append(g[m], i)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// closure is module and every module that it imports, directly or not: what one Lean environment of it holds.
func (g importGraph) closure(module string) map[string]bool {
	seen := map[string]bool{module: true}
	todo := []string{module}
	for len(todo) > 0 {
		m := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, i := range g[m] {
			if !seen[i] {
				seen[i] = true
				todo = append(todo, i)
			}
		}
	}
	return seen
}

// Importers are modules and every module the index read that imports one of them, directly or not: the modules
// whose Lean environment holds a declaration of modules.
func (s *Store) Importers(ctx context.Context, modules []string) (map[string]bool, error) {
	g, err := loadImports(ctx, s.q)
	if err != nil {
		return nil, err
	}
	importedBy := map[string][]string{}
	for m, is := range g {
		for _, i := range is {
			importedBy[i] = append(importedBy[i], m)
		}
	}
	seen := map[string]bool{}
	todo := slices.Clone(modules)
	for _, m := range modules {
		seen[m] = true
	}
	for len(todo) > 0 {
		m := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, by := range importedBy[m] {
			if !seen[by] {
				seen[by] = true
				todo = append(todo, by)
			}
		}
	}
	return seen, nil
}

// ownerResolver gives the declaration that a recorded constant belongs to (owner), from the declarations of the
// names it was made with and, when a name is declared by several modules, the index's imports.
type ownerResolver struct {
	ctx       context.Context
	q         querier
	declaring declaring
	imports   importGraph // read at its first need
	closures  map[string]map[string]bool
	memo      map[declRef]declRef
}

func newOwnerResolver(ctx context.Context, q querier, declaring declaring) *ownerResolver {
	return &ownerResolver{ctx: ctx, q: q, declaring: declaring, closures: map[string]map[string]bool{}, memo: map[declRef]declRef{}}
}

// maxClosures bounds the import closures that a resolver keeps: the graph's edges come ordered by the module of
// their constant, so each module's closure is computed about once.
const maxClosures = 64

// owner is the declaration that the constant c belongs to: the longest of c's name and its dotted prefixes that c's
// module declares (`X.mk` is X's constructor). A constant that Lean made in the module that uses it (an equation
// lemma `f.eq_1`, recorded under that module) belongs to the declaration of the longest prefix that a module in its
// module's import closure declares. A constant that belongs to no declaration is its own.
func (o *ownerResolver) owner(c declRef) (declRef, error) {
	if r, ok := o.memo[c]; ok {
		return r, nil
	}
	r, err := o.resolve(c)
	if err != nil {
		return declRef{}, err
	}
	o.memo[c] = r
	return r, nil
}

func (o *ownerResolver) resolve(c declRef) (declRef, error) {
	for p := range dottedPrefixes(c.name) {
		if _, ok := slices.BinarySearch(o.declaring[p], c.module); ok {
			return declRef{c.module, p}, nil
		}
	}
	for p := range dottedPrefixes(c.name) {
		mods := o.declaring[p]
		if len(mods) == 0 {
			continue
		}
		closure, err := o.closure(c.module)
		if err != nil {
			return declRef{}, err
		}
		for _, m := range mods {
			if closure[m] {
				return declRef{m, p}, nil
			}
		}
	}
	return c, nil
}

func (o *ownerResolver) closure(module string) (map[string]bool, error) {
	if c, ok := o.closures[module]; ok {
		return c, nil
	}
	if o.imports == nil {
		g, err := loadImports(o.ctx, o.q)
		if err != nil {
			return nil, err
		}
		o.imports = g
	}
	if len(o.closures) >= maxClosures {
		clear(o.closures)
	}
	c := o.imports.closure(module)
	o.closures[module] = c
	return c, nil
}

// exportPlan is how the compiled step reads a set of modules (exportGroups).
type exportPlan struct {
	groups [][]string // each read in one Lean environment with the registry's forms
	apart  [][]string // each read in one Lean environment without the forms
	// clashes gives, for each module of apart, each name that keeps it from the forms, sorted by module and name.
	clashes []FormsClash
	// clashing are the names that more than one module declares.
	clashing map[string]bool
	// checks counts the pairs of modules whose import together the plan asked Lean about, and loaded those that Lean
	// imported.
	checks, loaded int
}

// declSets maps a name to the modules that declare it in one import closure: each list sorted, without repeats, and
// never changed once made, so that the closures of many modules share them.
type declSets map[string][]string

// mergeAll gives the union of parts. For each name it copies the lists once, or shares the one that holds the
// others, so a module that imports N declarers of one name costs about N, not N².
func mergeAll(parts []declSets) declSets {
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	}
	lists := map[string][][]string{}
	for _, p := range parts {
		for n, ms := range p {
			lists[n] = append(lists[n], ms)
		}
	}
	out := make(declSets, len(lists))
	for n, ls := range lists {
		out[n] = unionAll(ls)
	}
	return out
}

// unionAll gives the sorted union of the sorted lists ls: the longest of them, not a copy, when it holds the others.
func unionAll(ls [][]string) []string {
	longest, total := ls[0], 0
	for _, l := range ls {
		total += len(l)
		if len(l) > len(longest) {
			longest = l
		}
	}
	if !slices.ContainsFunc(ls, func(l []string) bool { return !subset(l, longest) }) {
		return longest
	}
	out := make([]string, 0, total)
	for _, l := range ls {
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// subset reports whether the sorted list a holds no element that the sorted list b does not.
func subset(a, b []string) bool {
	j := 0
	for _, x := range a {
		for j < len(b) && b[j] < x {
			j++
		}
		if j == len(b) || b[j] != x {
			return false
		}
		j++
	}
	return true
}

// loadRule decides whether two sets of declarations of one name, each of which Lean loaded together, load together
// in one environment (exportGroups).
type loadRule struct {
	ctx context.Context
	// axioms gives, for each name that two modules declare, the modules whose declaration of it is an axiom.
	axioms map[string]map[string]bool
	// loads asks Lean whether one environment imports two modules (Exporter.loadsTogether); nil answers no.
	loads func(ctx context.Context, modules []string) (bool, error)
	// loaded holds each answer of loads, by the two modules in order.
	loaded map[[2]string]bool
	// err is the first error of loads: the plan is not made.
	err error
}

// joins reports whether the declarations have (a group's) and ms (a module's, sorted) of the name n load together.
// They must share one, or Lean must import a module of each together: Lean's relation needs one type, which each set,
// loaded, has, and one such pair gives both sets. Where the two hold two axioms between them and are not the same set,
// Lean must also import each pair of the axioms' modules together, which it does in any order only when the type is
// a proposition by its cheap check: two axioms that only a theorem loaded before them reconciles do not. A module
// that declares the name imports no other declaration of it, so such a pair meets exactly its two declarations.
func (r *loadRule) joins(n string, have map[string]bool, ms []string) bool {
	shared := 0
	for _, m := range ms {
		if have[m] {
			shared++
		}
	}
	var axioms []string
	if ax := r.axioms[n]; len(ax) >= 2 {
		for m := range have {
			if ax[m] {
				axioms = append(axioms, m)
			}
		}
		for _, m := range ms {
			if ax[m] && !have[m] {
				axioms = append(axioms, m)
			}
		}
	}
	if len(axioms) >= 2 {
		if shared == len(ms) && len(have) == len(ms) {
			return true
		}
		slices.Sort(axioms)
		for i, a := range axioms {
			for _, b := range axioms[i+1:] {
				if !r.loadTogether(a, b) {
					return false
				}
			}
		}
	}
	if shared > 0 {
		return true
	}
	for _, h := range slices.Sorted(maps.Keys(have)) {
		for _, m := range ms {
			if r.loadTogether(h, m) {
				return true
			}
		}
	}
	return false
}

// loadTogether asks loads, once for each pair, whether Lean imports the modules a and b together.
func (r *loadRule) loadTogether(a, b string) bool {
	if r.loads == nil || r.err != nil {
		return false
	}
	key := [2]string{min(a, b), max(a, b)}
	if ok, asked := r.loaded[key]; asked {
		return ok
	}
	ok, err := r.loads(r.ctx, key[:])
	if err != nil {
		r.err = err
	}
	r.loaded[key] = ok
	return ok
}

// declGroup is the modules of one exporter environment and, for each name, the modules that declare it in their
// import closures.
type declGroup struct {
	sets    map[string]map[string]bool
	modules []string
}

func newDeclGroup(seed declSets) *declGroup {
	g := &declGroup{sets: map[string]map[string]bool{}}
	g.add(seed)
	return g
}

func (g *declGroup) add(c declSets) {
	for n, ms := range c {
		set := g.sets[n]
		if set == nil {
			set = make(map[string]bool, len(ms))
			g.sets[n] = set
		}
		for _, m := range ms {
			set[m] = true
		}
	}
}

// fits reports whether every declaration set of c joins the group's (loadRule.joins).
func (g *declGroup) fits(r *loadRule, c declSets) bool {
	for n, ms := range c {
		if have, ok := g.sets[n]; ok && !r.joins(n, have, ms) {
			return false
		}
	}
	return true
}

// clashes gives the names, sorted, whose declaration sets in c do not join the group's.
func (g *declGroup) clashes(r *loadRule, c declSets) []string {
	var out []string
	for n, ms := range c {
		if have, ok := g.sets[n]; ok && !r.joins(n, have, ms) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// exportGroups splits modules into the groups that one Lean environment can import together with base (the
// modules that the registry's forms import). Lean imports a second declaration of a name only when one of the two
// subsumes the other (Environment.lean, subsumesInfo): the same type, and both theorems or a theorem and an axiom;
// two axioms only when a cheap check finds their type a proposition. Each module's import closure built, so its
// declarations of a name loaded together, in its import order, as did base's. Among theorems and at most one axiom
// that relation is transitive and does not depend on the order, so two such sets of declarations of a name load
// together when they share one, or when Lean imports a module of each together (loads, an import that reads nothing:
// the index cannot compare elaborated statements). Two axioms may have loaded only because a theorem came before
// them, so two sets that hold two axioms between them load together when they are the same set (the first module of
// the environment that imports any of them then imports them all in the order it built with), or when Lean imports
// each pair of the axioms' modules together, which makes the order irrelevant (loadRule.joins). A module
// joins a group when, for every name that both declare, the two sets load together. A module that does not load
// with base goes into a group of apart, to be read without base, and each name that clashes is recorded with it. A
// project that declares no name twice gives one group. Every declaration that a reference file records counts, also
// a name with a component that starts with `_`: a user can write one (`_Value`), and the names that Lean makes there
// hold their module (`_private.M.0.x`, `_aux_M_…`), so no two modules declare them.
func exportGroups(ctx context.Context, q querier, modules, base []string, loads func(context.Context, []string) (bool, error)) (exportPlan, error) {
	plan := exportPlan{clashing: map[string]bool{}}
	all, err := allDeclaring(ctx, q)
	if err != nil {
		return plan, err
	}
	// twice: for each module that declares a name another module also declares, those names.
	twice := map[string][]string{}
	for n, ms := range all {
		if len(ms) > 1 {
			plan.clashing[n] = true
			for _, m := range ms {
				twice[m] = append(twice[m], n)
			}
		}
	}
	if len(twice) == 0 {
		plan.groups = [][]string{modules}
		return plan, nil
	}
	imports, err := loadImports(ctx, q)
	if err != nil {
		return plan, err
	}
	rule := &loadRule{ctx: ctx, axioms: map[string]map[string]bool{}, loads: loads, loaded: map[[2]string]bool{}}
	rows, err := q.Query(ctx, `SELECT name, module FROM decls WHERE kind = 'axiom'`)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var n, m string
		if err := r.Scan(&n, &m); err != nil {
			return err
		}
		if plan.clashing[n] {
			if rule.axioms[n] == nil {
				rule.axioms[n] = map[string]bool{}
			}
			rule.axioms[n][m] = true
		}
		return nil
	}); err != nil {
		return plan, err
	}
	// claims is, for a module, the modules of its import closure that declare each name declared twice.
	memo := map[string]declSets{}
	var claims func(m string) declSets
	claims = func(m string) declSets {
		if c, ok := memo[m]; ok {
			return c
		}
		memo[m] = nil // a cycle, which Lean refuses, ends here
		var parts []declSets
		if own := twice[m]; len(own) > 0 {
			o := make(declSets, len(own))
			for _, n := range own {
				o[n] = []string{m}
			}
			parts = append(parts, o)
		}
		for _, i := range imports[m] {
			if c := claims(i); len(c) > 0 {
				parts = append(parts, c)
			}
		}
		c := mergeAll(parts)
		memo[m] = c
		return c
	}
	var baseParts []declSets
	for _, b := range base {
		baseParts = append(baseParts, claims(b))
	}
	baseClaims := mergeAll(baseParts)
	baseGroup := newDeclGroup(baseClaims)
	var with, without []*declGroup
	place := func(gs []*declGroup, seed declSets, m string, c declSets) []*declGroup {
		for _, g := range gs {
			if g.fits(rule, c) {
				g.add(c)
				g.modules = append(g.modules, m)
				return gs
			}
		}
		g := newDeclGroup(seed)
		g.add(c)
		g.modules = []string{m}
		return append(gs, g)
	}
	for _, m := range modules {
		c := claims(m)
		names := baseGroup.clashes(rule, c)
		if len(names) == 0 {
			with = place(with, baseClaims, m, c)
			continue
		}
		without = place(without, nil, m, c)
		for _, n := range names {
			plan.clashes = append(plan.clashes, FormsClash{Module: m, Name: n, DeclaredIn: c[n], FormsDeclaredIn: baseClaims[n]})
		}
	}
	if rule.err != nil {
		return plan, rule.err
	}
	for _, g := range with {
		plan.groups = append(plan.groups, g.modules)
	}
	for _, g := range without {
		plan.apart = append(plan.apart, g.modules)
	}
	for _, ok := range rule.loaded {
		plan.checks++
		if ok {
			plan.loaded++
		}
	}
	slices.SortFunc(plan.clashes, func(a, b FormsClash) int {
		return cmp.Or(cmp.Compare(a.Module, b.Module), cmp.Compare(a.Name, b.Name))
	})
	return plan, nil
}
