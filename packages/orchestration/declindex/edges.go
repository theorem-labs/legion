package declindex

import (
	"context"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// edgeColumns are the columns of one edge row, in the order insertEdges copies them.
var edgeColumns = []string{"target", "tmodule", "dst", "module", "stmt", "kind", "owner", "owner_module", "by_closure"}

// insertEdges adds the declaration-level edges of the re-read modules: for each user declaration, the targets it
// uses as recorded, with the declaration each target belongs to (owner, owner_module). It resolves that owner here,
// through tx, which already holds the re-read modules' declarations and imports, so a row is written against the
// index the refresh leaves. It reads only the declarations of the dotted prefixes of the target names, never all of
// decls, and resolves the constants in the order of their module, so each import closure is read once.
//
// Which declaration a target belongs to (`X.wp` is X's field while its module declares no `X.wp`) also depends on
// the declarations and imports of modules this refresh does not re-read, so a later landing can move it:
// reresolveOwners updates the rows that one moved.
func insertEdges(ctx context.Context, tx pgx.Tx, results []ModuleData, errs []error) error {
	type pair struct{ target, tmodule, dst string }
	type edge struct {
		pair
		module, kind string
		stmt         bool
	}
	var edges []edge
	names := map[string]bool{}
	consts := map[declRef]bool{}
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
			edges = append(edges, edge{p, md.Module, kind[p.dst], agg[p]})
			names[p.target] = true
			consts[declRef{p.tmodule, p.target}] = true
		}
	}
	if len(edges) == 0 {
		return nil
	}
	owners, err := prefixResolver(ctx, tx, slices.Sorted(maps.Keys(names)))
	if err != nil {
		return err
	}
	owned, err := owners.all(consts)
	if err != nil {
		return err
	}
	rows := make([][]any, len(edges))
	for i, e := range edges {
		o := owned[declRef{e.tmodule, e.target}]
		rows[i] = []any{e.target, e.tmodule, e.dst, e.module, e.stmt, e.kind, o.ref.name, o.ref.module, o.byClosure}
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"edges"}, edgeColumns, pgx.CopyFromRows(rows))
	return err
}

// indexBefore is what the owners of the edges a refresh does not rewrite were resolved against: the declarations
// and the direct imports of the modules the refresh re-reads or drops. writeModules deletes those rows, so the
// refresh reads them first (readBeforeModules) and reresolveOwners compares them with what it wrote.
type indexBefore struct {
	decls   map[declRef]bool    // the (module, name) of each declaration
	imports map[string][]string // each module's direct imports, sorted and without repeats
}

// readBeforeModules reads the declarations and imports of the modules this refresh re-reads or drops. It runs in
// the refresh's write transaction, before writeModules deletes them.
func readBeforeModules(ctx context.Context, q querier, r reference) (indexBefore, error) {
	b := indexBefore{decls: map[declRef]bool{}, imports: map[string][]string{}}
	var modules []string
	for _, rel := range slices.Concat(r.gone, relsOf(r.changed)) {
		if m := r.old[rel].module; m != "" {
			modules = append(modules, m)
		}
	}
	slices.Sort(modules)
	if modules = slices.Compact(modules); len(modules) == 0 {
		return b, nil
	}
	rows, err := q.Query(ctx, `SELECT name, module FROM decls WHERE module = ANY($1)`, modules)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var d declRef
		if err := r.Scan(&d.name, &d.module); err != nil {
			return err
		}
		b.decls[d] = true
		return nil
	}); err != nil {
		return indexBefore{}, err
	}
	rows, err = q.Query(ctx, `SELECT module, imported FROM imports WHERE module = ANY($1)`, modules)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var m, i string
		if err := r.Scan(&m, &i); err != nil {
			return err
		}
		b.imports[m] = append(b.imports[m], i)
		return nil
	}); err != nil {
		return indexBefore{}, err
	}
	for m, is := range b.imports {
		slices.Sort(is)
		b.imports[m] = slices.Compact(is)
	}
	return b, nil
}

// reresolveOwners brings the owner columns of the edges this refresh did not write back in line with the index it
// leaves, in the refresh's write transaction, just after writeModules. An owner depends on which modules declare
// the dotted prefixes of the constant's name and, when the constant's own module declares none of them, on that
// module's import closure. So a row that stays must be resolved again when
//
//	(a) its target is, or lies below, a declaration name this refresh added to or removed from a module, or
//	(b) the index's imports decided its owner (by_closure, which also marks a constant no declaration owns yet)
//	    and its module imports, directly or not, a module whose imports this refresh changed, added or removed.
//
// Only the rows whose owner really moves are written: a landing that adds no declaration and changes no import
// writes no row of another module, and one that does writes only the rows its change moved.
func reresolveOwners(ctx context.Context, tx pgx.Tx, r reference, before indexBefore, log io.Writer) error {
	t0 := time.Now()
	names := changedDeclNames(r, before)
	seeds := changedImports(r, before)
	if len(names) == 0 && len(seeds) == 0 {
		return nil
	}
	modules, err := closureChanged(ctx, tx, seeds)
	if err != nil {
		return err
	}
	rows, err := ownersToCheck(ctx, tx, names, modules)
	if err != nil || len(rows) == 0 {
		return err
	}
	targets := map[string]bool{}
	consts := map[declRef]bool{}
	for c := range rows {
		targets[c.ref.name] = true
		consts[c.ref] = true
	}
	owners, err := prefixResolver(ctx, tx, slices.Sorted(maps.Keys(targets)))
	if err != nil {
		return err
	}
	now, err := owners.all(consts)
	if err != nil {
		return err
	}
	// A constant whose rows this refresh rewrote is read back with its new owner and, where an older row of
	// another module kept the one before, with that one too: a difference in either moves every row of it.
	moved := map[declRef]owned{}
	for c := range rows {
		if o := now[c.ref]; o != c.owner {
			moved[c.ref] = o
		}
	}
	if len(moved) == 0 {
		return nil
	}
	cs := slices.SortedFunc(maps.Keys(moved), declRef.compare)
	target, tmodule := make([]string, len(cs)), make([]string, len(cs))
	owner, ownerModule, byClosure := make([]string, len(cs)), make([]string, len(cs)), make([]bool, len(cs))
	for i, c := range cs {
		o := moved[c]
		target[i], tmodule[i] = c.name, c.module
		owner[i], ownerModule[i], byClosure[i] = o.ref.name, o.ref.module, o.byClosure
	}
	tag, err := tx.Exec(ctx, `UPDATE edges e SET owner = u.owner, owner_module = u.owner_module, by_closure = u.by_closure
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::boolean[])
		  AS u (target, tmodule, owner, owner_module, by_closure)
		WHERE e.target = u.target AND e.tmodule = u.tmodule
		  AND (e.owner, e.owner_module, e.by_closure) IS DISTINCT FROM (u.owner, u.owner_module, u.by_closure)`,
		target, tmodule, owner, ownerModule, byClosure)
	if err != nil {
		return err
	}
	logf(log, "index: %d edges of %d constants given another owner in %s", tag.RowsAffected(), len(cs),
		time.Since(t0).Round(time.Millisecond))
	return nil
}

// constOwner is one constant of the edges table as it is stored, with the owner some row of it holds.
type constOwner struct {
	ref   declRef // the constant: its tmodule and its target
	owner owned
}

// ownersToCheck reads the constants whose owner this refresh may have moved: those at or below one of the changed
// declaration names (edges_target), and those whose owner the imports decided in a module whose closure changed
// (edges_closure). It gives each (constant, stored owner) once, so that a constant whose rows hold two owners,
// the one this refresh wrote and the one an older row of another module kept, is checked under both.
func ownersToCheck(ctx context.Context, q querier, names, modules []string) (map[constOwner]bool, error) {
	out := map[constOwner]bool{}
	scan := func(r pgx.Rows) error {
		var c constOwner
		if err := r.Scan(&c.ref.name, &c.ref.module, &c.owner.ref.name, &c.owner.ref.module, &c.owner.byClosure); err != nil {
			return err
		}
		out[c] = true
		return nil
	}
	if len(names) > 0 {
		// The columns are COLLATE "C", so the names below n are those between n + "." and n + "/".
		rows, err := q.Query(ctx, `
			SELECT target, tmodule, owner, owner_module, by_closure FROM edges WHERE target = ANY($1::text[])
			UNION
			SELECT e.target, e.tmodule, e.owner, e.owner_module, e.by_closure
			  FROM unnest($1::text[]) AS n JOIN edges e ON e.target >= n || '.' AND e.target < n || '/'`, names)
		if err := eachRow(rows, err, scan); err != nil {
			return nil, err
		}
	}
	if len(modules) > 0 {
		rows, err := q.Query(ctx, `SELECT DISTINCT target, tmodule, owner, owner_module, by_closure
			FROM edges WHERE tmodule = ANY($1) AND by_closure`, modules)
		if err := eachRow(rows, err, scan); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// changedDeclNames are the names this refresh added to or removed from a module's declarations, sorted.
func changedDeclNames(r reference, before indexBefore) []string {
	after := map[declRef]bool{}
	for i, md := range r.results {
		if r.errs[i] != nil {
			continue
		}
		for _, d := range md.Decls {
			after[declRef{d.Module, d.Name}] = true
		}
	}
	set := map[string]bool{}
	for d := range before.decls {
		if !after[d] {
			set[d.name] = true
		}
	}
	for d := range after {
		if !before.decls[d] {
			set[d.name] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// changedImports are the modules whose direct imports this refresh changed, added or removed, sorted. A module
// that left the index counts, and so does the old name of one that was renamed.
func changedImports(r reference, before indexBefore) []string {
	set := map[string]bool{}
	for _, rel := range r.gone {
		if m := r.old[rel].module; m != "" {
			set[m] = true
		}
	}
	for i, md := range r.results {
		old := r.old[r.changed[i].rel].module
		if r.errs[i] != nil {
			// The module could not be read: its rows left the index, as a gone module's do.
			if old != "" {
				set[old] = true
			}
			continue
		}
		if old != "" && old != md.Module {
			set[old] = true
		}
		now := slices.Clone(md.Imports)
		slices.Sort(now)
		if !slices.Equal(before.imports[md.Module], slices.Compact(now)) {
			set[md.Module] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// closureChanged are the modules whose import closure this refresh changed: those of seeds, and every module that
// imports one of them, directly or not. A module that no longer imports a seed is a seed itself, since its own
// imports changed, so the walk over the imports the refresh leaves finds every module whose closure moved. The
// walk is the query's, on imports_imported, so it reads the importers of the seeds and no other import row.
func closureChanged(ctx context.Context, q querier, seeds []string) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	var out []string
	rows, err := q.Query(ctx, `
		WITH RECURSIVE importers (module) AS (
			SELECT unnest($1::text[]) COLLATE "C"
			UNION
			SELECT i.module FROM importers u JOIN imports i ON i.imported = u.module
		)
		SELECT module FROM importers ORDER BY module`, seeds)
	if err := eachRow(rows, err, func(r pgx.Rows) error {
		var m string
		if err := r.Scan(&m); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
