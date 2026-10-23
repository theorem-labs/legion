package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestOpenExistingRequiresEveryEmbeddedSchemaVersion(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_existing")

	if got, err := ledger.OpenExisting(ctx, dsn); got != nil {
		got.Close()
		t.Fatal("OpenExisting returned a store for an empty database")
	} else {
		var schemaErr *ledger.SchemaError
		if !errors.As(err, &schemaErr) || len(schemaErr.Missing) == 0 {
			t.Fatalf("OpenExisting error %v, want SchemaError with missing versions", err)
		}
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var contracts *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('contracts')").Scan(&contracts); err != nil {
		t.Fatal(err)
	}
	if contracts != nil {
		t.Fatalf("OpenExisting created contracts table %q", *contracts)
	}

	opened, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	existing, err := ledger.OpenExisting(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenExisting after Open: %v", err)
	}
	existing.Close()

	if _, err := conn.Exec(ctx, "DELETE FROM schema_versions WHERE version = (SELECT max(version) FROM schema_versions)"); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.OpenExisting(ctx, dsn); got != nil {
		got.Close()
		t.Fatal("OpenExisting accepted a database missing its newest schema version")
	} else {
		var schemaErr *ledger.SchemaError
		if !errors.As(err, &schemaErr) || len(schemaErr.Missing) == 0 {
			t.Fatalf("OpenExisting error %v, want SchemaError with missing versions", err)
		}
	}
	var rows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM schema_versions").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("OpenExisting restored %d schema-version rows", rows)
	}
}

func TestOpenExistingRejectsAnIncompleteLedgerSchema(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_missing_table")
	opened, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP TABLE contracts CASCADE"); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.OpenExisting(ctx, dsn); got != nil {
		got.Close()
		t.Fatal("OpenExisting accepted a database with a missing ledger table")
	} else {
		var schemaErr *ledger.SchemaError
		if !errors.As(err, &schemaErr) || len(schemaErr.Missing) == 0 {
			t.Fatalf("OpenExisting error %v, want SchemaError with missing versions", err)
		}
	}
	var contracts *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('contracts')").Scan(&contracts); err != nil {
		t.Fatal(err)
	}
	if contracts != nil {
		t.Fatalf("OpenExisting recreated contracts table %q", *contracts)
	}
}
