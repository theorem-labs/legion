package declindex

import (
	"context"
	"database/sql"
)

// LandSnapshot is the index state land-hash needs to answer one candidate tree. Its maps are read in one SQLite
// transaction (the store's snapshot, or one of their own), so a concurrent refresh cannot combine one commit's
// sources with another commit's declarations.
type LandSnapshot struct {
	HasSources bool
	// Prefix is the indexed tree's path below the root of its repository, with a trailing slash, empty at that
	// root: the index keys sources and declaration paths relative to the tree.
	Prefix  string
	Sources map[string]string
	// Modules are the source modules that the index read, each by its source path without the .lean suffix.
	Modules      map[string]bool
	Declarations map[string][]LandDeclaration
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
	tx := s.q
	if s.snapshot == nil {
		own, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return LandSnapshot{}, err
		}
		defer func() { _ = own.Rollback() }()
		tx = own
	}
	snapshot := LandSnapshot{
		Modules:      map[string]bool{},
		Declarations: map[string][]LandDeclaration{},
	}
	sourceKind, err := readMeta(ctx, tx, "sources")
	if err != nil {
		return LandSnapshot{}, err
	}
	snapshot.HasSources = sourceKind == "git"
	if snapshot.Prefix, err = readMeta(ctx, tx, "source_prefix"); err != nil {
		return LandSnapshot{}, err
	}
	if snapshot.Sources, err = readSourceBlobs(ctx, tx); err != nil {
		return LandSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT rel FROM modules`)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var rel string
		if err := r.Scan(&rel); err != nil {
			return err
		}
		snapshot.Modules[rel] = true
		return nil
	})
	if err != nil {
		return LandSnapshot{}, err
	}
	err = eachRowIn(ctx, tx, foundSelect+` WHERE d.name IN`, names, func(r *sql.Rows) error {
		f, err := scanFound(r)
		if err != nil {
			return err
		}
		snapshot.Declarations[f.Name] = append(snapshot.Declarations[f.Name],
			LandDeclaration{Name: f.Name, Module: f.Module, Path: f.Path, StmtHash: f.StmtHash})
		return nil
	})
	if err != nil {
		return LandSnapshot{}, err
	}
	return snapshot, nil
}
