package declindex

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// The reads that graph checks make of an index: a candidate's declarations, uses and imports against the built
// base's, and the compiled constants of either. Each takes a list of keys and gives every row that matches one of
// them, sorted, so that a caller needs no SQL of its own.

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

func scanDecl(r pgx.Rows, out *[]Decl) error {
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
	rows, err := s.q.Query(ctx, `SELECT `+declColumns+` FROM decls WHERE `+column+` = ANY($1)`, distinct(values))
	err = eachRow(rows, err, func(r pgx.Rows) error { return scanDecl(r, &out) })
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
// targets themselves, so this read does not resolve which declaration a target belongs to, as usersOf does. A name
// that is itself a part of another name asked for (Box.val beside Box) is not read again: the other's range holds
// its uses, so each use comes as many times as the index records it, whatever keys overlap.
func (s *Store) UsesOf(ctx context.Context, names []string) ([]Use, error) {
	keys := distinct(names)
	asked := make(map[string]bool, len(keys))
	for _, k := range keys {
		asked[k] = true
	}
	var out []Use
	for _, name := range keys {
		if partOfAnother(name, asked) {
			continue
		}
		rows, err := s.q.Query(ctx, `SELECT target, tmodule, module, line, col, parent, cls FROM uses
			WHERE target = $1 OR (target >= $2 AND target < $3)`, name, name+".", name+"0")
		err = eachRow(rows, err, func(r pgx.Rows) error {
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

// partOfAnother reports whether name begins with one of names and a dot or a slash: that name's uses range holds it.
func partOfAnother(name string, names map[string]bool) bool {
	for i := range len(name) {
		if (name[i] == '.' || name[i] == '/') && names[name[:i]] {
			return true
		}
	}
	return false
}

// Imports maps every module the index read to the modules that it imports directly, sorted; a module that imports
// none maps to nil.
func (s *Store) Imports(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	rows, err := s.q.Query(ctx, `SELECT m.module, i.imported FROM modules m
		LEFT JOIN imports i ON i.module = m.module ORDER BY m.module, i.imported`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var module string
		var imported *string
		if err := r.Scan(&module, &imported); err != nil {
			return err
		}
		list := out[module]
		if imported != nil {
			list = append(list, *imported)
		}
		out[module] = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConstantsIn gives the compiled constants of the modules as the index's compiled part holds them: what the
// exporter gave (Constant), without Atoms, Facts and Truncated, sorted by module and name. A module that a graph
// check compiles itself gives the same rows from Exporter.Run with Manifest.Types. The rows are of the index's commit
// only when CompiledCommit(ctx, true) accepts the index; else a module has none, or another commit's.
func (s *Store) ConstantsIn(ctx context.Context, modules []string) ([]Constant, error) {
	return s.constantsWhere(ctx, "module", modules)
}

// ConstantsConcluding gives the compiled constants whose Conclusion is one of heads, as ConstantsIn does: the
// declarations that produce a head (an instance of a class, a theorem that concludes a proposition).
func (s *Store) ConstantsConcluding(ctx context.Context, heads []string) ([]Constant, error) {
	return s.constantsWhere(ctx, "conclusion", heads)
}

// constantsWhere gives the constants whose column (a consts column this file names) is one of values, sorted by
// module and name.
func (s *Store) constantsWhere(ctx context.Context, column string, values []string) ([]Constant, error) {
	var out []Constant
	rows, err := s.q.Query(ctx, `SELECT name, module, kind, type, cstmt, binders, conclusion FROM consts WHERE `+column+` = ANY($1)`,
		distinct(values))
	err = eachRow(rows, err, func(r pgx.Rows) error {
		var c Constant
		var binders string
		if err := r.Scan(&c.Name, &c.Module, &c.Kind, &c.Type, &c.Hash, &binders, &c.Conclusion); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(binders), &c.Binders); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Constant) int { return cmp.Or(cmp.Compare(a.Module, b.Module), cmp.Compare(a.Name, b.Name)) })
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
