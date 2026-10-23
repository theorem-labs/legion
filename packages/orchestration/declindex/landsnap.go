package declindex

import (
	"context"
	"database/sql"
	"errors"
)

// LandSnapshot is the index state land-hash needs to answer one candidate tree. Its maps are read in one SQLite
// transaction, so a concurrent refresh cannot combine one commit's sources with another commit's declarations.
type LandSnapshot struct {
	HasSources   bool
	Sources      map[string]string
	Modules      map[string]LandModule
	Declarations map[string][]LandDeclaration
}

// LandModule is a source module that the index read, keyed by its source path without the .lean suffix.
type LandModule struct {
	Rel    string
	Module string
}

// LandDeclaration is one exact requested declaration from the snapshot.
type LandDeclaration struct {
	Name     string
	Module   string
	Path     string
	StmtHash string
}

// LandSnapshot reads source blobs, modules, and declarations with names in names from one read transaction.
func (s *Store) LandSnapshot(ctx context.Context, names []string) (LandSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LandSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := LandSnapshot{
		Sources:      map[string]string{},
		Modules:      map[string]LandModule{},
		Declarations: map[string][]LandDeclaration{},
	}
	var sourceKind string
	err = tx.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = 'sources'`).Scan(&sourceKind)
	if err == nil {
		snapshot.HasSources = sourceKind == "git"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return LandSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT path, blob FROM sources`)
	if err != nil {
		return LandSnapshot{}, err
	}
	for rows.Next() {
		var path, blob string
		if err := rows.Scan(&path, &blob); err != nil {
			rows.Close()
			return LandSnapshot{}, err
		}
		snapshot.Sources[path] = blob
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return LandSnapshot{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT rel, module FROM modules`)
	if err != nil {
		return LandSnapshot{}, err
	}
	for rows.Next() {
		var module LandModule
		if err := rows.Scan(&module.Rel, &module.Module); err != nil {
			rows.Close()
			return LandSnapshot{}, err
		}
		snapshot.Modules[module.Rel] = module
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return LandSnapshot{}, err
	}
	for i := 0; i < len(names); i += 500 {
		chunk := names[i:min(i+500, len(names))]
		rows, err = tx.QueryContext(ctx, `SELECT d.name, d.module, d.stmt_hash, COALESCE(m.rel, '')
			FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.name IN (`+placeholders(len(chunk))+`)`, anys(chunk)...)
		if err != nil {
			return LandSnapshot{}, err
		}
		for rows.Next() {
			var declaration LandDeclaration
			var rel string
			if err := rows.Scan(&declaration.Name, &declaration.Module, &declaration.StmtHash, &rel); err != nil {
				rows.Close()
				return LandSnapshot{}, err
			}
			declaration.Path = sourcePath(rel, declaration.Module)
			snapshot.Declarations[declaration.Name] = append(snapshot.Declarations[declaration.Name], declaration)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return LandSnapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return LandSnapshot{}, err
	}
	return snapshot, nil
}
