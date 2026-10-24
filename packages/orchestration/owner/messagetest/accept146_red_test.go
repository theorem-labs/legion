package messagetest

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// Two message tests started in the same second must each get a fresh ledger repository and a daemon of their own
// (the plan's end-to-end row for slice 1: "A fresh ledger repository and a separate daemon instance for each run").
// At 059e0e3b the run's name is message-test-<UTC stamp to the second>, and it names the ledger repository, the
// daemon's project, its tmux socket and the run's private bearer directory, so two runs started in one second share
// all four. Acceptance 2026-10-24 (Test146): two runs started 72 µs apart were both message-test-20261024t050420z,
// and the ledger held one repository for both.
//
// Each run here stops in setup (its graph directory is empty), before any ledger, daemon or tmux is used.
func TestRunsStartedInTheSameSecondGetTheirOwnLedgerRepository(t *testing.T) {
	run := func() Report {
		report, err := Run(t.Context(), Options{
			Nodes: []string{"function:c@x/y.c", "struct:t"}, Kills: 3, Out: filepath.Join(t.TempDir(), "run"),
			Deploy: ownerpkg.Deploy{GraphDir: t.TempDir(), LedgerDSNFile: filepath.Join(t.TempDir(), "ledger-dsn")},
			Owner:  "/opt/bin/owner", Legion: "/opt/bin/legion",
			Getenv: func(string) string { return "" }, Progress: io.Discard, Timeouts: Timeouts{Poll: time.Millisecond},
		})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	for range 20 {
		first, second := run(), run()
		if !first.StartedAt.Truncate(time.Second).Equal(second.StartedAt.Truncate(time.Second)) {
			continue // the two straddled a second; try again
		}
		if first.Run == second.Run {
			t.Fatalf("two runs started in the same second (%s, %s) share the run %q: one ledger repository, one daemon "+
				"project and the tmux socket %q", first.StartedAt.Format(time.RFC3339Nano), second.StartedAt.Format(time.RFC3339Nano),
				first.Run, DaemonSocket(first.Run))
		}
		if DaemonSocket(first.Run) == DaemonSocket(second.Run) {
			t.Fatalf("runs %q and %q share the tmux socket %q", first.Run, second.Run, DaemonSocket(first.Run))
		}
		return
	}
	t.Fatal("no two runs started in the same second in 20 tries")
}
