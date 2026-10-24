package declindex

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"slices"
)

// declRef names a declaration, or a constant that a use records: its module and its full name. Two modules that are
// never imported together can each declare one name, so a name alone does not name a declaration.
type declRef struct{ module, name string }

func (r declRef) compare(o declRef) int {
	return cmp.Or(cmp.Compare(r.name, o.name), cmp.Compare(r.module, o.module))
}

// declaring maps each name to the modules that declare it, each list sorted and without repeats.
type declaring map[string][]string

func (d declaring) add(r *sql.Rows) error {
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
	rows, err := q.QueryContext(ctx, `SELECT name, module FROM decls`)
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
	if err := eachRowIn(ctx, q, `SELECT name, module FROM decls WHERE name IN`, slices.Sorted(maps.Keys(cands)), d.add); err != nil {
		return nil, err
	}
	return d.sorted(), nil
}

// importGraph is the modules that each module the index read imports directly.
type importGraph map[string][]string

func loadImports(ctx context.Context, q querier) (importGraph, error) {
	g := importGraph{}
	rows, err := q.QueryContext(ctx, `SELECT module, imported FROM imports`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
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
}

// declSets maps a name to the modules that declare it, in some import closure: each list sorted, without repeats,
// and never changed once made, so that the maps of many modules share them.
type declSets map[string][]string

// merge adds o's declarations to d.
func (d declSets) merge(o declSets) {
	for n, ms := range o {
		d[n] = union(d[n], ms)
	}
}

// fits reports whether d and o share a declaration of every name that both hold.
func (d declSets) fits(o declSets) bool {
	for n, ms := range o {
		if have, ok := d[n]; ok && disjoint(have, ms) {
			return false
		}
	}
	return true
}

// clashes gives the names, sorted, for which d and o each hold declarations and share none.
func (d declSets) clashes(o declSets) []string {
	var out []string
	for n, ms := range o {
		if have, ok := d[n]; ok && disjoint(have, ms) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// union gives the sorted union of the sorted lists a and b: a or b itself, not a copy, when it holds the other.
func union(a, b []string) []string {
	switch {
	case subset(b, a):
		return a
	case subset(a, b):
		return b
	}
	out := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch c := cmp.Compare(a[i], b[j]); {
		case c < 0:
			out = append(out, a[i])
			i++
		case c > 0:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
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

// disjoint reports whether the sorted lists a and b share no element.
func disjoint(a, b []string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch c := cmp.Compare(a[i], b[j]); {
		case c < 0:
			i++
		case c > 0:
			j++
		default:
			return false
		}
	}
	return true
}

// exportGroups splits modules into the groups that one Lean environment can import together with base (the
// modules that the registry's forms import). Lean imports two declarations of one name together only when one
// subsumes the other: the same type, both theorems (Environment.lean, subsumesInfo). That relation is transitive,
// and each module's import closure built, so all of its declarations of a name load together, as do base's. Two
// sets of declarations of a name that share one therefore load together. Two that share none may not, and the index
// cannot tell without their elaborated statements, so they clash. A module joins a group when, for every name that
// both declare, the group's declarations and the module's share one. A module that clashes with base goes into a
// group of apart, to be read without base, and each name that clashes is recorded with it. A project that declares
// no name twice gives one group. Every declaration that a reference file records counts, also a name with a
// component that starts with `_`: a user can write one (`_Value`), and the names that Lean makes there hold their
// module (`_private.M.0.x`, `_aux_M_…`), so no two modules declare them.
func exportGroups(ctx context.Context, q querier, modules, base []string) (exportPlan, error) {
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
	// claims is, for a module, the modules of its import closure that declare each name declared twice.
	memo := map[string]declSets{}
	var claims func(m string) declSets
	claims = func(m string) declSets {
		if c, ok := memo[m]; ok {
			return c
		}
		memo[m] = nil // a cycle, which Lean refuses, ends here
		c := declSets{}
		for _, n := range twice[m] {
			c[n] = []string{m}
		}
		for _, i := range imports[m] {
			c.merge(claims(i))
		}
		memo[m] = c
		return c
	}
	baseClaims := declSets{}
	for _, b := range base {
		baseClaims.merge(claims(b))
	}
	type group struct {
		claims  declSets
		modules []string
	}
	var with, without []*group
	place := func(gs []*group, seed declSets, m string, c declSets) []*group {
		for _, g := range gs {
			if g.claims.fits(c) {
				g.claims.merge(c)
				g.modules = append(g.modules, m)
				return gs
			}
		}
		g := &group{claims: declSets{}, modules: []string{m}}
		g.claims.merge(seed)
		g.claims.merge(c)
		return append(gs, g)
	}
	for _, m := range modules {
		c := claims(m)
		names := baseClaims.clashes(c)
		if len(names) == 0 {
			with = place(with, baseClaims, m, c)
			continue
		}
		without = place(without, nil, m, c)
		for _, n := range names {
			plan.clashes = append(plan.clashes, FormsClash{Module: m, Name: n, DeclaredIn: c[n], FormsDeclaredIn: baseClaims[n]})
		}
	}
	for _, g := range with {
		plan.groups = append(plan.groups, g.modules)
	}
	for _, g := range without {
		plan.apart = append(plan.apart, g.modules)
	}
	slices.SortFunc(plan.clashes, func(a, b FormsClash) int {
		return cmp.Or(cmp.Compare(a.Module, b.Module), cmp.Compare(a.Name, b.Name))
	})
	return plan, nil
}
