package declindex

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// propagates says whether an edge passes a change on: a use in a statement, or any use by a definition (defKinds).
var propagates = `(e.stmt OR e.kind IN ('` + strings.Join(defKinds, `', '`) + `'))`

// reachEdge is one edge of the declaration graph out of a declaration: the declaration that uses it, and whether
// the use passes a change on.
type reachEdge struct {
	dst  declRef
	prop bool
}

// edgesFrom reads the edges whose used declaration is one of nodes, by that declaration: the refresh resolved each
// use to the declaration it belongs to (edges.owner, edges.owner_module), so one indexed read gives a node's
// users. Two uses of one declaration by one user give one edge, which propagates when either use does, and a
// declaration's use of itself is no edge.
func edgesFrom(ctx context.Context, q querier, nodes []declRef) (map[declRef][]reachEdge, error) {
	names, modules := make([]string, len(nodes)), make([]string, len(nodes))
	for i, n := range nodes {
		names[i], modules[i] = n.name, n.module
	}
	out := map[declRef][]reachEdge{}
	at := map[[2]declRef]int{}
	rows, err := q.Query(ctx, `SELECT e.owner, e.owner_module, e.dst, e.module, `+propagates+` FROM edges e
		JOIN unnest($1::text[], $2::text[]) AS n(name, module) ON e.owner = n.name AND e.owner_module = n.module`,
		names, modules)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var src, dst declRef
		var prop bool
		if err := r.Scan(&src.name, &src.module, &dst.name, &dst.module, &prop); err != nil {
			return err
		}
		if src == dst {
			return nil
		}
		if i, ok := at[[2]declRef{src, dst}]; ok {
			out[src][i].prop = out[src][i].prop || prop
			return nil
		}
		at[[2]declRef{src, dst}] = len(out[src])
		out[src] = append(out[src], reachEdge{dst: dst, prop: prop})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A walk follows a node's users in one order whatever the server returns them in: by name, then by module.
	for _, es := range out {
		slices.SortFunc(es, func(a, b reachEdge) int { return a.dst.compare(b.dst) })
	}
	return out, nil
}

// walk computes the statements that a change to root changes (followed through propagating edges) and the proofs
// at their edge, and for each, the declaration it was first reached from. It reads one level of the graph at a
// time, so a change that reaches nothing reads almost nothing.
func (s *Store) walk(ctx context.Context, root declRef) (changed, reprove map[declRef]bool, via map[declRef]declRef, err error) {
	changed, reprove, via = map[declRef]bool{}, map[declRef]bool{}, map[declRef]declRef{}
	inClosure := map[declRef]bool{root: true}
	frontier := []declRef{root}
	// The edges that do not propagate are the proofs at the edge of the change, and are counted once the closure
	// is known: a declaration the closure holds is changed, not reproved.
	type edge struct{ src, dst declRef }
	var atEdge []edge
	for len(frontier) > 0 {
		users, err := edgesFrom(ctx, s.q, frontier)
		if err != nil {
			return nil, nil, nil, err
		}
		var next []declRef
		for _, src := range frontier {
			for _, e := range users[src] {
				if !e.prop {
					atEdge = append(atEdge, edge{src, e.dst})
					continue
				}
				if e.dst != root {
					changed[e.dst] = true
				}
				if !inClosure[e.dst] {
					inClosure[e.dst] = true
					via[e.dst] = src
					next = append(next, e.dst)
				}
			}
		}
		frontier = next
	}
	for _, e := range atEdge {
		if inClosure[e.dst] {
			continue
		}
		reprove[e.dst] = true
		if _, ok := via[e.dst]; !ok {
			via[e.dst] = e.src
		}
	}
	return changed, reprove, via, nil
}

// reach computes the statements a change to root's statement changes, and the proofs at their edge, on the
// declaration graph in the edges table.
func (s *Store) reach(ctx context.Context, root declRef, why bool) (Reach, error) {
	paths, err := s.modulePaths(ctx)
	if err != nil {
		return Reach{}, err
	}
	changed, reprove, via, err := s.walk(ctx, root)
	if err != nil {
		return Reach{}, err
	}
	// byDir groups the reached declarations by the directory of their module's source.
	byDir := func(set map[declRef]bool) map[string][]declRef {
		out := map[string][]declRef{}
		for d := range set {
			dir := dirOf(pathOf(paths, d.module))
			out[dir] = append(out[dir], d)
		}
		return out
	}
	cdirs, rdirs := byDir(changed), byDir(reprove)
	all := map[string]bool{}
	for d := range cdirs {
		all[d] = true
	}
	for d := range rdirs {
		all[d] = true
	}
	r := Reach{ReachCounts: ReachCounts{
		StatementsChanged: len(changed), StatementDirectories: len(cdirs),
		ProofsToRedo: len(reprove), ProofDirectories: len(rdirs), AllDirectories: len(all),
	}}
	if !why {
		return r, nil
	}
	r.Why = map[string]string{}
	short := root.name[strings.LastIndexByte(root.name, '.')+1:]
	for d := range all {
		ds := cdirs[d]
		if ds == nil {
			ds = rdirs[d]
		}
		chain := []declRef{slices.MinFunc(ds, declRef.compare)}
		for len(chain) < 50 {
			v, ok := via[chain[len(chain)-1]]
			if !ok || v == root {
				break
			}
			chain = append(chain, v)
		}
		slices.Reverse(chain)
		names := make([]string, len(chain))
		for i, c := range chain {
			names[i] = c.name
		}
		r.Why[d] = short + " -> " + strings.Join(names, " -> ")
	}
	return r, nil
}

// reachedModules are the modules, other than skip, of the statements that a change to defs changes.
func (s *Store) reachedModules(ctx context.Context, defs []declRef, skip []string) ([]string, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	done := map[string]bool{}
	for _, m := range skip {
		done[m] = true
	}
	var out []string
	for _, d := range defs {
		changed, _, _, err := s.walk(ctx, d)
		if err != nil {
			return nil, err
		}
		for r := range changed {
			if !done[r.module] {
				done[r.module] = true
				out = append(out, r.module)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
