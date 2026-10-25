package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// TestOpenRefusesASchemaVersionAppliedFromAnotherFile catches Open skipping a version whose recorded file is not the
// binary's: a database migrated by an earlier head has a version 3 with other tables, and the ledger then failed
// later, with exit 4, on a missing column. Open now names both files and says to recreate the database.
func TestOpenRefusesASchemaVersionAppliedFromAnotherFile(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_schema_names")
	s, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE schema_versions SET name = '0003_ready_upstream_rounds.sql' WHERE version = 3`); err != nil {
		t.Fatal(err)
	}

	s, err = ledger.Open(ctx, dsn)
	if err == nil {
		s.Close()
		t.Fatal("Open accepted a version 3 that another schema file applied")
	}
	for _, want := range []string{"0003_ready_upstream_rounds.sql", "0003_ready_rounds_refusals.sql", "recreate the database"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Open error %q does not name %q", err, want)
		}
	}

	// Control: the database as this binary migrated it opens again.
	if _, err := conn.Exec(ctx, `UPDATE schema_versions SET name = '0003_ready_rounds_refusals.sql' WHERE version = 3`); err != nil {
		t.Fatal(err)
	}
	s, err = ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open of a database this binary migrated: %v", err)
	}
	s.Close()
}
