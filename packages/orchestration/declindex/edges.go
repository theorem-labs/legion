package declindex

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// insertEdges adds the declaration-level edges of the re-read modules: for each user declaration, the targets
// it uses as recorded, with the declaration each target belongs to.
//
// The owner columns here are the recorded target itself (owner = target, owner_module = tmodule, by_closure
// false): resolving them needs the declarations and imports of other modules, which the lane that owns the graph
// reads through this transaction.
func insertEdges(ctx context.Context, tx pgx.Tx, results []ModuleData, errs []error) error {
	type pair struct{ target, tmodule, dst string }
	row := make([]any, 9)
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"edges"},
		[]string{"target", "tmodule", "dst", "module", "stmt", "kind", "owner", "owner_module", "by_closure"},
		copyFrom(func(yield func([]any) bool) {
			for i, md := range results {
				if errs[i] != nil {
					continue
				}
				kind := map[string]string{}
				for _, d := range md.Decls {
					kind[d.Name] = d.Kind
				}
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
					row[0], row[1], row[2], row[3] = p.target, p.tmodule, p.dst, md.Module
					row[4], row[5] = agg[p], kind[p.dst]
					row[6], row[7], row[8] = p.target, p.tmodule, false
					if !yield(row) {
						return
					}
				}
			}
		}))
	return err
}
