package ledger_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

// TestRoundKeyBodyMigrationRequiresEmptyTable catches schema 0005 inventing bodies for old round keys, or adding
// nullable body columns that let a key compare against missing data.
func TestRoundKeyBodyMigrationRequiresEmptyTable(t *testing.T) {
	ctx := context.Background()
	applyThroughRoundKeys := func(t *testing.T) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, testpg.Database(t, "ledger_round_keys_schema"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		for _, path := range []string{
			"schema/0001_init.sql",
			"schema/0002_work_item_owner.sql",
			"schema/0003_ready_upstream_rounds.sql",
			"schema/0004_contract_last_change.sql",
		} {
			schema, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if _, err := conn.Exec(ctx, string(schema)); err != nil {
				t.Fatalf("apply %s: %v", path, err)
			}
		}
		return conn
	}
	bodySchema, err := os.ReadFile("schema/0005_round_keys_body.sql")
	if err != nil {
		t.Fatalf("read schema 0005: %v", err)
	}

	t.Run("existing key", func(t *testing.T) {
		conn := applyThroughRoundKeys(t)
		for _, statement := range []string{
			`INSERT INTO repositories (name, max_landing_rounds) VALUES ('r', 3)`,
			`INSERT INTO migrations (repo, id, owner) VALUES ('r', 'M-1', 'alice')`,
			`INSERT INTO round_keys (repo, migration, key) VALUES ('r', 'M-1', 'old-round')`,
		} {
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		_, err := conn.Exec(ctx, string(bodySchema))
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" ||
			!strings.Contains(databaseError.Message, "round_keys already contains rows") {
			t.Fatalf("schema 0005 with an existing round key: %v", err)
		}
	})

	t.Run("empty table", func(t *testing.T) {
		conn := applyThroughRoundKeys(t)
		if _, err := conn.Exec(ctx, string(bodySchema)); err != nil {
			t.Fatalf("apply schema 0005: %v", err)
		}
		rows, err := conn.Query(ctx, `SELECT column_name, data_type, is_nullable, column_default IS NULL
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'round_keys' AND column_name IN ('generation', 'reason', 'batch')
			ORDER BY column_name`)
		if err != nil {
			t.Fatal(err)
		}
		type column struct {
			name, dataType, nullable string
			noDefault                bool
		}
		columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
			var c column
			err := row.Scan(&c.name, &c.dataType, &c.nullable, &c.noDefault)
			return c, err
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []column{
			{name: "batch", dataType: "text", nullable: "NO", noDefault: true},
			{name: "generation", dataType: "bigint", nullable: "NO", noDefault: true},
			{name: "reason", dataType: "text", nullable: "NO", noDefault: true},
		}
		if len(columns) != len(want) {
			t.Fatalf("round key body columns: got %+v, want %+v", columns, want)
		}
		for i := range want {
			if columns[i] != want[i] {
				t.Fatalf("round key body columns: got %+v, want %+v", columns, want)
			}
		}
	})
}
