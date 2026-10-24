package declindex

import (
	"context"
	"database/sql"
	"errors"
)

// LandSnapshot is the index state land-hash needs to answer one candidate tree. Its maps are read in one SQLite
// transaction, so a concurrent refresh cannot combine one commit's sources with another commit's declarations.
type LandSnapshot struct {
	HasSources bool
	// Prefix is the indexed tree's path below the root of its repository, with a trailing slash, empty at that
	// root: the index keys sources and declaration paths relative to the tree.
	Prefix       string
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
	meta := func(k string) (string, error) {
		var v string
		err := tx.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	sourceKind, err := meta("sources")
	if err != nil {
		return LandSnapshot{}, err
	}
	snapshot.HasSources = sourceKind == "git"
	if snapshot.Prefix, err = meta("source_prefix"); err != nil {
		return LandSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT path, blob FROM sources`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var path, blob string
		if err := r.Scan(&path, &blob); err != nil {
			return err
		}
		snapshot.Sources[path] = blob
		return nil
	})
	if err != nil {
		return LandSnapshot{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT rel, module FROM modules`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var module LandModule
		if err := r.Scan(&module.Rel, &module.Module); err != nil {
			return err
		}
		snapshot.Modules[module.Rel] = module
		return nil
	})
	if err != nil {
		return LandSnapshot{}, err
	}
	for i := 0; i < len(names); i += 500 {
		chunk := names[i:min(i+500, len(names))]
		rows, err = tx.QueryContext(ctx, `SELECT d.name, d.module, d.stmt_hash, COALESCE(m.rel, '')
			FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.name IN (`+placeholders(len(chunk))+`)`, anys(chunk)...)
		err = eachRow(rows, err, func(r *sql.Rows) error {
			var declaration LandDeclaration
			var rel string
			if err := r.Scan(&declaration.Name, &declaration.Module, &declaration.StmtHash, &rel); err != nil {
				return err
			}
			declaration.Path = sourcePath(rel, declaration.Module)
			snapshot.Declarations[declaration.Name] = append(snapshot.Declarations[declaration.Name], declaration)
			return nil
		})
		if err != nil {
			return LandSnapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return LandSnapshot{}, err
	}
	return snapshot, nil
}
