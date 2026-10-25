package declindex

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// propagates says whether an edge passes a change on: a use in a statement, or any use by a definition (defKinds).
var propagates = `(e.stmt OR e.kind = ANY('{` + strings.Join(defKinds, ",") + `}'))`

// outEdge is one declaration that uses another, and whether a change passes to it.
type outEdge struct {
	dst  declRef
	prop bool
}

// outEdges reads the edges out of every declaration of frontier in one query, on the index of (owner_module,
// owner): the declarations that use a constant of one of them. Two constants of one owner give one edge, which
// propagates when either use does, and a declaration's use of its own constant is no edge. Each declaration's
// edges come ordered by the name and then the module of their user, the walk's tie-break.
func (s *Store) outEdges(ctx context.Context, frontier []declRef) (map[declRef][]outEdge, error) {
	modules, names := make([]string, len(frontier)), make([]string, len(frontier))
	for i, d := range frontier {
		modules[i], names[i] = d.module, d.name
	}
	out := make(map[declRef][]outEdge, len(frontier))
	rows, err := s.q.Query(ctx, `
		SELECT e.owner_module, e.owner, e.dst, e.module, bool_or(`+propagates+`)
		  FROM unnest($1::text[], $2::text[]) AS f (m, n)
		  JOIN edges e ON e.owner_module = f.m AND e.owner = f.n
		 WHERE e.dst <> e.owner OR e.module <> e.owner_module
		 GROUP BY e.owner_module, e.owner, e.dst, e.module
		 ORDER BY e.dst, e.module`, modules, names)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var src, dst declRef
		var prop bool
		if err := r.Scan(&src.module, &src.name, &dst.name, &dst.module, &prop); err != nil {
			return err
		}
		out[src] = append(out[src], outEdge{dst, prop})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// walk computes the statements that a change to root changes (followed through propagating edges), and for each
// the declaration it was first reached from. With reprove it also gives the proofs at the edge of that closure,
// the users a change does not pass through, and the declaration each was first reached from.
//
// It walks the edges table breadth first, one query for each level over the whole level's declarations, and reads
// only the rows it reaches.
func (s *Store) walk(ctx context.Context, root declRef, reprove bool) (map[declRef]bool, map[declRef]bool, map[declRef]declRef, error) {
	changed, proofs, via := map[declRef]bool{}, map[declRef]bool{}, map[declRef]declRef{}
	inClosure := map[declRef]bool{root: true}
	frontier := []declRef{root}
	var atEdge [][2]declRef // the edges out of the closure that do not propagate, checked after it
	for len(frontier) > 0 {
		out, err := s.outEdges(ctx, frontier)
		if err != nil {
			return nil, nil, nil, err
		}
		var next []declRef
		for _, src := range frontier {
			for _, e := range out[src] {
				if !e.prop {
					if reprove {
						atEdge = append(atEdge, [2]declRef{src, e.dst})
					}
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
		if inClosure[e[1]] {
			continue
		}
		proofs[e[1]] = true
		if _, ok := via[e[1]]; !ok {
			via[e[1]] = e[0]
		}
	}
	return changed, proofs, via, nil
}

// reach computes the statements a change to root's statement changes, and the proofs at their edge.
func (s *Store) reach(ctx context.Context, root declRef, why bool) (Reach, error) {
	changed, reprove, via, err := s.walk(ctx, root, true)
	if err != nil {
		return Reach{}, err
	}
	paths, err := s.modulePaths(ctx)
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
		changed, _, _, err := s.walk(ctx, d, false)
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
