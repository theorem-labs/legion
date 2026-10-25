//go:build ledgertesthold

package main

import (
	"os"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// holdAfterCommit sleeps after a command's transaction has committed and before it prints, when ledger.HoldEnv is
// after-commit, so an end-to-end test can kill the process right after the commit. Only test builds carry the
// ledgertesthold tag.
func holdAfterCommit() {
	if os.Getenv(ledger.HoldEnv) == "after-commit" {
		time.Sleep(10 * time.Minute)
	}
}
