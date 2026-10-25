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

// DeclsWithShape gives the declarations whose shape (Decl.Shape) is one of shapes: renamed copies of a statement.
func (s *Store) DeclsWithShape(ctx context.Context, shapes []string) ([]Decl, error) {
	return s.declsWhere(ctx, "shape", shapes)
}

// declColumns are the columns of a Decl, in the order scanDecl reads them.
const declColumns = `name, short, module, kind, line, stmt_hash, shape`

func scanDecl(r *sql.Rows, out *[]Decl) error {
	var d Decl
	if err := r.Scan(&d.Name, &d.Short, &d.Module, &d.Kind, &d.Line, &d.StmtHash, &d.Shape); err != nil {
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
// target is the name, or begins with the name and a dot (a field, a constructor) or a slash (an auxiliary). It is
// the range that usersOf reads, without its owner resolution: a graph check compares the targets themselves.
func (s *Store) UsesOf(ctx context.Context, names []string) ([]Use, error) {
	var out []Use
	for _, name := range distinct(names) {
		rows, err := s.q.QueryContext(ctx, `SELECT target, tmodule, module, line, col, parent, cls FROM uses
			WHERE target = ? OR (target >= ? AND target < ?)`, name, name+".", name+"/")
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

// Head is one type head that a declaration's source writes (SourceHeads), as the index keeps it: Role is binder or
// conclusion, Head the name that ResolveHead gave it against every declaration of the index (a declared full name,
// else the name as written), and Line the declaration's line.
type Head struct {
	Name, Module, Role, Head string
	Line                     int
}

// Heads gives the heads of role (of any role when role is empty) that resolved to one of heads.
func (s *Store) Heads(ctx context.Context, role string, heads []string) ([]Head, error) {
	return s.headsWhere(ctx, "head", role, heads)
}

// HeadsOf gives every head of the named declarations.
func (s *Store) HeadsOf(ctx context.Context, names []string) ([]Head, error) {
	return s.headsWhere(ctx, "name", "", names)
}

// headsWhere gives the heads of role (any when empty) whose column (a heads column this file names) is one of values,
// sorted by name, module, role, head and line.
func (s *Store) headsWhere(ctx context.Context, column, role string, values []string) ([]Head, error) {
	var out []Head
	for chunk := range slices.Chunk(distinct(values), maxInParams) {
		query := `SELECT name, module, role, head, line FROM heads WHERE ` + column + ` IN (` + placeholders(len(chunk)) + `)`
		args := anys(chunk)
		if role != "" {
			query += ` AND role = ?`
			args = append(args, role)
		}
		rows, err := s.q.QueryContext(ctx, query, args...)
		err = eachRow(rows, err, func(r *sql.Rows) error {
			var h Head
			if err := r.Scan(&h.Name, &h.Module, &h.Role, &h.Head, &h.Line); err != nil {
				return err
			}
			out = append(out, h)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b Head) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Module, b.Module), cmp.Compare(a.Role, b.Role),
			cmp.Compare(a.Head, b.Head), cmp.Compare(a.Line, b.Line))
	})
	return out, nil
}

// declaredBy gives the names that modules declare, as the index holds them: a refresh compares them with what the
// modules declare after it (headModulesToResolve).
func declaredBy(ctx context.Context, q querier, modules []string) (map[string]bool, error) {
	out := map[string]bool{}
	err := eachRowIn(ctx, q, `SELECT name FROM decls WHERE module IN`, modules, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		out[name] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// headsByModule maps each module that holds a head to the distinct heads it holds.
func headsByModule(ctx context.Context, q querier) (map[string][]string, error) {
	out := map[string][]string{}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT module, head FROM heads`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var module, head string
		if err := r.Scan(&module, &head); err != nil {
			return err
		}
		out[module] = append(out[module], head)
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
