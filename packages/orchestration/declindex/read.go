package declindex

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// IncompleteError says that an index cannot answer graph checks because it has unreadable modules or no modules.
type IncompleteError struct {
	Unreadable string
}

func (e *IncompleteError) Error() string {
	if e.Unreadable != "0" {
		return fmt.Sprintf("the index is incomplete: %s unreadable modules", e.Unreadable)
	}
	return "the index is incomplete: no modules"
}

// Commit returns the commit whose tree the index describes.
func (s *Store) Commit(ctx context.Context) (string, error) {
	return s.Meta(ctx, "commit")
}

// Complete refuses an index with unreadable modules or no indexed modules.
func (s *Store) Complete(ctx context.Context) error {
	unreadable, err := s.Meta(ctx, "errors")
	if err != nil {
		return err
	}
	if unreadable != "0" {
		return &IncompleteError{Unreadable: unreadable}
	}
	count, err := s.ModuleCount(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return &IncompleteError{Unreadable: unreadable}
	}
	return nil
}

// ModulePaths returns every indexed module's source path relative to the index tree.
func (s *Store) ModulePaths(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel, module FROM modules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]string{}
	for rows.Next() {
		var rel, module string
		if err := rows.Scan(&rel, &module); err != nil {
			return nil, err
		}
		paths[module] = sourcePath(rel, module)
	}
	return paths, rows.Err()
}

// DeclsInModules returns declarations in the named modules.
func (s *Store) DeclsInModules(ctx context.Context, modules []string) ([]Decl, error) {
	return s.declarations(ctx, "module", modules)
}

// DeclsNamed returns declarations with any of the supplied full names.
func (s *Store) DeclsNamed(ctx context.Context, names []string) ([]Decl, error) {
	return s.declarations(ctx, "name", names)
}

// DeclsShort returns declarations with any of the supplied short names.
func (s *Store) DeclsShort(ctx context.Context, shorts []string) ([]Decl, error) {
	return s.declarations(ctx, "short", shorts)
}

func (s *Store) declarations(ctx context.Context, column string, values []string) ([]Decl, error) {
	values = uniqueStrings(values)
	var declarations []Decl
	for start := 0; start < len(values); start += 500 {
		chunk := values[start:min(start+500, len(values))]
		rows, err := s.db.QueryContext(ctx, `SELECT name, short, module, kind, line, stmt_hash FROM decls WHERE `+column+` IN (`+
			placeholders(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var declaration Decl
			if err := rows.Scan(&declaration.Name, &declaration.Short, &declaration.Module, &declaration.Kind, &declaration.Line, &declaration.StmtHash); err != nil {
				rows.Close()
				return nil, err
			}
			declarations = append(declarations, declaration)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.Slice(declarations, func(i, j int) bool {
		if declarations[i].Name != declarations[j].Name {
			return declarations[i].Name < declarations[j].Name
		}
		if declarations[i].Module != declarations[j].Module {
			return declarations[i].Module < declarations[j].Module
		}
		return declarations[i].Line < declarations[j].Line
	})
	return declarations, nil
}

// UsesOf returns uses of each requested declaration and its generated parts.
func (s *Store) UsesOf(ctx context.Context, names []string) ([]Use, error) {
	names = uniqueStrings(names)
	var uses []Use
	for start := 0; start < len(names); start += 500 {
		for _, name := range names[start:min(start+500, len(names))] {
			rows, err := s.db.QueryContext(ctx, `SELECT target, module, line, col, parent, cls FROM uses
				WHERE target = ? OR (target >= ? AND target < ?)`, name, name+".", name+"/")
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var use Use
				if err := rows.Scan(&use.Target, &use.Module, &use.Line, &use.Col, &use.Parent, &use.Class); err != nil {
					rows.Close()
					return nil, err
				}
				uses = append(uses, use)
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].Target != uses[j].Target {
			return uses[i].Target < uses[j].Target
		}
		if uses[i].Module != uses[j].Module {
			return uses[i].Module < uses[j].Module
		}
		if uses[i].Line != uses[j].Line {
			return uses[i].Line < uses[j].Line
		}
		return uses[i].Col < uses[j].Col
	})
	return uses, nil
}

// Imports returns every indexed module's header imports.
func (s *Store) Imports(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.module, i.imported FROM modules m
		LEFT JOIN imports i ON i.module = m.module ORDER BY m.module, i.imported`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	imports := map[string][]string{}
	for rows.Next() {
		var module string
		var imported sql.NullString
		if err := rows.Scan(&module, &imported); err != nil {
			return nil, err
		}
		if _, ok := imports[module]; !ok {
			imports[module] = nil
		}
		if imported.Valid {
			imports[module] = append(imports[module], imported.String)
		}
	}
	return imports, rows.Err()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
