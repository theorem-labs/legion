package ledger

import (
	"slices"
	"testing"
)

// The tables that OpenExisting requires are those the migrations leave: created, less those a later migration
// drops, whatever the statements' case, indentation or IF [NOT] EXISTS.
func TestMigrationTablesFollowCreateAndDrop(t *testing.T) {
	got := migrationTables(
		"CREATE TABLE repositories (\n  name text\n);\n-- CREATE TABLE commented (x int);\ncreate table if not exists Events (x int);\n",
		"  CREATE TABLE scratch (x int);\nALTER TABLE events ADD COLUMN y int;\n",
		"DROP TABLE IF EXISTS scratch;\nCREATE TABLE contracts (x int);\n",
	)
	want := []string{"repositories", "events", "contracts"}
	if !slices.Equal(got, want) {
		t.Fatalf("migration tables %v, want %v", got, want)
	}
}

// The embedded schema yields every table it creates: here, two that the ledger's own queries use.
func TestSchemaTablesReadTheEmbeddedMigrations(t *testing.T) {
	tables, err := schemaTables()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"contracts", "events"} {
		if !slices.Contains(tables, table) {
			t.Errorf("schema tables %v lack %s", tables, table)
		}
	}
}
