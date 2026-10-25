//go:build ledgertesthold

package ledger

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
)

// HoldEnv names the point at which a ledger built with the ledgertesthold tag holds its work, so an end-to-end test can
// kill the process there. Only test builds carry the tag; a production build ignores the variable.
const HoldEnv = "LEDGER_TEST_HOLD"

// holdTransaction sleeps inside tx, after every write of the call and before its commit, when HoldEnv names point. The
// session checks its client every 100ms while it sleeps, so a killed process's transaction ends at once.
func holdTransaction(ctx context.Context, tx pgx.Tx, point string) error {
	if os.Getenv(HoldEnv) != point {
		return nil
	}
	if _, err := tx.Exec(ctx, `SET LOCAL client_connection_check_interval = '100ms'`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_sleep(600)`)
	return err
}
