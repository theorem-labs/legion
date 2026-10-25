//go:build !ledgertesthold

package main

import (
	"testing"
	"time"
)

// A ledger built without the ledgertesthold tag, as every production build is, ignores LEDGER_TEST_HOLD: an effect
// command with each hold point set finishes at once and commits.
func TestProductionBuildIgnoresTheTestHold(t *testing.T) {
	for _, point := range []string{"inside", "after-commit"} {
		t.Run(point, func(t *testing.T) {
			cli := newEffectCLI(t, cliHookKey)
			t.Setenv("LEDGER_TEST_HOLD", point)
			finished := make(chan int, 1)
			go func() {
				code, _, _ := cli.call("effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 0}))
				finished <- code
			}()
			select {
			case code := <-finished:
				if code != 0 {
					t.Fatalf("effect-done with LEDGER_TEST_HOLD=%s exited %d", point, code)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("effect-done with LEDGER_TEST_HOLD=%s held a production build", point)
			}
			if done := cli.effect("batch-1", cliHookKey); done.State != "done" {
				t.Fatalf("after effect-done the effect is %+v", done)
			}
		})
	}
}
