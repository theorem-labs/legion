package ledger

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// The tables that OpenExisting requires (schemaTables) are those the embedded migrations leave: a migration that
// adds, drops or renames a table must change the list too.
func TestSchemaTablesAreTheMigratedTables(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testpg.Database(t, "ledger_schema_tables"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.pool.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'schema_versions'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(schemaTables)); !slices.Equal(got, want) {
		t.Fatalf("the migrated ledger has tables %v; schemaTables lists %v", got, want)
	}
}
