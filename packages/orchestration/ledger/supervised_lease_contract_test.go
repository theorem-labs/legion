package ledger_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

// TestClaimAllowsSupervisedLeaseAndRenewRefusesIt catches a zero-length claim being treated as an
// invalid timed lease, or renewed despite having no end.
func TestClaimAllowsSupervisedLeaseAndRenewRefusesIt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	lease, err := s.Claim(ctx, "r", "item", "supervisor:worker", 0)
	if err != nil {
		t.Fatalf("claim a supervised lease: %v", err)
	}
	if lease.ExpiresAt != nil {
		t.Fatalf("supervised lease expires at %s, want no expiry", lease.ExpiresAt)
	}
	if item := must(s.Item(ctx, "r", "item")); item.ExpiresAt != nil {
		t.Fatalf("supervised item expires at %s, want no expiry", item.ExpiresAt)
	}

	_, err = s.Renew(ctx, "r", "item", "supervisor:worker", lease.Generation, time.Minute)
	wantRefusal(t, err, ledger.ReasonSupervisedLease)
}

// TestOwnerMigrationRejectsLeaseEndWithoutHolder catches migration 0002 accidentally dropping the
// invariant that every fixed lease end has a holder.
func TestOwnerMigrationRejectsLeaseEndWithoutHolder(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "ledger_schema")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	for _, path := range []string{"schema/0001_init.sql", "schema/0002_work_item_owner.sql"} {
		schema, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := conn.Exec(ctx, string(schema)); err != nil {
			t.Fatalf("apply %s: %v", path, err)
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO repositories (name, max_landing_rounds) VALUES ($1, 0)", "r"); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, "INSERT INTO work_items (repo, id, lease_expires_at) VALUES ($1, $2, clock_timestamp())", "r", "bad")
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
		t.Fatalf("lease end without holder error = %v, want check violation", err)
	}
}
