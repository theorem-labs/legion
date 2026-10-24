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

// exportGroups splits modules into the groups that one Lean environment can import together with base (the
// modules that the registry's forms import): within a group, no name is declared by two of the modules that the
// group's modules and base import, as Lean refuses such an environment. A module whose imports clash with base's
// goes into a group of apart, to be read without base. A project that declares no name twice gives one group.
// Names that Lean made (madeByLean) are left out: they are unique to their module, or no constant at all. clashing
// are the names that more than one module declares.
func exportGroups(ctx context.Context, q querier, modules, base []string) (groups, apart [][]string, clashing map[string]bool, err error) {
	all, err := allDeclaring(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	// twice: for each module that declares a name another module also declares, those names.
	twice := map[string][]string{}
	clashing = map[string]bool{}
	for n, ms := range all {
		if len(ms) > 1 && !madeByLean(n) {
			clashing[n] = true
			for _, m := range ms {
				twice[m] = append(twice[m], n)
			}
		}
	}
	if len(twice) == 0 {
		return [][]string{modules}, nil, clashing, nil
	}
	imports, err := loadImports(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	// claims is, for a module, the module of its import closure that declares each name declared twice.
	memo := map[string]map[string]string{}
	var claims func(m string) map[string]string
	claims = func(m string) map[string]string {
		if c, ok := memo[m]; ok {
			return c
		}
		memo[m] = nil // a cycle, which Lean refuses, ends here
		c := map[string]string{}
		for _, n := range twice[m] {
			c[n] = m
		}
		for _, i := range imports[m] {
			maps.Copy(c, claims(i))
		}
		memo[m] = c
		return c
	}
	fits := func(have, add map[string]string) bool {
		for n, m := range add {
			if h, ok := have[n]; ok && h != m {
				return false
			}
		}
		return true
	}
	baseClaims := map[string]string{}
	for _, b := range base {
		maps.Copy(baseClaims, claims(b))
	}
	type group struct {
		claims  map[string]string
		modules []string
	}
	var with, without []*group
	place := func(gs []*group, seed map[string]string, m string, c map[string]string) []*group {
		for _, g := range gs {
			if fits(g.claims, c) {
				maps.Copy(g.claims, c)
				g.modules = append(g.modules, m)
				return gs
			}
		}
		g := &group{claims: map[string]string{}, modules: []string{m}}
		maps.Copy(g.claims, seed)
		maps.Copy(g.claims, c)
		return append(gs, g)
	}
	for _, m := range modules {
		c := claims(m)
		if fits(baseClaims, c) {
			with = place(with, baseClaims, m, c)
		} else {
			without = place(without, nil, m, c)
		}
	}
	for _, g := range with {
		groups = append(groups, g.modules)
	}
	for _, g := range without {
		apart = append(apart, g.modules)
	}
	return groups, apart, clashing, nil
}
