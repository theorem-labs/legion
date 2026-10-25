package declindex

import (
	"context"
	"database/sql"
)

// insertEdges adds the declaration-level edges of the re-read modules: for each user declaration, the targets
// it uses as recorded. Which declaration a target belongs to (`X.wp` is X's field while its module declares no
// `X.wp`) depends on the declarations and imports of other modules, so buildGraph decides it when it writes the graph.
func insertEdges(ctx context.Context, tx *sql.Tx, results []ModuleData, errs []error) error {
	edges := newBatch(tx, "INSERT", "edges", 6)
	for i, md := range results {
		if errs[i] != nil {
			continue
		}
		kind := map[string]string{}
		for _, d := range md.Decls {
			kind[d.Name] = d.Kind
		}
		type pair struct{ target, tmodule, dst string }
		agg := map[pair]bool{}
		var order []pair
		for _, u := range md.Uses {
			if u.Parent == "" {
				continue
			}
			p := pair{u.Target, u.TargetModule, u.Parent}
			if _, ok := agg[p]; !ok {
				order = append(order, p)
			}
			agg[p] = agg[p] || u.Class == "stmt"
		}
		for _, p := range order {
			stmt := 0
			if agg[p] {
				stmt = 1
			}
			if err := edges.add(ctx, p.target, p.tmodule, p.dst, md.Module, stmt, kind[p.dst]); err != nil {
				return err
			}
		}
	}
	return edges.flush(ctx)
}
