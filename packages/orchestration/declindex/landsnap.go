package declindex

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LandSnapshot is the index state land-hash needs to answer one candidate tree. Its maps are read through the
// store's reads: a store opened read-only (OpenReadOnly) reads them from its one snapshot, so a concurrent refresh
// cannot combine one commit's sources with another commit's declarations.
type LandSnapshot struct {
	HasSources bool
	// Prefix is the indexed tree's path below the root of its repository, with a trailing slash, empty at that
	// root: the index keys sources and declaration paths relative to the tree.
	Prefix  string
	Sources map[string]string
	// Modules are the source modules that the index read, each by its source path without the .lean suffix.
	Modules map[string]bool
	// Declarations are the declarations of each requested name.
	Declarations map[string][]Found
}

// LandSnapshot reads source blobs, modules, and declarations with names in names.
func (s *Store) LandSnapshot(ctx context.Context, names []string) (LandSnapshot, error) {
	snapshot := LandSnapshot{Modules: map[string]bool{}}
	sourceKind, err := s.Meta(ctx, "sources")
	if err != nil {
		return LandSnapshot{}, err
	}
	snapshot.HasSources = sourceKind == "git"
	if snapshot.Prefix, err = s.Meta(ctx, "source_prefix"); err != nil {
		return LandSnapshot{}, err
	}
	if snapshot.Sources, err = s.sourceBlobs(ctx); err != nil {
		return LandSnapshot{}, err
	}
	rows, err := s.q.Query(ctx, `SELECT rel FROM modules`)
	err = eachRow(rows, err, func(r pgx.Rows) error {
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
	if snapshot.Declarations, err = s.declarationsNamed(ctx, names); err != nil {
		return LandSnapshot{}, err
	}
	return snapshot, nil
}
