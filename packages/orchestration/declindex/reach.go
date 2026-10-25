package declindex

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
)

// propagates says whether an edge passes a change on: a use in a statement, or any use by a definition (defKinds).
var propagates = `(e.stmt = 1 OR e.kind IN ('` + strings.Join(defKinds, `', '`) + `'))`

// reach computes the statements a change to root's statement changes, and the proofs at their edge, on the
// declaration graph that the last refresh wrote.
func (s *Store) reach(ctx context.Context, root declRef, why bool) (Reach, error) {
	g, err := s.graph()
	if err != nil {
		return Reach{}, err
	}
	paths, err := s.modulePaths(ctx)
	if err != nil {
		return Reach{}, err
	}
	changed, reprove, via := g.walk(root)
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

// graph loads the declaration graph once for the store (derived), from its graph file: for a store opened read-only,
// the one in place when its snapshot began, which a later refresh's replacement does not change. A graph of another
// generation than the index's is refused.
func (s *Store) graph() (*graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.derived.graph != nil || s.derived.graphErr != nil {
		return s.derived.graph, s.derived.graphErr
	}
	data, err := io.ReadAll(io.NewSectionReader(s.graphFile, 0, math.MaxInt64))
	var g *graph
	if err == nil {
		g, err = parseGraph(s.path+".graph", data)
	}
	if err == nil {
		if want, metaErr := s.Meta(context.Background(), "graph"); metaErr != nil {
			err = metaErr
		} else if g.token != want {
			err = fmt.Errorf("the graph file is of generation %q, the index of %q", g.token, want)
		}
	}
	if err != nil {
		s.derived.graphErr = graphUnreadable(err)
		return nil, s.derived.graphErr
	}
	s.derived.graph = g
	return g, nil
}

func graphUnreadable(err error) error {
	return fmt.Errorf("the declaration graph is missing, unreadable or out of date: run declindex refresh: %w", err)
}

// reachedModules are the modules, other than skip, of the statements that a change to defs changes.
func (s *Store) reachedModules(defs []declRef, skip []string) ([]string, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	g, err := s.graph()
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, m := range skip {
		done[m] = true
	}
	var out []string
	for _, d := range defs {
		changed, _, _ := g.walk(d)
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
