//go:build !ledgertesthold

package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// holdTransaction does nothing outside a build with the ledgertesthold tag; see hold_on.go.
func holdTransaction(context.Context, pgx.Tx, string) error { return nil }
