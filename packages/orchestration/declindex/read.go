package declindex

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"slices"
)

// The reads that graph checks make of an index: a candidate's declarations, uses and imports against the built
// base's. Each takes a list of keys and gives every row that matches one of them, sorted, so that a caller needs no
// SQL of its own.

// Commit is the commit whose build the index describes (meta "commit").
func (s *Store) Commit(ctx context.Context) (string, error) {
	return s.Meta(ctx, "commit")
}

// ModulePaths maps every module the index read to its source path, relative to the tree. The map is the caller's.
func (s *Store) ModulePaths(ctx context.Context) (map[string]string, error) {
	paths, err := s.modulePaths(ctx)
	if err != nil {
		return nil, err
	}
	return maps.Clone(paths), nil
}

// DeclsInModules gives the declarations of the modules.
func (s *Store) DeclsInModules(ctx context.Context, modules []string) ([]Decl, error) {
	return s.declsWhere(ctx, "module", modules)
}

// DeclsNamed gives the declarations of each full name.
func (s *Store) DeclsNamed(ctx context.Context, names []string) ([]Decl, error) {
	return s.declsWhere(ctx, "name", names)
}

// DeclsShort gives the declarations whose last name component is one of shorts.
func (s *Store) DeclsShort(ctx context.Context, shorts []string) ([]Decl, error) {
	return s.declsWhere(ctx, "short", shorts)
}

// declColumns are the columns of a Decl, in the order scanDecl reads them.
const declColumns = `name, short, module, kind, line, stmt_hash`

func scanDecl(r *sql.Rows, out *[]Decl) error {
	var d Decl
	if err := r.Scan(&d.Name, &d.Short, &d.Module, &d.Kind, &d.Line, &d.StmtHash); err != nil {
		return err
	}
	*out = append(*out, d)
	return nil
}

// declsWhere gives the declarations whose column (a decls column this file names) is one of values, sorted by name,
// module and line.
func (s *Store) declsWhere(ctx context.Context, column string, values []string) ([]Decl, error) {
	var out []Decl
	err := eachRowIn(ctx, s.q, `SELECT `+declColumns+` FROM decls WHERE `+column+` IN`, distinct(values),
		func(r *sql.Rows) error { return scanDecl(r, &out) })
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Decl) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Module, b.Module), cmp.Compare(a.Line, b.Line))
	})
	return out, nil
}

// UsesOf gives the recorded uses of each named declaration and of the constants Lean made from it: those whose
// target is the name, or begins with the name and a dot (a field, a constructor, an equation lemma) or a slash (a
// name a macro made). The range is from the name and a dot to the name and the character after a slash, which are
// the only two characters between them, so one index range holds both and nothing else. A graph check compares the
// targets themselves, so this read does not resolve which declaration a target belongs to, as usersOf does.
func (s *Store) UsesOf(ctx context.Context, names []string) ([]Use, error) {
	var out []Use
	for _, name := range distinct(names) {
		rows, err := s.q.QueryContext(ctx, `SELECT target, tmodule, module, line, col, parent, cls FROM uses
			WHERE target = ? OR (target >= ? AND target < ?)`, name, name+".", name+"0")
		err = eachRow(rows, err, func(r *sql.Rows) error {
			var u Use
			if err := r.Scan(&u.Target, &u.TargetModule, &u.Module, &u.Line, &u.Col, &u.Parent, &u.Class); err != nil {
				return err
			}
			out = append(out, u)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sortUses(out)
	return out, nil
}

// Imports maps every module the index read to the modules that it imports directly, sorted; a module that imports
// none maps to nil.
func (s *Store) Imports(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	rows, err := s.q.QueryContext(ctx, `SELECT m.module, i.imported FROM modules m
		LEFT JOIN imports i ON i.module = m.module ORDER BY m.module, i.imported`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var module string
		var imported sql.NullString
		if err := r.Scan(&module, &imported); err != nil {
			return err
		}
		list := out[module]
		if imported.Valid {
			list = append(list, imported.String)
		}
		out[module] = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// distinct is values without repeats, in their first order.
func distinct(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
